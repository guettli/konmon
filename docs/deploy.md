# Running konmon in a cluster

[deploy/](../deploy/) has everything konmon needs, in the namespace `konmon`:

To try it: `kubectl apply -k 'github.com/guettli/konmon/deploy?ref=main'`.

- a ServiceAccount with `get`, `list` and `watch` on everything (a ClusterRole). konmon never reads Secrets, but RBAC cannot exclude them from `*`; if that is too much, list the resources explicitly: konmon skips types it may not list and logs them once;
- a PersistentVolumeClaim for the database (`/data`, 2Gi) of the default StorageClass: without a default StorageClass, set `storageClassName`, or the Pod stays Pending;
- a ConfigMap `konmon-config` with the [config file](config.md) (`/config/konmon.yaml`). konmon reads it at startup: restart it after a change, or let [Reloader](https://github.com/stakater/Reloader) do it (`reloader.stakater.com/auto: "true"` on the Deployment);
- a Deployment with exactly one replica (one writer per database), non-root, read-only root filesystem, and a liveness probe on `/healthz` (it fails when evaluations stop).

For real use, pin a [release](https://github.com/guettli/konmon/releases) (the manifests and the image) and set the cluster name and Alertmanager in your own `kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - github.com/guettli/konmon/deploy?ref=vX.Y.Z # a release which contains deploy/
images:
  - name: ghcr.io/guettli/konmon
    newTag: vX.Y.Z
patches:
  - target:
      kind: Deployment
      name: konmon
    patch: |
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --cluster-name=prod
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --alertmanager-url=http://alertmanager.monitoring.svc:9093
configMapGenerator:
  - name: konmon-config
    namespace: konmon
    behavior: replace
    files:
      - konmon.yaml # written with konmon triage
```

CI deploys these manifests on a kind cluster for every change ([hack/e2e.sh](../hack/e2e.sh)).

To look at the history, run the commands inside the Pod (the image is distroless, `/konmon` is the only binary):

```console
kubectl -n konmon exec deploy/konmon -- /konmon findings --db /data/konmon.db --at 3h
kubectl -n konmon exec deploy/konmon -- /konmon history --db /data/konmon.db -n kube-system pods/coredns-6799fbcd5-abcde
```

Triage on your machine, with the config you deployed, and put the result back into the ConfigMap: `konmon triage --context prod --config konmon.yaml`.

## Important flags of `konmon run`

| Flag | Default | |
|---|---|---|
| `--db` | `konmon.db` | SQLite database |
| `--context` | current | kubeconfig context; in a Pod the in-cluster config is used |
| `--for` | `2m` | how long a finding must be open before it fires |
| `--eval-interval` | `30s` | time between two evaluations |
| `--retention` | `720h` | history to keep (revisions, condition changes, resolved findings, deleted objects); `0` keeps everything |
| `--revisions` / `--no-revisions-for` | `true` / none | keep a gzip copy of every change, per resource type |
| `--exclude-namespace` | none | globs, not stored and not checked |
| `--exclude-resource` | leases, endpointslices, events.k8s.io | `resource.group` globs; not checked or stored, but checks may read them |
| `--checks` | none | directory with own checks, repeatable |
| `--disable-check` | none | check names, globs |
| `--check-param` | none | `check.param=value`, repeatable |
| `--config` | none | config file with ignore rules (below), repeatable |
| `--ignore-finding-regex` | none | drop findings; matched against `check resource namespace/name message`. Prefer `--config` |
| `--alertmanager-url` | none | repeatable; without it konmon only records |
| `--alertmanager-watchdog` | `false` | always-firing `KonmonWatchdog` alert for a dead man's switch |
| `--cluster-name` | none | `cluster` label on all alerts |
| `--listen` | `:8080` | `/healthz` |

Parameters of the checks: see [Checks](checks.md#core-checks).

## Alerts

| Label | Value |
|---|---|
| `alertname` | `KonmonFinding` |
| `check` | the check name, e.g. `conditions`; `check-error` if a check failed |
| `severity` | from the check |
| `category` | `health` (only health findings are sent) |
| `cluster` | `--cluster-name` |
| `group`, `resource`, `namespace`, `name` | the object |
| `condition` | condition type(s), e.g. `Ready` or `Failed/FailureTarget` |
| `container` | e.g. for `pod-container-restarts` |
| `key` | the finding key, if there is no condition or container |

Annotations: `summary` (the output line), `since`, and the finding's `details` (for `conditions`: `status`, `reason`, `message`).

## Memory and disk

The informers keep all watched objects in memory, without `managedFields`. A first test on a small cluster (1,264 objects, 138 types) produced a 10 MB database, of which 2 MB were compressed revisions. Most of the size is the latest version of every object. Revisions grow with the change rate of the cluster. Use `--no-revisions-for` for types that change often but whose history you don't need.

