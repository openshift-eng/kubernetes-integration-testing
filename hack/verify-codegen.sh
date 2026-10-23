#!/bin/sh
set -e

API_DIR="api/k8s/v1alpha1"
CRD_DIR="api/crds"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

mkdir -p "${TMP_DIR}/before" "${TMP_DIR}/current"
cp "${API_DIR}"/zz_generated.deepcopy.go "${TMP_DIR}/before/"
cp "${CRD_DIR}"/*.yaml "${TMP_DIR}/before/"

./hack/update-codegen.sh > /dev/null 2>&1

cp "${API_DIR}"/zz_generated.deepcopy.go "${TMP_DIR}/current/"
cp "${CRD_DIR}"/*.yaml "${TMP_DIR}/current/"

if ! diff -rq "${TMP_DIR}/before" "${TMP_DIR}/current" > /dev/null 2>&1; then
    echo "ERROR: codegen files are out of date."
    diff -ru "${TMP_DIR}/before" "${TMP_DIR}/current" || true
    echo "Run 'make generate' and commit the result."
    exit 1
fi

echo "codegen files are up to date."
