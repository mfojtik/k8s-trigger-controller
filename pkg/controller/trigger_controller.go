package controller

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/util/sets"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const (
	controllerAgentName = "hash-controller"
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

// TriggerController is the controller implementation for Foo resources
type TriggerController struct {
	client kubernetes.Interface

	configMapsLister v1.ConfigMapLister
	configMapsSynced cache.InformerSynced

	secretsLister v1.SecretLister
	secretsSynced cache.InformerSynced

	deploymentClient  appsv1client.DeploymentsGetter
	deploymentsSynced cache.InformerSynced

	deploymentsIndex cache.Indexer

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder
}

// NewTriggerController returns a new trigger controller
func NewTriggerController(
	kubeclientset kubernetes.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory) *TriggerController {

	configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
	secretInformer := kubeInformerFactory.Core().V1().Secrets()
	deploymentInformer := kubeInformerFactory.Apps().V1().Deployments()

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &TriggerController{
		client:           kubeclientset,
		deploymentClient: kubeclientset.AppsV1(),
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "data-version"),
		recorder:         recorder,
		secretsLister:    secretInformer.Lister(),
		configMapsLister: configMapInformer.Lister(),
	}

	controller.configMapsSynced = configMapInformer.Informer().HasSynced
	controller.secretsSynced = secretInformer.Informer().HasSynced
	controller.deploymentsSynced = deploymentInformer.Informer().HasSynced

	deploymentInformer.Informer().AddIndexers(cache.Indexers{
		"configMap": indexDeploymentsByTriggeringConfigMaps,
		"secret":    indexDeploymentsByTriggeringSecrets,
	})
	controller.deploymentsIndex = deploymentInformer.Informer().GetIndexer()

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

func indexDeploymentsByTriggeringConfigMaps(obj interface{}) ([]string, error) {
	var configs []string
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return configs, fmt.Errorf("object is not deployment")
	}
	annotations := d.GetAnnotations()
	if len(annotations) == 0 {
		return configs, nil
	}
	if triggers, ok := annotations[triggerConfigMapsAnnotation]; ok {
		configMaps := sets.NewString(strings.Split(triggers, ",")...)
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.ConfigMap != nil {
				if configMaps.Has(v.ConfigMap.Name) {
					configs = append(configs, v.ConfigMap.Name)
				}
			}
		}
	}
	return configs, nil
}

func indexDeploymentsByTriggeringSecrets(obj interface{}) ([]string, error) {
	var configs []string
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return configs, fmt.Errorf("object is not deployment")
	}
	annotations := d.GetAnnotations()
	if len(annotations) == 0 {
		return configs, nil
	}
	if triggers, ok := annotations[triggerSecretsAnnotation]; ok {
		secrets := sets.NewString(strings.Split(triggers, ",")...)
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Secret != nil {
				if secrets.Has(v.Secret.SecretName) {
					configs = append(configs, v.Secret.SecretName)
				}
			}
		}
	}
	return configs, nil
}

func (c *TriggerController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtimeutil.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting trigger controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync ...")
	if ok := cache.WaitForCacheSync(stopCh, c.configMapsSynced, c.secretsSynced, c.deploymentsSynced); !ok {
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

// calculateDataHash calculates a hash from the map[string]string
// TODO: This might be inefficient, there should be a better way to get a checksum for the current data.
func calculateDataHash(obj interface{}) string {
	var sortedMapKeys []string
	hash := sha1.New()
	switch t := obj.(type) {
	case *corev1.Secret:
		for k := range t.Data {
			sortedMapKeys = append(sortedMapKeys, k)
		}
		sort.Strings(sortedMapKeys)
		hash.Write([]byte(strings.Join(sortedMapKeys, "")))
		for _, key := range sortedMapKeys {
			hash.Write(t.Data[key])
		}
	case *corev1.ConfigMap:
		for k := range t.Data {
			sortedMapKeys = append(sortedMapKeys, k)
		}
		sort.Strings(sortedMapKeys)
		hash.Write([]byte(strings.Join(sortedMapKeys, "")))
		for _, key := range sortedMapKeys {
			hash.Write([]byte(t.Data[key]))
		}
	default:
		runtimeutil.HandleError(fmt.Errorf("unknown object: %v", obj))
	}
	return hex.EncodeToString(hash.Sum(nil))
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

	// Get all deployments that use the configMap or Secret
	toTrigger, err := c.deploymentsIndex.ByIndex(kind, objMeta.GetName())
	if err != nil {
		return err
	}

	// No triggers active for this secret/configMap
	if len(toTrigger) == 0 {
		// noisy: glog.V(5).Infof("%s %q is not triggering any deployment", kind, objMeta.GetName())
		return nil
	}

	newDataHash := calculateDataHash(obj)
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

	// Determine whether to trigger these deployments
	var triggerErrors []error
	for _, obj := range toTrigger {
		dMeta, err := meta.Accessor(obj)
		if err != nil {
			runtimeutil.HandleError(fmt.Errorf("failed to get accessor for %#v", err))
			continue
		}
		d, err := c.client.AppsV1().Deployments(dMeta.GetNamespace()).Get(dMeta.GetName(), meta_v1.GetOptions{})
		if err != nil {
			runtimeutil.HandleError(fmt.Errorf("failed to get deployment %s/%s: %v", dMeta.GetNamespace(), dMeta.GetName(), err))
		}
		glog.V(3).Infof("Processing deployment %s/%s that tracks %s %s ...", d.Namespace, d.Name, kind, objMeta.GetName())

		annotations := d.Spec.Template.Annotations
		triggerAnnotationKey := triggeredByAnnotation(kind, dMeta)
		if annotations == nil {
			annotations = map[string]string{
				triggerAnnotationKey: newDataHash,
			}
			glog.V(3).Infof("Deployment %s/%s now triggers on %s %q updates and will now rollout", d.Namespace, d.Name, kind, objMeta.GetName())
		} else {
			if hash, exists := annotations[triggerAnnotationKey]; exists && hash == newDataHash {
				glog.V(3).Infof("Deployment %s/%s already have latest %s %q", d.Namespace, d.Name, kind, objMeta.GetName())
				continue
			}
			glog.V(3).Infof("Deployment %s/%s has old %s %q and will rollout", d.Namespace, d.Name, kind, objMeta.GetName())
			annotations[triggerAnnotationKey] = newDataHash
		}

		dCopy := d.DeepCopy()
		dCopy.Spec.Template.Annotations = annotations
		if _, err := c.client.AppsV1().Deployments(d.Namespace).Update(dCopy); err != nil {
			glog.Errorf("Failed to update deployment %s/%s: %v", d.Namespace, d.Name, err)
			triggerErrors = append(triggerErrors, err)
		}
	}

	if len(triggerErrors) != 0 {
		return errorsutil.NewAggregate(triggerErrors)
	}

	return nil
}

func triggeredByAnnotation(kind string, objMeta meta_v1.Object) string {
	return fmt.Sprintf("trigger.k8s.io/%s-%s-last-hash", kind, objMeta.GetName())
}
