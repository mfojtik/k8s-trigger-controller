FROM openshift/origin-release:golang-1.9

RUN GOBIN=/usr/bin go get github.com/mfojtik/k8s-trigger-controller

CMD ["/usr/bin/k8s-trigger-controller", "-v", "5", "-logtostderr", "-alsologtostderr"]
