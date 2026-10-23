#!/usr/bin/env bash
set -euo pipefail

N="${1:-3}"
PREFIX="test-cluster"

echo "Creating ${N} clusters in parallel..."

pids=()
for i in $(seq 1 "$N"); do
    name="${PREFIX}-${i}"
    echo "  -> ${name}"
    ./bin/kit cluster create "$name" --runtime podman &
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
    echo "${failed}/${N} clusters failed."
fi

echo ""
echo "Destroying ${N} clusters in parallel..."

pids=()
for i in $(seq 1 "$N"); do
    name="${PREFIX}-${i}"
    echo "  -> ${name}"
    ./bin/kit cluster destroy "$name" &
    pids+=($!)
done

dfailed=0
for pid in "${pids[@]}"; do
    if ! wait "$pid"; then
        ((dfailed++))
    fi
done

echo ""
if [ "$dfailed" -eq 0 ]; then
    echo "All ${N} clusters destroyed successfully."
else
    echo "${dfailed}/${N} clusters failed to destroy."
fi

[ "$failed" -eq 0 ] && [ "$dfailed" -eq 0 ]
