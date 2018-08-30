[![Travis](https://api.travis-ci.org/mfojtik/k8s-trigger-controller.svg?branch=master)](https://travis-ci.org/mfojtik/k8s-trigger-controller)
[![Licensed under Apache License version 2.0](https://img.shields.io/github/license/openshift/origin.svg?maxAge=2592000)](https://www.apache.org/licenses/LICENSE-2.0)
[![Docker Automated Build](https://img.shields.io/docker/build/mfojtik/k8s-trigger-controller.svg)](https://hub.docker.com/r/mfojtik/k8s-trigger-controller/)
# k8s-trigger-controller

This Kubernetes controller, when installed will let users to configure Secret or ConfigMap
names on Deployments that should trigger a rollout of this Deployment when the data inside
the Secret or ConfigMap change.

Demo: https://youtu.be/SRDsRZwAdlA

## Installation

On Kubernetes, you can run the controller image:

```
$ kubectl run trigger-controller --image=docker.io/mfojtik/k8s-trigger-controller:latest --generator=deployment/apps.v1beta1
```

On OpenShift, you have to grant the controller permissions to work properly:

```
# Create special project and role
$ oc new-project k8s-trigger-controller
$ oc create clusterrole trigger-controller --verb=get,list,update,watch --resource=secrets,configmaps,deployments
$ oc adm policy add-cluster-role-to-user trigger-controller system:serviceaccount:k8s-trigger-controller:default

$ kubectl run trigger-controller --image=docker.io/mfojtik/k8s-trigger-controller:latest --generator=deployment/apps.v1beta1 -n k8s-trigger-controller
```

## Usage

First create Deployment:
```
$ kubectl run sleeper --image=docker.io/centos:7 --generator=deployment/apps.v1beta1 --command -- /bin/bash -c "sleep infinity"
```

Create Secret:
```
$ kubectl create secret generic top-secret --from-literal=foo=bar
```

Add the Secret into Deployment as a volume (in Kubernetes, edit the Deployment resource)
```
$ oc volume deployment/sleeper --add --secret-name=top-secret -m /secret
```

Now, once the trigger controller is running, you can annotate the Deployment to indicate that
you want to automatically rollout when the `top-secret` Secret is changed:

```
$ kubectl annotate deployment/sleeper trigger.k8s.io/triggering-secrets='top-secret'
```

You can specify multiple Secrets separated by comma. For ConfigMaps, just use '-configMaps' in the annotation.

Now, when you change the content of the Secret `top-secret` (`kubectl edit secret/top-secret`) and change
the value of the *'foo'* key and save, you should see that a new rollout is triggered automatically.

## How it works

When the controller observes a Deployment or StatefulSet with the `trigger.k8s.io/triggering-secrets` annotation, it will automatically calculate hash of the Secret 'data' field and store it inside the Secret `trigger.k8s.io/data-hash` annotation. It will behave similarly for configMaps via the `trigger.k8s.io/triggering-configMaps` annotation.

Then it looks up the Deployment and checks if the ReplicaSet template embedded inside Deployment contain
the `trigger.k8s.io/[secret|configMap]-NAME-last-hash` annotation. This annotation value represents the last
observed hash. If the hash differs or the annotation is not present, the controller update the template
with the current Secret or ConfigMap hash. Updating the template will cause the the Deployment or StatefulSet to
rollout new version as per it's RollingUpdate configuration.

## Limitations && TODO

* Currently only Deployments and StatefulSets are supported, DaemonSets are TBD
* If secrets or configMaps are updated in bulk, the controller might trigger rollout for every update (you should pause the Deployment in that case)
* The hash calculation should be more efficient
* Versioning of ConfigMaps and Secrets is out of scope for this controller

## License

 k8s-trigger-controller is licensed under the [Apache License, Version 2.0](http://www.apache.org/licenses/).