FROM docker.io/library/golang:1.26 AS builder
WORKDIR /go/src/github.com/openshift-eng/kubernetes-integration-testing
COPY . .
RUN TARGETS=kit-controller hack/build.sh

FROM quay.io/centos/centos:stream9-minimal
RUN microdnf install -y podman-remote curl && microdnf clean all
COPY --from=builder /go/src/github.com/openshift-eng/kubernetes-integration-testing/bin/kit-controller /usr/bin/kit-controller
ENTRYPOINT ["/usr/bin/kit-controller"]
