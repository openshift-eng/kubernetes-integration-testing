#!/bin/sh
set -ex

if [ "${IS_CONTAINER}" != "" ] || [ "${OPENSHIFT_CI}" != "" ]; then
    golangci-lint run "${@}"
else
    podman run --rm \
        --env IS_CONTAINER=TRUE \
        --volume "${PWD}:/app:z" \
        --workdir /app \
        docker.io/golangci/golangci-lint:v2.10.1 \
        golangci-lint run "${@}"
fi
