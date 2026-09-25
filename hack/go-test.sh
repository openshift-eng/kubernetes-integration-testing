#!/bin/sh
set -ex

GO_PKG="github.com/openshift-eng/kubernetes-integration-testing"
GIT_TAG="${BUILD_VERSION:-$(git describe --always --abbrev=40 --dirty)}"
DEFAULT_ARCH="${DEFAULT_ARCH:-amd64}"
LDFLAGS="${LDFLAGS} -X ${GO_PKG}/pkg/version.Raw=${GIT_TAG} -X ${GO_PKG}/pkg/version.DefaultArch=${DEFAULT_ARCH}"

if [ "${IS_CONTAINER}" != "" ] || [ "${OPENSHIFT_CI}" != "" ]; then
    go test -ldflags "${LDFLAGS}" ./... "${@}"
else
    podman run --rm \
        --env IS_CONTAINER=TRUE \
        --env LDFLAGS="${LDFLAGS}" \
        --volume "${PWD}:/go/src/${GO_PKG}:z" \
        --workdir /go/src/${GO_PKG} \
        docker.io/golang:1.26 \
        ./hack/go-test.sh "${@}"
fi
