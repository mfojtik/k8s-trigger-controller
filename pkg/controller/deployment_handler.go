package controller

import (
	"fmt"
	"strings"

	"github.com/golang/glog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/typed/apps/v1"

	"github.com/mfojtik/k8s-trigger-controller/pkg/apis/trigger/v1alpha1"
)

type HandlerInterface interface {
	Handle(namespace, deployment string) error
}

type DeploymentHandler struct {
	client         v1.DeploymentsGetter
	deploymentName string
	secretName     string
	configMapName  string
	namespace      string
	hash           string
}

// NewDeploymentHandler returns a new handler for Deployments
func NewDeploymentHandler(client v1.DeploymentsGetter, kind string, configNamespace, configName string, hash string) (HandlerInterface, error) {
	h := &DeploymentHandler{
		client:    client,
		hash:      hash,
		namespace: configNamespace,
	}
	switch kind {
	case "configMap":
		h.configMapName = configName
	case "secret":
		h.secretName = configName
	default:
		return nil, fmt.Errorf("unknown kind: %q", kind)
	}
	return h, nil
}

// Handle handles the given deployment secrets/configMaps
// NOTE: This can (and will) race with the deployment controller. If there are multiple secrets/configMaps that trigger updated in quick succession,
// then this method can trigger multiple rollouts (the number of rollouts is nondetermistic).
func (h *DeploymentHandler) Handle(namespace, deploymentName string) error {
	d, err := h.client.Deployments(namespace).Get(deploymentName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	annotations := d.Spec.Template.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}

	// First try to determine if we need to update the deployment (when it already has the annotation)
	isUpdate := false
	for name, value := range annotations {
		if strings.HasPrefix(name, v1alpha1.TriggerByDeploymentSourceAnnotationPrefix) {
			c := strings.TrimPrefix(name, v1alpha1.TriggerByDeploymentSourceAnnotationPrefix+".")
			switch c {
			case h.namespace + "." + h.configMapName:
				if value == h.hash {
					// No-op, the hashes are unchanged, return early
					glog.V(3).Infof("Deployment %s/%s already has up to date configMap %s", namespace, deploymentName, h.configMapName)
					return nil
				}
				annotations[v1alpha1.TriggerByDeploymentSourceAnnotationPrefix+"."+h.namespace+"."+h.configMapName] = h.hash
				isUpdate = true
			case h.namespace + "." + h.secretName:
				if value == h.hash {
					glog.V(3).Infof("Deployment %s/%s already has up to date secret %s", namespace, deploymentName, h.secretName)
					// No-op, the hashes are unchanged, return early
					return nil
				}
				annotations[v1alpha1.TriggerByDeploymentSourceAnnotationPrefix+"."+h.namespace+"."+h.secretName] = h.hash
				isUpdate = true
			default:
				glog.Infof("Invalid %s annotation format in %s/%s", v1alpha1.TriggerByDeploymentSourceAnnotationPrefix, namespace, deploymentName)
				return nil
			}
			if isUpdate {
				break
			}
		}
	}

	// If the deployment does not have the annotation set, then set it (probably the secret/configMap just started to trigger now)
	if !isUpdate {
		if len(h.configMapName) > 0 {
			annotations[v1alpha1.TriggerByDeploymentSourceAnnotationPrefix+"."+h.namespace+"."+h.configMapName] = h.hash
		}
		if len(h.secretName) > 0 {
			annotations[v1alpha1.TriggerByDeploymentSourceAnnotationPrefix+"."+h.namespace+"."+h.secretName] = h.hash
		}
	}

	// We need to update the hash for the secret/configMap
	dCopy := d.DeepCopy()
	dCopy.Spec.Template.Annotations = annotations
	_, err = h.client.Deployments(namespace).Update(dCopy)
	return err
}
