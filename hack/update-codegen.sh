#!/bin/sh
set -ex

API_DIR="api/k8s/v1alpha1"
CRD_OUTPUT="api/crds"

if [ "${IS_CONTAINER}" != "" ] || [ "${OPENSHIFT_CI}" != "" ]; then
    go tool controller-gen \
        object \
        paths="./${API_DIR}/..."

    go tool controller-gen \
        crd \
        paths="./${API_DIR}/..." \
        output:crd:dir="${CRD_OUTPUT}"
else
    podman run --rm \
        --env IS_CONTAINER=TRUE \
        --volume "${PWD}:/app:z" \
        --workdir /app \
        docker.io/golang:1.26 \
        sh -c "./hack/update-codegen.sh"
fi
