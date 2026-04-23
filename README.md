# k8s-active-image-tracker

Golang service. Scans Kubernetes pods. Finds image refs. Writes namespace-scoped tracking tag back to source repository.

Example:

- pod image: `ghcr.io/acme/api:1.4.2`
- namespace: `payments`
- tag prefix: `active`
- tracker writes: `ghcr.io/acme/api:active-payments`

Use cases:

- mark image currently referenced by active pods in namespace
- integrate deploy visibility with registry tooling
- filter by namespace and registry

## Behavior

- watches Kubernetes pods with controller-runtime manager/controller
- reads images from init, regular, ephemeral containers
- tracks only active pods: not deleting, not in `Succeeded`, not in `Failed`
- filters by namespace allow list and registry allow list
- de-duplicates same destination tag
- skips destination when multiple different source refs would fight for same `<prefix>-<namespace>` tag in same repository
- exposes `/healthz` and `/readyz` in watch mode for Kubernetes probes
- returns non-zero / reconcile error when registry syncs fail
- uses [`go-containerregistry/pkg/crane`](https://github.com/google/go-containerregistry/tree/main/pkg/crane) for registry operations
- uses Docker/keychain auth supported by `go-containerregistry`

## Build

```bash
go build ./cmd/k8s-active-image-tracker
```

Container image:

```bash
docker build -t ghcr.io/your-org/k8s-active-image-tracker:latest .
```

## Run

From local kubeconfig:

```bash
go run ./cmd/k8s-active-image-tracker \
  --namespaces payments,checkout \
  --registries ghcr.io,123456789012.dkr.ecr.us-east-1.amazonaws.com \
  --tag-prefix active
```

One-shot dry run:

```bash
go run ./cmd/k8s-active-image-tracker \
  --once \
  --dry-run \
  --namespaces payments \
  --registries ghcr.io
```

In cluster, leave `--kubeconfig` empty. Service tries in-cluster config first, then `~/.kube/config`.

## Helm chart

Chart path: `./helm`

Install:

```bash
helm install active-image-tracker ./helm \
  --namespace ops \
  --create-namespace \
  --set image.repository=ghcr.io/your-org/k8s-active-image-tracker \
  --set image.tag=latest \
  --set tracker.tagPrefix=active \
  --set tracker.registries[0]=ghcr.io \
  --set tracker.namespaces[0]=payments \
  --set tracker.namespaces[1]=checkout
```

Chart includes only resources needed to run in cluster:
- `Deployment`
- `ServiceAccount`
- `ClusterRole`
- `ClusterRoleBinding`

Health probes:
- liveness: `GET /healthz`
- readiness: `GET /readyz`

## Flags

- `--kubeconfig`: kubeconfig path. Empty = try in-cluster first, then `~/.kube/config`
- `--namespaces`: comma-separated namespace allow list. Empty = all
- `--registries`: comma-separated registry allow list. Empty = all
- `--registry`: alias for single registry value
- `--tag-prefix`: destination tag prefix. Final tag = `<prefix>-<namespace>`. Default `active`.
- `--workers`: concurrent registry sync workers. Default `4`
- `--health-probe-bind-address`: health probe bind address for `/healthz` and `/readyz`. Default `:8081`. Set `0` to disable.
- `--dry-run`: log only, no registry writes
- `--once`: single sync then exit
- `--verbose`: debug logs

## Notes

Registry filter matches normalized registry host names from image refs. Example: `docker.io` normalizes to `index.docker.io`.

Name means source image chosen from active, non-terminal pods tracker can currently observe. Tracker is not pod health monitor.

Tracker does not delete or roll back destination tags when pods disappear, become unobservable, or fall outside filters. Existing tag remains until newer observed active state overwrites same destination.

If same repository in same namespace appears with multiple different image refs at once, tracker logs conflict and skips tag update for that destination. Prevents tag flapping.

Default mode is event-driven watch. Tracker syncs after controller-runtime cache warmup, then on relevant pod add/update/delete events, including phase and deletion-state changes.

`/readyz` stays failing until initial cache sync and first registry sync complete. `/healthz` uses controller-runtime ping check.
