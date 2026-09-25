FROM registry.ci.openshift.org/ocp/builder:rhel-9-golang-1.26-openshift-5.0 AS builder
WORKDIR /go/src/github.com/openshift-eng/kubernetes-integration-testing
COPY . .
RUN hack/build.sh

FROM registry.ci.openshift.org/ocp/5.0:base-rhel9
COPY --from=builder /go/src/github.com/openshift-eng/kubernetes-integration-testing/bin/kit /usr/bin/kit
ENTRYPOINT ["/usr/bin/kit"]
