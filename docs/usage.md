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
- `-n` filters the findings; konmon still lists all namespaces, so a check can see objects in other namespaces (a Gateway of an HTTPRoute).
- A false positive is fixed in a [config file](config.md) (`konmon triage`), not with a regex.
- konmon has more checks: `konmon checks list`. `konmon check conditions deletion-timestamp pod-container-restarts` runs only the ones of check-conditions.
