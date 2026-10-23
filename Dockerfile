FROM registry.access.redhat.com/ubi9/ubi:latest AS builder
RUN dnf install -y golang && dnf clean all
WORKDIR /go/src/github.com/openshift-eng/kubernetes-integration-testing
COPY . .
RUN TARGETS=kit-controller hack/build.sh

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
RUN microdnf install -y podman-remote && microdnf clean all
COPY --from=builder /go/src/github.com/openshift-eng/kubernetes-integration-testing/bin/kit-controller /usr/bin/kit-controller
ENTRYPOINT ["/usr/bin/kit-controller"]
