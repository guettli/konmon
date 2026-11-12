# Using konmon

```console
# Watch the cluster of the current kubeconfig context.
konmon run --db konmon.db

# Send alerts, with a cluster label.
konmon run --db konmon.db --cluster-name prod --alertmanager-url http://alertmanager:9093

# Ad hoc, like check-conditions: list everything once and print findings,
# health first, hygiene (never alerts) below.
# Exit code 1: health findings; 2: some types could not be listed;
# 3: konmon could not check.
konmon check --context prod
konmon check -n 'my-app-*' --category health   # one app, only what is broken
konmon check conditions 'pod-*'                 # only some checks (and the types they need)
konmon check -o json | jq '.findings[] | select(.severity == "critical")'

# Again and again, like check-conditions forever / while.
konmon check --repeat --interval 30s
konmon check --while StillProvisioning && music   # until no health finding matches
konmon check --while . --timeout 15m              # until healthy; exit 3 on timeout

# Before draining a node: would a PodDisruptionBudget make the drain hang?
# Also Pods on NODE selected by two budgets (they cannot be evicted at all).
konmon check pdb-blocks-drain pod-multiple-pdbs \
    --check-param pdb-blocks-drain.node=NODE --check-param pod-multiple-pdbs.node=NODE \
  && kubectl drain NODE --ignore-daemonsets --delete-emptydir-data
# Wait until no budget blocks (e.g. Pods still starting after an update).
konmon check pdb-blocks-drain --check-param pdb-blocks-drain.node=NODE --while . --timeout 10m \
  && kubectl drain NODE --ignore-daemonsets --delete-emptydir-data

# What is wrong now? What was wrong two hours ago?
konmon findings --db konmon.db
konmon findings --db konmon.db --at 2h
konmon findings --db konmon.db --at 2026-10-24T03:00:00Z
konmon findings --db konmon.db -n my-app --category health -o json

# What happened to one object?
konmon history --db konmon.db -n kube-system pods/coredns-6799fbcd5-abcde
konmon history --db konmon.db machines.cluster.x-k8s.io/my-machine --since 24h
```

`findings` and `history` read the database while `konmon run` writes to it (SQLite WAL mode); they never create a database, so a wrong `--db` is an error, not an empty history. Use `sqlite3` for anything else. The JSON functions of SQLite work on the `object` column:

```sql
SELECT namespace, name, json_extract(object, '$.status.phase')
FROM objects WHERE resource = 'pods' AND deleted_at IS NULL;
```

Times are stored as Unix milliseconds.

Example `history` output:

```
kustomizations.kustomize.toolkit.fluxcd.io flux-system/infra uid=10cb7e5c-… first seen 2026-10-24 15:48:41
  2026-10-24 15:49:11  condition Ready=Unknown Progressing "Reconciliation in progress" [lastTransitionTime 2026-10-24 15:49:10]
  2026-10-24 15:49:19  finding opened:   flux-system kustomizations infra Condition Ready=Unknown Progressing "Reconciliation in progress" (9s) [conditions]
  2026-10-24 15:49:20  condition Ready=True ReconciliationSucceeded "Applied revision: released@sha1:ad79e3d…" [lastTransitionTime 2026-10-24 15:49:20]
  2026-10-24 15:49:29  finding resolved: flux-system kustomizations infra Condition Ready=Unknown Progressing "Reconciliation in progress" (19s) [conditions]
```



## Suggesting resource requests

`konmon run` samples the CPU and memory use of every container from the metrics API (`metrics.k8s.io`, served by metrics-server; k3s ships it) once a minute (`--usage-interval`, 0 disables it). It keeps one row per workload, container and hour: the number of samples, the CPU sum and maximum, and the memory maximum. Rows are pruned with `--retention`. Pods count for their workload: a ReplicaSet's for its Deployment (or Argo Rollout), a CronJob's Jobs for the CronJob. Namespaces excluded with `--exclude-namespace` are not sampled.

`konmon suggest-requests` turns that into requests:

```console
$ konmon suggest-requests --db konmon.db -n monitoring
NAMESPACE   WORKLOAD                     CONTAINER         DATA  CPU REQ  CPU P  CPU MAX  -> CPU  MEM REQ  MEM PEAK  -> MEM
monitoring  Deployment/victoria-metrics  victoria-metrics  168h  100m     40m    53m      40m     256Mi    561Mi     645Mi
monitoring  Deployment/reloader          reloader          168h  -        1m     2m       5m      -        28Mi      32Mi
```

- **CPU request** = the 95th percentile (`--cpu-percentile`) of the hourly average CPU use, rounded up to 5m. The rare peak (CPU MAX) is what the unused CPU of the node is for; a CPU limit is never suggested (see the check `container-cpu-limit`).
- **Memory request** = the highest memory use seen (working set) plus 15% (`--memory-headroom`), rounded up to 1Mi. Memory cannot be borrowed back like CPU, so the peak counts.
- **DATA** is the number of hours with samples. Workloads with less than `--min-hours` (24) get no suggestion. Use a window (`--since`, default `168h`) which contains the real load: a week covers the weekly pattern, a nightly job needs nights.
- `-o yaml` prints a `resources:` snippet per container, `-o json` everything.
- `--live 30m` samples the cluster itself for that long instead of reading a database (`--live-interval`, default 15s; one bucket per minute): for a cluster where `konmon run` does not run. Short windows miss daily peaks. Ctrl-C ends sampling early and still prints the suggestions.
- Jobs and CronJobs have data only while they run: for them `--min-hours` applies to the span from the first to the last run.

## Coming from check-conditions


konmon contains all checks of [check-conditions](https://github.com/guettli/check-conditions): the `conditions` check with its polarity tables and ignore list, `deletion-timestamp` and `pod-container-restarts`. Run `konmon check` where you ran `check-conditions all`:

| check-conditions | konmon |
|---|---|
| `all` | `check` |
| `forever` | `check --repeat` |
| `while REGEX` | `check --while REGEX` |
| `-s/--sleep 15s` | `--interval 15s` |
| `-t/--timeout 5m` | `--timeout 5m` (exit code 3, check-conditions: 3) |
| `--name` | `--name` |
| `-n`, `--exclude-namespace` | same (glob patterns, comma-separated) |
| `--pod-start-grace 1m` | `--check-param conditions.podStartGraceSeconds=60` |
| `--pod-restart-warn-count 20` | `--check-param pod-container-restarts.restarts=20` (0: `--disable-check pod-container-restarts`) |
| `--warn-deletion-older-than 1h` | `--check-param deletion-timestamp.olderThanSeconds=3600` (0: `--disable-check deletion-timestamp`) |
| `--ignore-condition-regex 'pods X=False Y .*'` | `--ignore-finding-regex 'conditions pods \S+ Condition (\S+/)?X(/\S+)?=False Y'`, or better a rule in the [config file](config.md) |
| `--ignore-condition-younger-than`, `--ignore-condition-older-than` | `--ignore-younger-than`, `--ignore-older-than` (all findings with a start time, not only conditions) |
| `--retry-count 5` | `--retry-count 5`: network errors before the first successful run; later runs of `--repeat` and `--while` retry forever |
| `-v/--verbose` | `--log-level debug` |
| `logs` | not ported: use `kubectl logs` |

`--ignore-finding-regex` is matched against `check resource namespace/name message`, for example `conditions pods default/web-1 Condition Ready=False PodFailed "…"`. Condition types with the same status, reason and message are one finding (`Condition Failed/FailureTarget=True …`), so a regex for one type has to allow the others, and it hides the whole finding. `--while` tries each type on its own too, so `--while Failed=True` matches that finding. The age of a finding of several types is the age of the oldest.

Differences:

- `--while` ends only after a run which could list everything: during an API server restart it goes on.
- `-n` filters the findings; konmon still lists all namespaces, so a check can see objects in other namespaces (a Gateway of an HTTPRoute). If you may only read some namespaces (a Role, no ClusterRole), `konmon check -n my-ns` lists each type which it may not list in all namespaces in `my-ns` only, like check-conditions (namespace names, not glob patterns; a namespace of `-n` which you may not read is named and skipped). A check which then needs objects of another namespace (the Gateway of an HTTPRoute in `gateway-system`) cannot check that object: it is listed under "Not checked (not allowed to list)", and is neither a finding nor changes the exit code.
- A false positive is fixed in a [config file](config.md) (`konmon triage`), not with a regex.
- konmon has more checks: `konmon checks list`. `konmon check conditions deletion-timestamp pod-container-restarts` runs only the ones of check-conditions.
