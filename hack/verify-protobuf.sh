#!/bin/sh
set -e

PROTO_DIR="api/proto"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

cp "${PROTO_DIR}"/*.pb.go "${TMP_DIR}/"

./hack/update-protobuf.sh > /dev/null 2>&1

if ! diff -q "${TMP_DIR}" "${PROTO_DIR}" --exclude='*.proto' > /dev/null 2>&1; then
    echo "ERROR: protobuf generated files are out of date. Run 'make generate' and commit the result."
    diff -u "${TMP_DIR}" "${PROTO_DIR}" --exclude='*.proto' || true
    # Restore original files
    cp "${TMP_DIR}"/*.pb.go "${PROTO_DIR}/"
    exit 1
fi

echo "protobuf files are up to date."
