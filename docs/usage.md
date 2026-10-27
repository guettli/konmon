# Using konmon

```console
# Watch the cluster of the current kubeconfig context.
konmon run --db konmon.db

# Send alerts, with a cluster label.
konmon run --db konmon.db --cluster-name prod --alertmanager-url http://alertmanager:9093

# Ad hoc, like check-conditions: list everything once and print findings,
# health first, hygiene (never alerts) below.
# Exit code 1: health findings; 2: some types could not be listed.
konmon check --context prod
konmon check -n 'my-app-*' --category health   # one app, only what is broken
konmon check conditions 'pod-*'                 # only some checks (and the types they need)
konmon check -o json | jq '.findings[] | select(.severity == "critical")'

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
  2026-10-24 15:49:19  finding opened:   flux-system kustomizations infra Condition Ready=Unknown Progressing "Reconciliation in progress" (9s)
  2026-10-24 15:49:20  condition Ready=True ReconciliationSucceeded "Applied revision: released@sha1:ad79e3d…" [lastTransitionTime 2026-10-24 15:49:20]
  2026-10-24 15:49:29  finding resolved: flux-system kustomizations infra Condition Ready=Unknown Progressing "Reconciliation in progress" (19s)
```

