#!/usr/bin/env bash
set -euo pipefail

PULL_SECRET="${PULL_SECRET:-$HOME/mco-dev/pull-secret}"
RELEASE_IMAGE="quay.io/openshift-release-dev/ocp-release:5.0.0-x86_64"
CLUSTER_NAME="test-cluster"
PROVIDER="${PROVIDER:-kwok}"
KUBE_VERSION="${KUBE_VERSION:-}"

usage() {
  echo "Usage: $0 [provider] [--provider kwok|kind] [--kube-version v1.36.4]" >&2
}

if [[ $# -ge 1 && "$1" != --* ]]; then
  PROVIDER="$1"
  shift
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    --provider)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      PROVIDER="$2"
      shift 2
      ;;
    --provider=*)
      PROVIDER="${1#*=}"
      shift
      ;;
    --kube-version)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      KUBE_VERSION="$2"
      shift 2
      ;;
    --kube-version=*)
      KUBE_VERSION="${1#*=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

echo "=== Building ==="
make all
make image-dev

echo "=== Cleaning previous state ==="
if ./bin/kit status >/dev/null 2>&1; then
  for c in $(./bin/kit cluster list 2>/dev/null); do
    echo "Destroying leftover cluster: $c"
    ./bin/kit cluster destroy "$c" || true
  done
  ./bin/kit stop || true
  sleep 2
fi

echo "=== Starting daemon ==="
DAEMON_PID=""
cleanup() {
  if [[ -n "$DAEMON_PID" ]]; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

KIT_CONTROLLER_IMAGE=localhost/kit-controller:latest ./bin/kit start --foreground &
DAEMON_PID=$!
sleep 3
if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
  wait "$DAEMON_PID"
fi

echo "=== Creating cluster ==="
echo "Provider: $PROVIDER"
CREATE_ARGS=(--provider "$PROVIDER" --pull-secret "$PULL_SECRET")
if [[ -n "$KUBE_VERSION" ]]; then
  echo "Kube version: $KUBE_VERSION"
  CREATE_ARGS+=(--kube-version "$KUBE_VERSION")
fi
./bin/kit cluster create "$CLUSTER_NAME" "${CREATE_ARGS[@]}"

export KUBECONFIG="$HOME/.local/share/openshift-kit/clusters/$CLUSTER_NAME/kubeconfig"

echo "=== Patching KitCluster ==="
oc patch kitcluster cluster --type merge -p \
  "{\"spec\":{\"image\":\"$RELEASE_IMAGE\", \"featureSet\":\"TechPreviewNoUpgrade\"}}"

sleep 2

echo "=== Applying DeploymentPlan ==="
oc apply -f deploymentplan.yaml

NS="openshift-machine-config-operator"
EXPECTED_POD_PREFIXES=(
  machine-config-operator
  machine-config-controller
  machine-config-daemon
  machine-config-server
)

expected_pods_present() {
  local prefix
  for prefix in "${EXPECTED_POD_PREFIXES[@]}"; do
    oc get pods -n "$NS" -o name 2>/dev/null | grep -q "^pod/${prefix}" || return 1
  done
}

wait_for_all_pods_ready() {
  echo "=== Waiting for all MCO pods to be ready ==="
  until expected_pods_present; do
    sleep 5
  done

  # Run oc wait in a loop so pods created after the first check are also
  # included before the final readiness pass succeeds.
  until oc wait --for=condition=Ready pod --all -n "$NS" --timeout=10s >/dev/null; do
    sleep 5
  done
  echo "All MCO pods are ready"
}

wait_for_mcps_updated() {
  echo "=== Waiting for MachineConfigPools to be updated ==="
  until oc get machineconfigpools -o name 2>/dev/null | grep -q '^machineconfigpool'; do
    sleep 5
  done
  oc wait --for=condition=Updated machineconfigpool --all --timeout=900s
  echo "All MachineConfigPools are updated"
}

wait_for_cluster_operator_healthy() {
  echo "=== Waiting for the machine-config ClusterOperator to be healthy ==="
  until oc get clusteroperator machine-config >/dev/null 2>&1; do
    sleep 5
  done
  oc wait --for=condition=Available clusteroperator/machine-config --timeout=900s
  oc wait --for=condition=Progressing=False clusteroperator/machine-config --timeout=900s
  oc wait --for=condition=Degraded=False clusteroperator/machine-config --timeout=900s
  echo "The machine-config ClusterOperator is healthy"
}

wait_for_all_pods_ready
wait_for_mcps_updated
wait_for_cluster_operator_healthy

echo ""
echo "=== MCO deployed ==="
oc get pods -n "$NS"
oc get machineconfigpools
oc get clusteroperator machine-config

echo ""
echo "Press Enter to tear down..."
read -r

echo "=== Destroying cluster ==="
./bin/kit cluster destroy "$CLUSTER_NAME"
