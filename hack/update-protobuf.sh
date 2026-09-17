#!/bin/sh
set -ex

PROTO_DIR="api/proto"
PROTOC_GEN_GO_VERSION="${PROTOC_GEN_GO_VERSION:-$(grep 'google.golang.org/protobuf ' go.mod | awk '{print $2}')}"
PROTOC_GEN_GO_GRPC_VERSION="${PROTOC_GEN_GO_GRPC_VERSION:-$(grep 'google.golang.org/grpc/cmd/protoc-gen-go-grpc ' go.mod | awk '{print $2}')}"

if [ "${IS_CONTAINER}" != "" ] || [ "${OPENSHIFT_CI}" != "" ]; then
    protoc \
        --go_out=. --go_opt=paths=source_relative \
        --go-grpc_out=. --go-grpc_opt=paths=source_relative \
        "${PROTO_DIR}/mco.proto"
else
    podman run --rm \
        --env IS_CONTAINER=TRUE \
        --volume "${PWD}:/app:z" \
        --workdir /app \
        docker.io/golang:1.26 \
        sh -c "DEBIAN_FRONTEND=noninteractive apt-get update -qq && apt-get install -y -qq protobuf-compiler > /dev/null && \
            go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION} && \
            go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION} && \
            ./hack/update-protobuf.sh"
fi
