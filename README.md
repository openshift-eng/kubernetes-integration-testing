# kit - Kubernetes Integration Testing

> **This is a proof of concept** built with the help of Claude. It is not intended for production use. The final version may differ entirely from what is currently in this repository.

Lightweight tool for creating simulated Kubernetes clusters using [kwok](https://kwok.sigs.k8s.io/) and deploying OpenShift components into them. Workload pods run as real containers via podman, while the control plane is simulated.

## Prerequisites

- Go 1.22+
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
  --provider kwok \
  --image registry.ci.openshift.org/ocp/release-5:5.1.0-0.nightly-2026-09-28-053718 \
  --pull-secret ~/mco-dev/pull-secret \
  --include machine-config
```

#### kind

```bash
./bin/kit cluster create test-cluster \
  --provider kind \
  --image registry.ci.openshift.org/ocp/release-5:5.1.0-0.nightly-2026-09-28-053718 \
  --pull-secret ~/mco-dev/pull-secret \
  --include machine-config \
  --workers 2
```

### Interacting with the cluster

```bash
export KUBECONFIG=$(./bin/kit cluster kubeconfig test-cluster)
kubectl get pods -A
kubectl logs -n openshift-machine-config-operator <pod-name>
```

### Destroying a cluster

```bash
./bin/kit cluster destroy test-cluster --provider kwok
```

## Architecture

- **kwok** simulates the Kubernetes control plane (etcd, API server, controller-manager, scheduler) as podman containers on a shared podman network.
- **kit controller** watches for pods claimed by kwok fake nodes and starts real containers for them via podman.
- **Service DNS** is provided by podman's aardvark-dns: each pod gets `--network-alias` entries matching Kubernetes service DNS conventions (`<svc>.<ns>.svc.cluster.local`).
- **Volume materialization**: Secrets and ConfigMaps are fetched from the API and bind-mounted into containers.
- **SecretProvisioner**: a PoC controller that watches workloads and auto-generates TLS secrets for missing secret volume references.
