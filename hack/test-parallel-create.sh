#!/usr/bin/env bash
set -euo pipefail

N="${1:-3}"
PROVIDER="${2:-kwok}"
PREFIX="test-cluster"
KIT="./bin/kit"

cleanup() {
    echo ""
    echo "Cleaning up all clusters..."
    local pids=()
    for i in $(seq 1 "$N"); do
        "$KIT" cluster destroy "${PREFIX}-${i}" 2>/dev/null &
        pids+=($!)
    done
    for pid in "${pids[@]}"; do
        wait "$pid" 2>/dev/null || true
    done
    echo "Cleanup done."
}

trap cleanup EXIT

echo "Creating ${N} clusters in parallel (provider: ${PROVIDER})..."

pids=()
for i in $(seq 1 "$N"); do
    name="${PREFIX}-${i}"
    echo "  -> ${name}"
    "$KIT" cluster create "$name" --provider "$PROVIDER" &
    pids+=($!)
done

failed=0
for pid in "${pids[@]}"; do
    if ! wait "$pid"; then
        ((failed++))
    fi
done

echo ""
if [ "$failed" -eq 0 ]; then
    echo "All ${N} clusters created successfully."
else
    echo "${failed}/${N} clusters failed to create."
    exit 1
fi
