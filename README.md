# kit - Kubernetes Integration Testing

> **This is a proof of concept** built with the help of Claude. It is not intended for production use. The final version may differ entirely from what is currently in this repository.

Lightweight tool for creating simulated Kubernetes clusters using [kwok](https://kwok.sigs.k8s.io/) and deploying OpenShift components into them. Workload pods run as real containers via podman, while the control plane is simulated.

## Prerequisites

- Go 1.26+
- podman (rootless)
- kwokctl is downloaded automatically on first use

## Build

```bash
make all       # build the binary to ./bin/kit
make clean     # remove build artifacts
```

## Usage

kit runs as a daemon that manages cluster lifecycle via gRPC.

### Starting and stopping the daemon

```bash
./bin/kit start              # start the daemon in the background
./bin/kit start --foreground # start in the foreground (useful for debugging)
./bin/kit stop               # stop the daemon
```

Before launching a new version of kit, make sure the old daemon is stopped:

```bash
./bin/kit stop
make all
./bin/kit start
```

### Daemon logs

```bash
./bin/kit logs        # show recent logs
tail -f ~/.local/share/openshift-kit/daemon.log
```

### Creating a cluster

There are two providers: **kwok** (lightweight, simulated control plane) and **kind** (full Kubernetes cluster via kind).

#### kwok (default)

```bash
./bin/kit cluster create test-cluster \
  --pull-secret ~/mco-dev/pull-secret
```

#### kind

```bash
./bin/kit cluster create test-cluster \
  --provider kind \
  --pull-secret ~/mco-dev/pull-secret \
  --workers 2
```

### Deploying operators

Operator deployment is driven by `KitCluster` spec and `DeploymentPlan` resources. After creating a cluster, patch the `KitCluster` with the release image and feature set, then apply a `DeploymentPlan`:

```bash
export KUBECONFIG=$(./bin/kit cluster kubeconfig test-cluster)

oc patch kitcluster test-cluster --type merge -p \
  '{"spec":{"image":"registry.ci.openshift.org/ocp/release-5:5.1.0-0.nightly-2026-10-21-040310", "featureSet":"TechPreviewNoUpgrade"}}'

oc apply -f deploymentplan.yaml
```

A `DeploymentPlan` declares an ordered sequence of steps. Each step is either a `ResourceCollection` (rendered templates) or a set of manifests filtered from the release image:

```yaml
apiVersion: kit.openshift.io/v1alpha1
kind: DeploymentPlan
metadata:
  name: mco-deployment
spec:
  steps:
    - name: default-collection
      collection:
        source: openshift-default
    - name: mco-collection
      collection:
        source: openshift-mco
    - name: machine-config-operator
      release:
        filenameRegex: machine-config
```

### Interacting with the cluster

```bash
export KUBECONFIG=$(./bin/kit cluster kubeconfig test-cluster)
kubectl get pods -A
kubectl logs -n openshift-machine-config-operator <pod-name>
```

### Destroying a cluster

```bash
./bin/kit cluster destroy test-cluster
```

## Architecture

- **kwok** simulates the Kubernetes control plane (etcd, API server, controller-manager, scheduler) as podman containers on a shared podman network.
- **kit controller** watches for pods claimed by kwok fake nodes and starts real containers for them via podman.
- **Service DNS** is provided by podman's aardvark-dns: each pod gets `--network-alias` entries matching Kubernetes service DNS conventions (`<svc>.<ns>.svc.cluster.local`).
- **Volume materialization**: Secrets and ConfigMaps are fetched from the API and bind-mounted into containers.
- **Service certificates**: a controller-runtime reconciler watches Services with the `service.beta.openshift.io/serving-cert-secret-name` annotation and auto-generates TLS secrets, replicating the OpenShift service-ca-operator behavior.
- **DeploymentPlan**: an ordered pipeline of collection and release steps with automatic retry on failure.
- **ResourceCollection**: templated Kubernetes manifests with cert helper functions, rendered and applied by the collection reconciler.
