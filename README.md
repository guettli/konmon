# konmon: Kubernetes Condition Monitor

konmon watches **all** resources of a Kubernetes cluster (including every CRD), keeps a local copy with history in SQLite, checks `status.conditions` with the rules of [check-conditions](https://github.com/guettli/check-conditions), and sends alerts to Alertmanager.

Conditions have one big advantage over logs and events: they show the current state, and a warning disappears as soon as the desired state is reached. But `status.conditions` only shows *now*. konmon records every change, so you can answer "what was wrong at 03:00?" and "when did this machine become not ready?".

## How it works

```
             watch (dynamic informers, all listable+watchable types)
 API server ───────────────────────────────────────────────────────▶ konmon run
                                                                    │
     ┌──────────────────────────────────────────────────────────────┤
     ▼                                                              ▼
 SQLite (konmon.db)                                         every --eval-interval:
   objects            latest version of every object        check-conditions rules
   revisions          gzip copy of every real change          → findings table
   conditions         current conditions per object           → Alertmanager
   condition_history  one row per condition change
   findings           one row per period a problem was open
```

- **Change detection ignores noise:** `resourceVersion`, `managedFields` and condition heartbeats (`lastHeartbeatTime`, `lastProbeTime`, `lastUpdateTime`) are not real changes. A revision or history row is only written if something else changed.
- **Types without conditions are not watched:** secrets, configmaps, events and the other types check-conditions skips. Leases, EndpointSlices and `events.k8s.io` are excluded too, since they change all the time (`--exclude-resource`).
- **Deletions while konmon was down** are detected after the initial list of each type.
- **New CRDs** are picked up by re-running discovery (`--discovery-interval`).
- **Findings work like Prometheus alerts:** a finding is *pending* until it has been open for `--for`, then it *fires*. Firing alerts are re-sent on every evaluation; resolved alerts are sent for `--resend-resolved`.

## Install

```console
go install github.com/guettli/konmon@latest
```

Binaries for Linux, macOS and Windows are attached to every [release](https://github.com/guettli/konmon/releases), and every release is pushed as `ghcr.io/guettli/konmon:<version>` (and `:latest`).

## Usage

```console
# Watch the cluster of the current kubeconfig context.
konmon run --db konmon.db

# Send alerts, with a cluster label.
konmon run --db konmon.db --cluster-name prod --alertmanager-url http://alertmanager:9093

# What is wrong now? What was wrong two hours ago?
konmon findings --db konmon.db
konmon findings --db konmon.db --at 2h
konmon findings --db konmon.db --at 2026-10-24T03:00:00Z

# What happened to one object?
konmon history --db konmon.db -n kube-system pods/coredns-6799fbcd5-abcde
konmon history --db konmon.db machines.cluster.x-k8s.io/my-machine --since 24h
```

`findings` and `history` read the database while `konmon run` writes to it (SQLite WAL mode). Use `sqlite3` for anything else. The JSON functions of SQLite work on the `object` column:

```sql
SELECT namespace, name, json_extract(object, '$.status.phase')
FROM objects WHERE resource = 'pods' AND deleted_at IS NULL;
```

Times are stored as Unix milliseconds.

Example `history` output:

```
kustomizations.kustomize.toolkit.fluxcd.io flux-system/infra uid=10cb7e5c-… first seen 2026-10-24 15:48:41
  2026-10-24 15:49:11  condition Ready=Unknown Progressing "Reconciliation in progress" [lastTransitionTime 2026-10-24 15:49:10]
  2026-10-24 15:49:19  finding opened:   flux-system kustomizations infra Condition Ready=Unknown Progressing "Reconciliation in progress" (9s)
  2026-10-24 15:49:20  condition Ready=True ReconciliationSucceeded "Applied revision: released@sha1:ad79e3d…" [lastTransitionTime 2026-10-24 15:49:20]
  2026-10-24 15:49:29  finding resolved: flux-system kustomizations infra Condition Ready=Unknown Progressing "Reconciliation in progress" (19s)
```

### Important flags of `konmon run`

| Flag | Default | |
|---|---|---|
| `--db` | `konmon.db` | SQLite database |
| `--context` | current | kubeconfig context; in a Pod the in-cluster config is used |
| `--for` | `2m` | how long a finding must be open before it fires |
| `--eval-interval` | `30s` | time between two evaluations |
| `--retention` | `720h` | history to keep (revisions, condition changes, resolved findings, deleted objects); `0` keeps everything |
| `--revisions` / `--no-revisions-for` | `true` / none | keep a gzip copy of every change, per resource type |
| `--exclude-namespace` | none | globs, not stored and not checked |
| `--exclude-resource` | leases, endpointslices, events.k8s.io | `resource.group` globs |
| `--alertmanager-url` | none | repeatable; without it konmon only records |
| `--alertmanager-watchdog` | `false` | always-firing `KonmonWatchdog` alert for a dead man's switch |
| `--cluster-name` | none | `cluster` label on all alerts |
| `--listen` | `:8080` | `/healthz` |

The check-conditions rules use the same flags and defaults as check-conditions: `--warn-deletion-older-than`, `--pod-start-grace`, `--pod-restart-warn-count` and `--ignore-condition-regex`.

### Alerts

| Label | Value |
|---|---|
| `alertname` | `KonmonCondition`, `KonmonDeletionTimestamp`, `KonmonContainerRestarts`, `KonmonError` |
| `severity` | `warning` |
| `cluster` | `--cluster-name` |
| `group`, `resource`, `namespace`, `name` | the object |
| `condition` | condition type(s), e.g. `Ready` or `Failed/FailureTarget` |
| `container` | for `KonmonContainerRestarts` |

Annotations: `summary` (the check-conditions output line), `status`, `reason`, `message`, `detail`, `lastTransitionTime`.

## Running in a cluster

konmon needs `get`, `list` and `watch` on everything it should monitor, and a volume for the database:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: konmon
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]
```

Secrets are never watched, but this role can read them. If that is too much, list the resources explicitly: konmon skips types it may not list and logs them once.

Run exactly one replica (one writer per database), with a PersistentVolume mounted at `/data` and a liveness probe on `/healthz`.

## Memory and disk

The informers keep all watched objects in memory, without `managedFields`. A first test on a small cluster (1,264 objects, 138 types) produced a 10 MB database, of which 2 MB were compressed revisions. Most of the size is the latest version of every object. Revisions grow with the change rate of the cluster. Use `--no-revisions-for` for types that change often but whose history you don't need.

## Development

```console
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
```

CI runs tests and lint on every push and pull request. After CI succeeds on `main`, the release workflow bumps the patch version, creates a GitHub release with binaries and pushes the image.

## License

Apache License 2.0
