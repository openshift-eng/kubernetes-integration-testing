#!/bin/sh
set -ex

GO_PKG="github.com/openshift-eng/kubernetes-integration-testing"
BINARY="${BINARY:-kit}"
OUTPUT="${OUTPUT:-bin/${BINARY}}"
MODE="${MODE:-release}"

GIT_COMMIT="${SOURCE_GIT_COMMIT:-$(git rev-parse --verify 'HEAD^{commit}')}"
GIT_TAG="${BUILD_VERSION:-$(git describe --always --abbrev=40 --dirty)}"
BUILD_DATE="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
DEFAULT_ARCH="${DEFAULT_ARCH:-amd64}"
GOFLAGS="${GOFLAGS:--mod=vendor}"
GCFLAGS=""
LDFLAGS="${LDFLAGS} -X ${GO_PKG}/pkg/version.Raw=${GIT_TAG} -X ${GO_PKG}/pkg/version.Commit=${GIT_COMMIT} -X ${GO_PKG}/pkg/version.BuildDate=${BUILD_DATE} -X ${GO_PKG}/pkg/version.DefaultArch=${DEFAULT_ARCH}"

case "${MODE}" in
release)
    LDFLAGS="${LDFLAGS} -s -w"
    export CGO_ENABLED=0
    ;;
dev)
    GCFLAGS="${GCFLAGS} all=-N -l"
    ;;
esac

GO_COMPLIANCE_POLICY="exempt_all" go build ${GOFLAGS} -gcflags "${GCFLAGS}" -ldflags "${LDFLAGS}" -o "${OUTPUT}" .
