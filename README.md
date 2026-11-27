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

#### Kubernetes version

Both providers accept `--kube-version` to pin the Kubernetes version of the cluster control plane, either a minor (e.g. `v1.36`) or an exact version (e.g. `v1.36.4`). The version the control plane ends up running is reported in `KitCluster.status.kubeVersion`.

### Deploying operators

Operator deployment is driven by `KitCluster` spec and `DeploymentPlan` resources. The simplest path is passing the release image and the kit CRs (deployment plans, resource collections) directly to `create`: the cluster comes up with everything applied and ready, in one command.

```bash
./bin/kit cluster create test-cluster \
  --pull-secret ~/mco-dev/pull-secret \
  --image quay.io/openshift-release-dev/ocp-release:5.0.0-x86_64 \
  --feature-set TechPreviewNoUpgrade \
  -f deploymentplan.yaml
```

`-f` is repeatable and accepts a file, a directory, or `-` for stdin; each may contain several YAML documents. By default the command waits until the applied resources report Ready (`--no-wait` skips the wait, `--timeout` bounds it). Only kit CRs are accepted: kit CRDs are installed by the controller during bootstrap and payload CRDs come from the release image, so neither is passed as a resource.

On failure the cluster is left running for debugging; destroy it with `kit cluster destroy`.

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

### Using kit from Go

The CLI is a thin wrapper over the public client package,
`github.com/openshift-eng/kubernetes-integration-testing/pkg/client`. It
connects to the daemon (starting it if needed) and offers the same
operations:

```go
import "github.com/openshift-eng/kubernetes-integration-testing/pkg/client"

c, err := client.Connect(ctx)
if err != nil {
    return err
}
defer func() { _ = c.Close() }()

cl, err := c.Create(ctx,
    client.WithName("test-cluster"),
    client.WithPullSecret("/path/to/pull-secret"),
    client.WithResources(client.FromFS(manifests, "deploymentplan.yaml")),
)
// cl.Kubeconfig is the path of the cluster kubeconfig file, and
// cl.Applied lists the resources applied at creation.
```

where `manifests` is an `embed.FS` (`//go:embed`), so a suite ships its
kit CRs inside the binary. `client.FromFiles` and `client.FromString`
are available for paths and inline manifests.

Connect starts the daemon if needed and only returns once the daemon
answers RPCs, so a returned Client is ready to use. Set a deadline on the
context to bound the wait.

Programs that are not the kit binary itself (for example test binaries)
must tell the client where to find the kit executable to spawn the
daemon: set `KIT_BINARY` or pass `client.WithBinary(path)`.

## Architecture

- **kwok** simulates the Kubernetes control plane (etcd, API server, controller-manager, scheduler) as podman containers on a shared podman network.
- **kit controller** watches for pods claimed by kwok fake nodes and starts real containers for them via podman.
- **Service DNS** is provided by podman's aardvark-dns: each pod gets `--network-alias` entries matching Kubernetes service DNS conventions (`<svc>.<ns>.svc.cluster.local`).
- **Volume materialization**: Secrets and ConfigMaps are fetched from the API and bind-mounted into containers.
- **Service certificates**: a controller-runtime reconciler watches Services with the `service.beta.openshift.io/serving-cert-secret-name` annotation and auto-generates TLS secrets, replicating the OpenShift service-ca-operator behavior.
- **DeploymentPlan**: an ordered pipeline of collection and release steps with automatic retry on failure.
- **ResourceCollection**: templated Kubernetes manifests with cert helper functions, rendered and applied by the collection reconciler.
