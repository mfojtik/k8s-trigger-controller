package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func fakeSecret(mutateFn func(s *corev1.Secret)) *corev1.Secret {
	s := &corev1.Secret{}
	s.Name = "fake-secret"
	mutateFn(s)
	return s
}
func fakeConfig(mutateFn func(s *corev1.ConfigMap)) *corev1.ConfigMap {
	s := &corev1.ConfigMap{}
	s.Name = "fake-config-map"
	mutateFn(s)
	return s
}

func TestCalculateHash(t *testing.T) {
	samples := []struct {
		name         string
		obj          interface{}
		expectedHash string
	}{
		{
			name: "simple secret",
			obj: fakeSecret(func(s *corev1.Secret) {
				s.Data = map[string][]byte{}
				s.Data["foo"] = []byte("bar")
			}),
			expectedHash: "8843d7f92416211de9ebb963ff4ce28125932878",
		},
		{
			name: "simple configMap",
			obj: fakeConfig(func(s *corev1.ConfigMap) {
				s.Data = map[string]string{}
				s.Data["foo"] = "bar"
			}),
			expectedHash: "8843d7f92416211de9ebb963ff4ce28125932878",
		},
		{
			name: "simple different configMap",
			obj: fakeConfig(func(s *corev1.ConfigMap) {
				s.Data = map[string]string{}
				s.Data["foo"] = "hello world"
			}),
			expectedHash: "d766ca6a87a1a587bc5b3decf8619ad72a79417e",
		},
		{
			name: "binary data configMap",
			obj: fakeConfig(func(s *corev1.ConfigMap) {
				s.BinaryData = map[string][]byte{}
				s.BinaryData["foo"] = []byte("hello world")
			}),
			expectedHash: "d766ca6a87a1a587bc5b3decf8619ad72a79417e",
		},
		{
			name: "simple string data secret",
			obj: fakeSecret(func(s *corev1.Secret) {
				s.StringData = map[string]string{}
				s.StringData["foo"] = "bar"
			}),
			expectedHash: "8843d7f92416211de9ebb963ff4ce28125932878",
		},
		{
			name: "empty secret",
			obj: fakeSecret(func(s *corev1.Secret) {
			}),
			expectedHash: "",
		},
		{
			name:         "unknown object",
			obj:          nil,
			expectedHash: "",
		},
	}

	for _, s := range samples {
		hash := calculateDataHash(s.obj)
		if hash != s.expectedHash {
			t.Errorf("[%s] expected hash: %s, got %q", s.name, s.expectedHash, hash)
		}
	}
}
