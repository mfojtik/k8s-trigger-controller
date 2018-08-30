package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const (
	controllerAgentName = "trigger-controller"
	configMapPrefix     = "configMap#"
	secretPrefix        = "secret#"

	dataHashAnnotation = "trigger.k8s.io/data-hash"

	// triggerSecretsAnnotation is a deployment annotation that contains a comma separated list of secret names that this deployment should
	// be automatically triggered when the content of those secrets is changed.
	triggerSecretsAnnotation = "trigger.k8s.io/triggering-secrets"

	// triggerConfigMapsAnnotation is a deployment annotation that contains a comma separated list of configMaps names that this deployment should
	// be automatically triggered when the content of those configMaps is changed.
	triggerConfigMapsAnnotation = "trigger.k8s.io/triggering-configMaps"
)

var (
	// lastHashAnnotation is a deployment replicaset template annotation that contains last SHA256 hash of the secret or configMap
	lastHashAnnotation = func(kind, name string) string {
		return fmt.Sprintf("trigger.k8s.io/%s-%s-last-hash", kind, name)
	}
)

// TriggerController is the controller implementation for Foo resources
type TriggerController struct {
	client kubernetes.Interface

	configMapsLister v1.ConfigMapLister
	configMapsSynced cache.InformerSynced

	secretsLister v1.SecretLister
	secretsSynced cache.InformerSynced

	deploymentsSynced  cache.InformerSynced
	statefulSetsSynced cache.InformerSynced

	deploymentsIndex  cache.Indexer
	statefulSetsIndex cache.Indexer

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder

	calculateDataHashFn func(interface{}) string
}

// NewTriggerController returns a new trigger controller
func NewTriggerController(
	kubeclientset kubernetes.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory) *TriggerController {

	configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
	secretInformer := kubeInformerFactory.Core().V1().Secrets()
	deploymentInformer := kubeInformerFactory.Apps().V1().Deployments()
	statefulSetInformer := kubeInformerFactory.Apps().V1().StatefulSets()

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &TriggerController{
		client:              kubeclientset,
		workqueue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "data-version"),
		recorder:            recorder,
		secretsLister:       secretInformer.Lister(),
		configMapsLister:    configMapInformer.Lister(),
		calculateDataHashFn: calculateDataHash,
	}

	controller.configMapsSynced = configMapInformer.Informer().HasSynced
	controller.secretsSynced = secretInformer.Informer().HasSynced
	controller.deploymentsSynced = deploymentInformer.Informer().HasSynced
	controller.statefulSetsSynced = statefulSetInformer.Informer().HasSynced

	deploymentInformer.Informer().AddIndexers(cache.Indexers{
		"configMap": indexByConfigMaps,
		"secret":    indexBySecrets,
	})
	controller.deploymentsIndex = deploymentInformer.Informer().GetIndexer()

	statefulSetInformer.Informer().AddIndexers(cache.Indexers{
		"configMap": indexByConfigMaps,
		"secret":    indexBySecrets,
	})
	controller.statefulSetsIndex = statefulSetInformer.Informer().GetIndexer()

	configMapInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(new interface{}) {
			controller.enqueueConfigMap(nil, new)
		},
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueConfigMap(old, new)
		},
	})

	secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(new interface{}) {
			controller.enqueueSecret(nil, new)
		},
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueSecret(old, new)
		},
	})

	return controller
}

func indexByConfigMaps(obj interface{}) ([]string, error) {
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return []string{}, nil
	}

	annotations := objMeta.GetAnnotations()
	if triggers, ok := annotations[triggerConfigMapsAnnotation]; ok {
		return sets.NewString(strings.Split(triggers, ",")...).List(), nil
	}
	return []string{}, nil
}

func indexBySecrets(obj interface{}) ([]string, error) {
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return []string{}, nil
	}

	annotations := objMeta.GetAnnotations()
	if triggers, ok := annotations[triggerSecretsAnnotation]; ok {
		return sets.NewString(strings.Split(triggers, ",")...).List(), nil
	}
	return []string{}, nil
}

func (c *TriggerController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtimeutil.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting trigger controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync ...")
	if ok := cache.WaitForCacheSync(stopCh, c.configMapsSynced, c.secretsSynced, c.deploymentsSynced, c.statefulSetsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers ...")
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers ...")
	return nil
}

func (c *TriggerController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *TriggerController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var (
			key string
			ok  bool
		)
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			runtimeutil.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if syncErr := c.syncHandler(key); syncErr != nil {
			return fmt.Errorf("error syncing '%s': %s", key, syncErr.Error())
		}
		c.workqueue.Forget(obj)
		return nil
	}(obj)
	if err != nil {
		runtimeutil.HandleError(err)
	}
	return true
}

func (c *TriggerController) enqueueObject(prefix string, old, new interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(new)
	if err != nil {
		runtimeutil.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(prefix + key)
}

func (c *TriggerController) enqueueConfigMap(old, new interface{}) {
	c.enqueueObject(configMapPrefix, old, new)
}

func (c *TriggerController) enqueueSecret(old, new interface{}) {
	c.enqueueObject(secretPrefix, old, new)
}

func (c *TriggerController) syncHandler(key string) error {
	parts := strings.Split(key, "#")
	if len(parts) != 2 {
		runtimeutil.HandleError(fmt.Errorf("unexpected resource key: %s", key))
		return nil
	}
	kind := parts[0]

	namespace, name, err := cache.SplitMetaNamespaceKey(parts[1])
	if err != nil {
		runtimeutil.HandleError(fmt.Errorf("invalid resource key: %s", parts[1]))
		return nil
	}

	var (
		obj interface{}
	)

	switch kind {
	case "configMap":
		obj, err = c.configMapsLister.ConfigMaps(namespace).Get(name)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
	case "secret":
		obj, err = c.secretsLister.Secrets(namespace).Get(name)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
	default:
		runtimeutil.HandleError(fmt.Errorf("invalid resource kind, only configMap and secret are allowed, got: %s", kind))
		return nil
	}

	// First update the data hashes into ConfigMap/Secret annotations.
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		runtimeutil.HandleError(err)
	}

	newDataHash := c.calculateDataHashFn(obj)
	// The secret/configMap is empty
	// TODO: Should this trigger?
	if len(newDataHash) == 0 {
		return nil
	}
	oldDataHash := objMeta.GetAnnotations()[dataHashAnnotation]

	if newDataHash != oldDataHash {
		switch kind {
		case "configMap":
			objCopy := obj.(*corev1.ConfigMap).DeepCopy()
			if objCopy.Annotations == nil {
				objCopy.Annotations = map[string]string{}
			}
			objCopy.Annotations[dataHashAnnotation] = newDataHash
			glog.V(3).Infof("Updating configMap %s/%s with new data hash: %v", objCopy.Namespace, objCopy.Name, newDataHash)
			if _, err := c.client.CoreV1().ConfigMaps(objCopy.Namespace).Update(objCopy); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		case "secret":
			objCopy := obj.(*corev1.Secret).DeepCopy()
			if objCopy.Annotations == nil {
				objCopy.Annotations = map[string]string{}
			}
			objCopy.Annotations[dataHashAnnotation] = newDataHash
			glog.V(3).Infof("Updating secret %s/%s with new data hash: %v", objCopy.Namespace, objCopy.Name, newDataHash)
			if _, err := c.client.CoreV1().Secrets(objCopy.Namespace).Update(objCopy); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		}
	} else {
		glog.V(5).Infof("No change detected in hash for %s %s/%s", kind, objMeta.GetNamespace(), objMeta.GetName())
	}

	// Get all deployments that use the configMap or Secret
	triggeredDeploys, err := c.deploymentsIndex.ByIndex(kind, objMeta.GetName())
	if err != nil {
		return err
	}

	triggeredStatefulSets, err := c.statefulSetsIndex.ByIndex(kind, objMeta.GetName())
	if err != nil {
		return err
	}

	triggerErrors := []error{}
	triggerErrors = append(triggerErrors, c.triggerDeployments(triggeredDeploys, kind, objMeta.GetName(), newDataHash)...)
	triggerErrors = append(triggerErrors, c.triggerStatefulSets(triggeredStatefulSets, kind, objMeta.GetName(), newDataHash)...)

	if len(triggerErrors) != 0 {
		return errorsutil.NewAggregate(triggerErrors)
	}

	return nil
}

func (c *TriggerController) triggerDeployments(toTrigger []interface{}, kind string, name string, newDataHash string) []error {
	var triggerErrors []error
	for _, obj := range toTrigger {
		rMeta, err := meta.Accessor(obj)
		if err != nil {
			runtimeutil.HandleError(fmt.Errorf("failed to get accessor for %#v", err))
			continue
		}
		res, err := c.client.AppsV1().Deployments(rMeta.GetNamespace()).Get(rMeta.GetName(), meta_v1.GetOptions{})
		if err != nil {
			runtimeutil.HandleError(fmt.Errorf("failed to get deployment %s/%s: %v", rMeta.GetNamespace(), rMeta.GetName(), err))
		}
		glog.V(3).Infof("Processing deployment %s/%s that tracks %s %s ...", res.Namespace, res.Name, kind, name)

		annotations := res.Spec.Template.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		triggerAnnotationKey := lastHashAnnotation(kind, name)
		if hash, exists := annotations[triggerAnnotationKey]; exists && hash == newDataHash {
			glog.V(3).Infof("Deployment %s/%s already have latest %s %q", res.Namespace, res.Name, kind, name)
			continue
		}

		glog.V(3).Infof("Deployment %s/%s has old %s %q and will rollout", res.Namespace, res.Name, kind, name)
		annotations[triggerAnnotationKey] = newDataHash

		resCopy := res.DeepCopy()
		resCopy.Spec.Template.Annotations = annotations
		if _, err := c.client.AppsV1().Deployments(res.Namespace).Update(resCopy); err != nil {
			glog.Errorf("Failed to update deployment %s/%s: %v", res.Namespace, res.Name, err)
			triggerErrors = append(triggerErrors, err)
		}
	}
	return triggerErrors
}

func (c *TriggerController) triggerStatefulSets(toTrigger []interface{}, kind string, name string, newDataHash string) []error {
	var triggerErrors []error
	for _, obj := range toTrigger {
		rMeta, err := meta.Accessor(obj)
		if err != nil {
			runtimeutil.HandleError(fmt.Errorf("failed to get accessor for %#v", err))
			continue
		}
		res, err := c.client.AppsV1().StatefulSets(rMeta.GetNamespace()).Get(rMeta.GetName(), meta_v1.GetOptions{})
		if err != nil {
			runtimeutil.HandleError(fmt.Errorf("failed to get statefulset %s/%s: %v", rMeta.GetNamespace(), rMeta.GetName(), err))
		}
		glog.V(3).Infof("Processing statefulset %s/%s that tracks %s %s ...", res.Namespace, res.Name, kind, name)

		annotations := res.Spec.Template.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		triggerAnnotationKey := lastHashAnnotation(kind, name)
		if hash, exists := annotations[triggerAnnotationKey]; exists && hash == newDataHash {
			glog.V(3).Infof("StatefulSet %s/%s already have latest %s %q", res.Namespace, res.Name, kind, name)
			continue
		}

		glog.V(3).Infof("StatefulSet %s/%s has old %s %q and will rollout", res.Namespace, res.Name, kind, name)
		annotations[triggerAnnotationKey] = newDataHash

		resCopy := res.DeepCopy()
		resCopy.Spec.Template.Annotations = annotations
		if _, err := c.client.AppsV1().StatefulSets(res.Namespace).Update(resCopy); err != nil {
			glog.Errorf("Failed to update statefulset %s/%s: %v", res.Namespace, res.Name, err)
			triggerErrors = append(triggerErrors, err)
		}
	}
	return triggerErrors
}
