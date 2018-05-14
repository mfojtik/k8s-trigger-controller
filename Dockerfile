FROM docker.io/centos:centos7
ADD ./k8s-trigger-controller /usr/bin/k8s-trigger-controller
CMD ["/usr/bin/k8s-trigger-controller", "-v", "5", "-logtostderr", "-alsologtostderr"]
