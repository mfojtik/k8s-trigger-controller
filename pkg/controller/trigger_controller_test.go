package controller

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type triggerCase struct {
	name string

	secret      *corev1.Secret
	configMap   *corev1.ConfigMap
	deployment  *appsv1.Deployment
	statefulSet *appsv1.StatefulSet

	expectedHash string
}

// stubTriggerControllerIndexers stubs the indexing related methods in the
// TriggerController. Because we don't go through the regular synchronization
// machinery to create these indexes. The method accounts for the conditional
// presecence of deployments and/or statefulsets in the provided testCase.
func stubTriggerControllerIndexers(testCase triggerCase, controller *TriggerController) {
	controller.secretsSynced = func() bool { return true }
	controller.configMapsSynced = func() bool { return true }
	controller.deploymentsSynced = func() bool { return true }
	controller.statefulSetsSynced = func() bool { return true }

	controller.deploymentsIndex = cache.NewIndexer(
		cache.MetaNamespaceKeyFunc,
		cache.Indexers{
			"secret":    indexBySecrets,
			"configMap": indexByConfigMaps,
		},
	)
	if testCase.deployment != nil {
		controller.deploymentsIndex.Add(testCase.deployment)
	}

	controller.statefulSetsIndex = cache.NewIndexer(
		cache.MetaNamespaceKeyFunc,
		cache.Indexers{
			"secret":    indexBySecrets,
			"configMap": indexByConfigMaps,
		},
	)
	if testCase.statefulSet != nil {
		controller.statefulSetsIndex.Add(testCase.statefulSet)
	}
}

// filterTriggerCaseRuntimeObjects is used to get a list of non-nil objects for
// a given testCase. Generally used to populate the mock kubernetes client.
func filterTriggerCaseRuntimeObjects(c *triggerCase) []runtime.Object {
	objs := []runtime.Object{}
	if c.configMap != nil {
		objs = append(objs, c.configMap)
	}
	if c.secret != nil {
		objs = append(objs, c.secret)
	}
	if c.deployment != nil {
		objs = append(objs, c.deployment)
	}
	if c.statefulSet != nil {
		objs = append(objs, c.statefulSet)
	}
	return objs
}

var triggerCases = []triggerCase{
	triggerCase{
		name: "secret with data and a single listening deployment",
		secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fake-deployment-secret",
				Namespace: "test",
			},
			Data: map[string][]byte{
				"hello": []byte("world"),
			},
		},
		deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fake-deployment",
				Namespace: "test",
				Annotations: map[string]string{
					triggerSecretsAnnotation: "fake-deployment-secret",
				},
			},
		},
		expectedHash: "trigger.k8s.io/secret-fake-deployment-secret-last-hash",
	},
	triggerCase{
		name: "secret with data and a single listening statefulset",
		secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fake-statefulset-secret",
				Namespace: "test",
			},
			Data: map[string][]byte{
				"hello": []byte("world"),
			},
		},
		statefulSet: &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fake-statefulset",
				Namespace: "test",
				Annotations: map[string]string{
					triggerSecretsAnnotation: "fake-statefulset-secret",
				},
			},
		},
		expectedHash: "trigger.k8s.io/secret-fake-statefulset-secret-last-hash",
	},
	triggerCase{
		name: "configMap with data and a single listening deployment",
		configMap: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fake-deployment-configmap",
				Namespace: "test",
			},
			Data: map[string]string{
				"hello": "world",
			},
		},
		deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fake-deployment",
				Namespace: "test",
				Annotations: map[string]string{
					triggerConfigMapsAnnotation: "fake-deployment-configmap",
				},
			},
		},
		expectedHash: "trigger.k8s.io/configMap-fake-deployment-configmap-last-hash",
	},
}

func TestTriggerControllerCases(t *testing.T) {
	for _, c := range triggerCases {
		t.Run(c.name, func(t *testing.T) {
			objs := filterTriggerCaseRuntimeObjects(&c)
			kubeclient := fake.NewSimpleClientset(objs...)

			fakeSecretWatch := watch.NewFake()
			kubeclient.PrependWatchReactor("secrets",
				func(action clientgotesting.Action) (handled bool, ret watch.Interface, err error) {
					return true, fakeSecretWatch, nil
				},
			)

			fakeConfigMapWatch := watch.NewFake()
			kubeclient.PrependWatchReactor("configmaps",
				func(action clientgotesting.Action) (handled bool, ret watch.Interface, err error) {
					return true, fakeConfigMapWatch, nil
				},
			)

			fakeDeploymentWatch := watch.NewFake()
			kubeclient.PrependWatchReactor("deployments",
				func(action clientgotesting.Action) (handled bool, ret watch.Interface, err error) {
					return true, fakeDeploymentWatch, nil
				},
			)

			fakeStatefulSetWatch := watch.NewFake()
			kubeclient.PrependWatchReactor("statefulsets",
				func(action clientgotesting.Action) (handled bool, ret watch.Interface, err error) {
					return true, fakeStatefulSetWatch, nil
				},
			)

			informerFactory := informers.NewSharedInformerFactory(kubeclient, 0)
			controller := NewTriggerController(kubeclient, informerFactory)
			stubTriggerControllerIndexers(c, controller)

			stopChannel := make(chan struct{})
			defer close(stopChannel)
			informerFactory.Start(stopChannel)
			go controller.Run(2, stopChannel)

			modifyAndAssertHashes(t, controller, c.expectedHash, func() {
				if c.secret != nil {
					fakeSecretWatch.Modify(c.secret)
				}
				if c.configMap != nil {
					fakeConfigMapWatch.Modify(c.configMap)
				}
			})
		})
	}
}

// modifyAndAssertHashes instruments our hash-generating callback methods and
// asserts that they've been called with appropriate values. This function is
// not thread safe due to the patching of the controller instance.
func modifyAndAssertHashes(t *testing.T, controller *TriggerController, desiredHash string, modifyFn func()) {
	t.Helper()

	hashCalculated := make(chan bool)
	controller.calculateDataHashFn = func(obj interface{}) string {
		defer close(hashCalculated)
		return calculateDataHash(obj)
	}

	lastHashAnnotationSet := make(chan bool)
	originalLastHashAnnotation := lastHashAnnotation
	// Restore our spied annotation generator.
	defer func() { lastHashAnnotation = originalLastHashAnnotation }()

	var lastHash string
	lastHashAnnotation = func(kind, name string) string {
		defer close(lastHashAnnotationSet)
		lastHash = originalLastHashAnnotation(kind, name)
		return lastHash
	}

	modifyFn()

	select {
	case <-hashCalculated:
	case <-time.After(time.Duration(1 * time.Second)):
		t.Fatalf("failed to calculate hash")
	}

	select {
	case <-lastHashAnnotationSet:
	case <-time.After(time.Duration(1 * time.Second)):
		t.Fatalf("failed to set annotation")
	}

	if lastHash != desiredHash {
		t.Fatalf("invalid lastHashAnnotation %q, wanted %q", lastHash, desiredHash)
	}
}
