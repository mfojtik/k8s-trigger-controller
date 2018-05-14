package v1alpha1

const (
	// TriggerDataHashAnnotation is the name of the annotation key that is set on secret or config map that contains a hash of the current content.
	// This annotation is added to the secret or config map automatically when below annotation is set on the workload controller.
	TriggerDataHashAnnotation = "trigger.k8s.io/data-hash"

	// TriggerDeploymentAnnotationPrefix is an annotation prefix that specifies the namespace and the name of the deployment to trigger when the
	// secret or the configMap changes.
	// The format of this annotation is: 'trigger.k8s.io/deployment/namespace/name' = 'enabled|paused'
	TriggerDeploymentAnnotationPrefix = "trigger.k8s.io/deployment"

	// TriggerByDeploymentSourceAnnotationPrefix is an annotation prefix used in deployments to store the current hash of the source secret of the
	// config map. This annotation is stored in the PodTemplateSpec of the deployment definition,
	// so mutating it will cause a new deployment to trigger.
	// The format of this annotation is: 'trigger.k8s.io/by/[secret|configMap]/namespace/name' = 'HASH'
	TriggerByDeploymentSourceAnnotationPrefix = "trigger.k8s.io/by"

	// TriggerEnabled means the trigger is enabled and will mutate the specified deployment when change occurs
	TriggerEnabled = "enabled"
	// TriggerPaused means the trigger is paused,  which means the data-hash for the secret or configMap will update but it won't update the linked
	// deployment.
	TriggerPaused = "paused"
)
