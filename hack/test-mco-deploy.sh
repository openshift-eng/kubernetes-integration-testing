#!/usr/bin/env bash
set -euo pipefail

PULL_SECRET="${PULL_SECRET:-$HOME/mco-dev/pull-secret}"
RELEASE_IMAGE="registry.ci.openshift.org/ocp/release-5:5.1.0-0.nightly-2026-10-21-040310"
CLUSTER_NAME="test-cluster"

echo "=== Building ==="
make all
make image-dev

echo "=== Starting daemon ==="
KIT_CONTROLLER_IMAGE=localhost/kit-controller:latest ./bin/kit start --foreground &
DAEMON_PID=$!
trap 'kill $DAEMON_PID 2>/dev/null; wait $DAEMON_PID 2>/dev/null' EXIT
sleep 3

echo "=== Creating cluster ==="
./bin/kit cluster create "$CLUSTER_NAME" --pull-secret "$PULL_SECRET"

export KUBECONFIG="$HOME/.local/share/openshift-kit/clusters/$CLUSTER_NAME/kubeconfig"

echo "=== Patching KitCluster ==="
oc patch kitcluster "$CLUSTER_NAME" --type merge -p \
  "{\"spec\":{\"image\":\"$RELEASE_IMAGE\", \"featureSet\":\"TechPreviewNoUpgrade\"}}"

sleep 2

echo "=== Applying DeploymentPlan ==="
oc apply -f deploymentplan.yaml

NS="openshift-machine-config-operator"

wait_for_pod() {
  local prefix="$1"
  echo "  Waiting for $prefix..."
  until oc get pods -n "$NS" 2>/dev/null | grep "$prefix" | grep -qE 'Running|CrashLoopBackOff'; do
    sleep 5
  done
  echo "  $prefix ready"
}

echo "=== Waiting for MCO pods ==="
wait_for_pod machine-config-operator
wait_for_pod machine-config-controller
wait_for_pod machine-config-daemon
wait_for_pod machine-config-server

echo ""
echo "=== MCO deployed ==="
oc get pods -n "$NS"

echo ""
echo "Press Enter to tear down..."
read -r

echo "=== Destroying cluster ==="
./bin/kit cluster destroy "$CLUSTER_NAME"
