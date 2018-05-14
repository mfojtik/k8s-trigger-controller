package controller

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	"github.com/mfojtik/k8s-trigger-controller/pkg/apis/trigger/v1alpha1"
)

const (
	controllerAgentName = "hash-controller"
	configMapPrefix     = "configMap#"
	secretPrefix        = "secret#"
)

// TriggerController is the controller implementation for Foo resources
type TriggerController struct {
	client kubernetes.Interface

	configMapsLister v1.ConfigMapLister
	configMapsSynced cache.InformerSynced

	secretsLister v1.SecretLister
	secretsSynced cache.InformerSynced

	deploymentClient appsv1client.DeploymentsGetter

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder
}

type triggerDefinition struct {
	Name  string
	State string
}

// NewTriggerController returns a new trigger controller
func NewTriggerController(
	kubeclientset kubernetes.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory) *TriggerController {

	configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
	secretInformer := kubeInformerFactory.Core().V1().Secrets()

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

func (c *TriggerController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtimeutil.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting trigger controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync ...")
	if ok := cache.WaitForCacheSync(stopCh, c.configMapsSynced, c.secretsSynced); !ok {
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
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)
	if err != nil {
		runtimeutil.HandleError(err)
	}
	return true
}

func triggeredDeployments(obj interface{}) ([]triggerDefinition, error) {
	objectMeta, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	var deployments []triggerDefinition
	if annotations := objectMeta.GetAnnotations(); annotations != nil {
		for name, value := range annotations {
			if strings.HasPrefix(name, v1alpha1.TriggerDeploymentAnnotationPrefix) {
				deploymentName := strings.TrimPrefix(name, v1alpha1.TriggerDeploymentAnnotationPrefix+".")
				parts := strings.Split(deploymentName, ".")
				if len(parts) != 2 {
					continue
				}
				d := triggerDefinition{
					Name:  parts[0] + "/" + parts[1],
					State: value,
				}
				deployments = append(deployments, d)
			}
		}
	}
	return deployments, nil
}

func (c *TriggerController) enqueueObject(prefix string, old, new interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(new)
	if err != nil {
		runtimeutil.HandleError(err)
		return
	}
	deployments, err := triggeredDeployments(new)
	if err != nil {
		runtimeutil.HandleError(err)
		return
	}
	if len(deployments) == 0 {
		return
	}
	glog.V(5).Infof("enqueuing %s%#v", prefix, new)
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

	newDataHash := calculateDataHash(obj)
	oldDataHash := objMeta.GetAnnotations()[v1alpha1.TriggerDataHashAnnotation]

	if newDataHash != oldDataHash {
		switch kind {
		case "configMap":
			objCopy := obj.(*corev1.ConfigMap).DeepCopy()
			objCopy.Annotations[v1alpha1.TriggerDataHashAnnotation] = newDataHash
			glog.V(3).Infof("Updating configMap %s/%s with new data hash: %v", objCopy.Namespace, objCopy.Name, newDataHash)
			if _, err := c.client.CoreV1().ConfigMaps(objCopy.Namespace).Update(objCopy); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		case "secret":
			objCopy := obj.(*corev1.Secret).DeepCopy()
			objCopy.Annotations[v1alpha1.TriggerDataHashAnnotation] = newDataHash
			glog.V(3).Infof("Updating secret %s/%s with new data hash: %v", objCopy.Namespace, objCopy.Name, newDataHash)
			if _, err := c.client.CoreV1().Secrets(objCopy.Namespace).Update(objCopy); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		}
	} else {
		glog.V(5).Infof("No change detected in hash for %s/%s", objMeta.GetNamespace(), objMeta.GetName())
	}

	deployments, err := triggeredDeployments(obj)
	if err != nil {
		runtimeutil.HandleError(err)
		return nil
	}

	var handleErrors []error

	handler, err := NewDeploymentHandler(c.client.AppsV1(), kind, objMeta.GetNamespace(), objMeta.GetName(), newDataHash)
	if err != nil {
		runtimeutil.HandleError(err)
		return nil
	}
	for _, d := range deployments {
		// Skip paused triggers
		if d.State != v1alpha1.TriggerEnabled {
			glog.V(3).Infof("Trigger for %s/%s is not enabled, skipping", objMeta.GetNamespace(), objMeta.GetName())
			continue
		}
		namespace, name, err := cache.SplitMetaNamespaceKey(d.Name)
		glog.V(3).Infof("Checking if deployment %s/%s hash for %s/%s is %v", namespace, name, objMeta.GetNamespace(), objMeta.GetName(), newDataHash)
		if err != nil {
			runtimeutil.HandleError(err)
			continue
		}
		if err := handler.Handle(namespace, name); err != nil {
			handleErrors = append(handleErrors, err)
			continue
		}
	}

	if len(handleErrors) != 0 {
		return errorsutil.NewAggregate(handleErrors)
	}

	return nil
}
