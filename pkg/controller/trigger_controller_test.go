package controller

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func fakeDeployment(mutateFn func(d *appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{}
	d.Namespace = "test"
	d.Name = "fake-deployment"
	mutateFn(d)
	return d
}

func TestTriggerController(t *testing.T) {
	secret := fakeSecret(func(s *corev1.Secret) {
		s.Data = map[string][]byte{"foo": []byte("bar")}
	})
	deploy := fakeDeployment(func(d *appsv1.Deployment) {
		d.Annotations = map[string]string{
			triggerSecretsAnnotation: "fake-secret",
		}
		secretVolume := corev1.Volume{}
		secretVolume.Secret = &corev1.SecretVolumeSource{SecretName: secret.Name}
		d.Spec.Template.Spec.Volumes = []corev1.Volume{secretVolume}
	})

	kubeclient := fake.NewSimpleClientset([]runtime.Object{secret, deploy}...)
	informerFactory := informers.NewSharedInformerFactory(kubeclient, 0)
	controller := NewTriggerController(kubeclient, informerFactory)
	fakeSecretWatch := watch.NewFake()
	fakeDeploymentWatch := watch.NewFake()
	kubeclient.PrependWatchReactor("secrets",
		func(action clientgotesting.Action) (handled bool, ret watch.Interface, err error) {
			return true, fakeSecretWatch, nil
		})
	kubeclient.PrependWatchReactor("deployments",
		func(action clientgotesting.Action) (handled bool, ret watch.Interface, err error) {
			return true, fakeDeploymentWatch, nil
		})

	controller.secretsSynced = func() bool { return true }
	controller.configMapsSynced = func() bool { return true }
	controller.deploymentsSynced = func() bool { return true }

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{"secret": indexDeploymentsByTriggeringSecrets})
	indexer.Add(deploy)
	controller.deploymentsIndex = indexer

	hashCalculated := make(chan bool)
	controller.calculateDataHashFn = func(obj interface{}) string {
		defer close(hashCalculated)
		return calculateDataHash(obj)
	}

	stopChannel := make(chan struct{})
	defer close(stopChannel)
	informerFactory.Start(stopChannel)
	go controller.Run(2, stopChannel)

	fakeSecretWatch.Modify(secret)

	lastHashAnnotationSet := make(chan bool)
	originLastHashAnnotation := lastHashAnnotation
	var lastHash string
	lastHashAnnotation = func(kind, name string) string {
		defer close(lastHashAnnotationSet)
		lastHash = originLastHashAnnotation(kind, name)
		return lastHash
	}

	select {
	case <-hashCalculated:
	case <-time.After(time.Duration(15 * time.Second)):
		t.Fatalf("failed to calculate hash")
	}

	select {
	case <-lastHashAnnotationSet:
	case <-time.After(time.Duration(15 * time.Second)):
		t.Fatalf("failed to set deployment annotation")
	}

	if lastHash != "trigger.k8s.io/secret-fake-secret-last-hash" {
		t.Fatalf("invalid lastHashAnnotation %q", lastHash)
	}
}
