# konmon: Kubernetes Condition Monitor

konmon watches **all** resources of a Kubernetes cluster (including every CRD), keeps a local copy with history in SQLite, runs **checks written in TypeScript or JavaScript** on every object, and sends alerts to Alertmanager.

The core checks (unhealthy `status.conditions`, objects stuck in deletion, crash-looping containers; ported from [check-conditions](https://github.com/guettli/check-conditions)) are ordinary check files embedded in the binary. Your own checks work exactly the same way: no recompiling, and a check of yours with the name of a core check replaces it.

Conditions have one big advantage over logs and events: they show the current state, and a warning disappears as soon as the desired state is reached. But `status.conditions` only shows *now*. konmon records every change, so you can answer "what was wrong at 03:00?" and "when did this machine become not ready?".

## How it works

```
             watch (dynamic informers, the types the checks need)
 API server ───────────────────────────────────────────────────────▶ konmon run
                                                                    │
     ┌──────────────────────────────────────────────────────────────┤
     ▼                                                              ▼
 SQLite (konmon.db)                                         every --eval-interval:
   objects            latest version of every object        checks (TS/JS, goja)
   revisions          gzip copy of every real change          → findings table
   conditions         current conditions per object           → Alertmanager
   condition_history  one row per condition change
   findings           one row per period a problem was open
```

- **Change detection ignores noise:** `resourceVersion`, `managedFields` and condition heartbeats (`lastHeartbeatTime`, `lastProbeTime`, `lastUpdateTime`) are not real changes. A revision or history row is only written if something else changed.
- **Only the types the checks need are watched.** A type a check runs on (`match`) is stored; a type a check only reads (`reads`) stays in memory. Secrets are never read. Leases, EndpointSlices and `events.k8s.io` are excluded by default since they change all the time (`--exclude-resource`); a check can still read them.
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

# Ad hoc, like check-conditions: list everything once, print findings, exit 1 if any.
konmon check --context prod

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

## Checks

A check is a TypeScript or JavaScript file. konmon bundles it with esbuild and runs it with [goja](https://github.com/dop251/goja), so no Node.js is needed.

```ts
import { defineCheck } from "konmon";

export default defineCheck({
  name: "ingress-backend-has-endpoints",
  category: "health",                         // "health" alerts, "hygiene" is report-only
  severity: "warning",                        // critical | warning | info
  match: { resources: ["ingresses.networking.k8s.io"] },
  reads: ["endpointslices.discovery.k8s.io"], // other types this check reads
  params: { minEndpoints: 1 },                // --check-param ingress-backend-has-endpoints.minEndpoints=2
  check(ingress, ctx) {
    const findings = [];
    for (const rule of ingress.spec?.rules ?? []) {
      for (const p of rule.http?.paths ?? []) {
        const svc = p.backend?.service?.name;
        if (!svc) continue;
        const slices = ctx.list("endpointslices.discovery.k8s.io", {
          namespace: ingress.metadata.namespace,
          labels: { "kubernetes.io/service-name": svc },
        });
        const ready = slices.flatMap((s) => s.endpoints ?? []).filter((e) => e.conditions?.ready).length;
        if (ready < ctx.params.minEndpoints) {
          findings.push({ key: svc, message: `backend ${svc} has ${ready} ready endpoints` });
        }
      }
    }
    return findings;
  },
});
```

- `check(obj, ctx)` runs for every object of the `match` types and returns findings: `message` (one line), and optionally `key` (to tell several findings of one object apart), `since` (konmon prints the age), `condition`, `container` and `details` (alert labels and annotations).
- `ctx.get(resource, namespace, name)` and `ctx.list(resource, {namespace, labels, owner})` read konmon's cache, never the API server. A check may read the types in its `match` and `reads`. Reading a type which is not available throws, so a missing CRD never looks like "no objects"; use `ctx.has(resource)` for optional types.
- `ctx.history.conditions(obj, since)` returns the recorded condition changes (needs `uses: ["history"]`), e.g. to report a condition which flaps.
- Resource names are `resource.group` (`deployments.apps`); a name without a dot is the core group (`pods`).
- Objects are read-only. `Date` and `Math.random` are deterministic within an evaluation (`ctx.now`). There is no network or filesystem access, and the only import is `konmon`. A call which runs longer than `--check-timeout` fails.
- A check which throws for an object opens a `check-error` finding; its previous findings for that object stay open, so `--for` does not start again.

Use your checks with `--checks DIR` (repeatable). `--disable-check NAME` (globs) turns checks off, `--check-param check.param=value` changes parameters, and `konmon checks list` shows what is enabled.

**Types:** `konmon checks types > konmon.d.ts` and a `tsconfig.json` like the one in [checks/](checks/) give you completion and type errors in your editor.

**Tests:** put `NAME.test.yaml` next to a check. Each case has an object, optionally related objects, history, params and the time, and the expected findings (only the fields given are compared). `konmon checks test --checks DIR` runs them without a cluster. The core checks in [checks/](checks/) are examples.

```yaml
check: pod-container-restarts
cases:
  - name: crash loop
    resource: pods
    object:
      metadata: {name: p, namespace: default}
      status:
        containerStatuses:
          - {name: app, restartCount: 7, ready: false, state: {waiting: {reason: CrashLoopBackOff}}}
    expect:
      - {container: app, message: "container app restarted 7 times, currently CrashLoopBackOff"}
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
| `--exclude-resource` | leases, endpointslices, events.k8s.io | `resource.group` globs; not checked or stored, but checks may read them |
| `--checks` | none | directory with own checks, repeatable |
| `--disable-check` | none | check names, globs |
| `--check-param` | none | `check.param=value`, repeatable |
| `--ignore-finding-regex` | none | drop findings; matched against `check resource namespace/name message` |
| `--alertmanager-url` | none | repeatable; without it konmon only records |
| `--alertmanager-watchdog` | `false` | always-firing `KonmonWatchdog` alert for a dead man's switch |
| `--cluster-name` | none | `cluster` label on all alerts |
| `--listen` | `:8080` | `/healthz` |

The parameters of the core checks: `conditions.podStartGraceSeconds` (30), `deletion-timestamp.olderThanSeconds` (600) and `pod-container-restarts.restarts` (5).

### Alerts

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

konmon never reads Secrets, but this role allows it. If that is too much, list the resources explicitly: konmon skips types it may not list and logs them once.

Run exactly one replica (one writer per database), with a PersistentVolume mounted at `/data` and a liveness probe on `/healthz`.

## Memory and disk

The informers keep all watched objects in memory, without `managedFields`. A first test on a small cluster (1,264 objects, 138 types) produced a 10 MB database, of which 2 MB were compressed revisions. Most of the size is the latest version of every object. Revisions grow with the change rate of the cluster. Use `--no-revisions-for` for types that change often but whose history you don't need.

## Development

```console
go test -race ./...
go run . checks test
(cd checks && npx -p typescript@5.9.3 tsc -p .)
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
```

The core checks are a port of check-conditions. `internal/checks/corpus_test.go` compares both on the same objects; with `KONMON_CORPUS_DB=konmon.db` it uses the objects of a real database. On three clusters (about 2,000 objects) the results are identical. A full evaluation of 900 objects takes about 8 ms.

CI runs tests and lint on every push and pull request. After CI succeeds on `main`, the release workflow bumps the patch version, creates a GitHub release with binaries and pushes the image.

## License

Apache License 2.0
