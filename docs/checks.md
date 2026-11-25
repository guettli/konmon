# Checks

## Core checks

Health findings can alert; hygiene findings are only recorded and printed. Disable any of them with `--disable-check`, or replace one with your own file of the same name. A check whose default is `off` is too noisy for most clusters and runs only with `--enable-check NAME` (or when `konmon check NAME` names it).

<!-- BEGIN generated checks table: make docs -->

| Check | Category | Severity | Default | Reports | Parameters (`--check-param`) | From |
|---|---|---|---|---|---|---|
| [`admission-policy-missing`](../checks/admission-policy-missing.ts) | health | warning | on | ValidatingAdmissionPolicyBindings whose policy does not exist |  | k8sgpt |
| [`conditions`](../checks/conditions.ts) | health | warning | on | unhealthy entries of status.conditions; a failed Job of a CronJob (and its Pod) only until a newer run succeeded | `podStartGraceSeconds=30` `rules` | check-conditions |
| [`configmap-key-missing`](../checks/configmap-key-missing.ts) | health | warning | on | Pods which reference a ConfigMap, or a key of it, which does not exist (and is not optional): the container cannot start |  | Popeye |
| [`container-memory-near-limit`](../checks/container-memory-near-limit.ts) | health | warning | on | containers whose memory use (working set) stays at a share of their memory limit (default 90%) for 15 minutes: at the limit the kernel OOM-kills them unless it can reclaim page cache | `percent=90` | Popeye |
| [`cronjob-never-succeeded`](../checks/cronjob-never-succeeded.ts) | health | warning | on | CronJobs which have run but never succeeded, while their newest Job failed |  | Popeye |
| [`deletion-timestamp`](../checks/deletion-timestamp.ts) | health | warning | on | objects which have been in deletion for too long: usually a finalizer whose controller is gone or failing | `olderThanSeconds=600` | check-conditions |
| [`gateway-references-missing`](../checks/gateway-references-missing.ts) | health | warning | on | Gateway API objects which reference something that does not exist: a Gateway's GatewayClass, an HTTPRoute's parent Gateway, or an HTTPRoute backend Service (or its port) |  | k8sgpt, Popeye |
| [`hpa-target-missing`](../checks/hpa-target-missing.ts) | health | warning | on | HorizontalPodAutoscalers whose scale target does not exist |  | k8sgpt, Popeye |
| [`ingress-backend-missing`](../checks/ingress-backend-missing.ts) | health | warning | on | Ingress backends which point to a Service (or a Service port) which does not exist, and load balancer errors in the Ingress status |  | k8sgpt, Popeye |
| [`ingress-class-missing`](../checks/ingress-class-missing.ts) | health | warning | on | Ingresses whose ingressClassName names an IngressClass which does not exist, or which name no class at all while there is no default class: no controller serves them |  | k8sgpt |
| [`pdb-blocks-drain`](../checks/pdb-blocks-drain.ts) | health | warning | on | PodDisruptionBudgets which make `kubectl drain` hang right now: the eviction API refuses (429) to evict one of their Pods, and drain retries forever | `node=` |  |
| [`pod-container-restarts`](../checks/pod-container-restarts.ts) | health | warning | on | Pod containers which restarted often and are unhealthy right now | `restarts=5` | check-conditions |
| [`pod-container-waiting`](../checks/pod-container-waiting.ts) | health | warning | on | containers which cannot start: image pull errors, bad configuration (a missing ConfigMap key, an invalid image name), failing hooks |  | k8sgpt |
| [`pod-multiple-pdbs`](../checks/pod-multiple-pdbs.ts) | health | warning | on | Pods selected by more than one PodDisruptionBudget: the eviction API refuses to evict them (500), so a node drain fails | `node=` | Popeye |
| [`pv-failed`](../checks/pv-failed.ts) | health | warning | on | PersistentVolumes in phase Failed: automatic reclamation failed |  | k8sgpt |
| [`pvc-almost-full`](../checks/pvc-almost-full.ts) | health | warning | on | PersistentVolumeClaims whose volume is almost full: the used space or the used inodes reach a threshold (default 90%) | `percent=90` `sharedFilesystems=false` |  |
| [`pvc-not-bound`](../checks/pvc-not-bound.ts) | health | warning | on | PersistentVolumeClaims which are Lost, or Pending for longer than a grace period | `pendingGraceSeconds=300` | k8sgpt |
| [`secret-missing`](../checks/secret-missing.ts) | health | warning | on | Pods which reference a Secret which does not exist (and is not optional): a container which starts (again) cannot get it, or the volume cannot be mounted |  | Popeye |
| [`service-target-port-missing`](../checks/service-target-port-missing.ts) | health | warning | on | Service ports whose named targetPort is not a port of any Pod the Service selects: the EndpointSlices list those Pods without that port, so traffic to it fails although the Pods are ready |  | Popeye |
| [`service-without-endpoints`](../checks/service-without-endpoints.ts) | health | warning | on | Services whose Pods exist but none is ready: traffic to them fails |  | k8sgpt |
| [`serviceaccount-missing`](../checks/serviceaccount-missing.ts) | health | warning | on | workloads whose Pods use a ServiceAccount which does not exist: the Pods cannot be created |  | Popeye |
| [`webhook-service-unavailable`](../checks/webhook-service-unavailable.ts) | health | critical | on | admission webhooks whose Service does not exist or has no ready endpoints |  | k8sgpt |
| [`workload-not-ready`](../checks/workload-not-ready.ts) | health | warning | on | StatefulSets and DaemonSets with fewer ready Pods than wanted |  | k8sgpt |
| [`admission-policy-warnings`](../checks/admission-policy-warnings.ts) | hygiene | info | on | CEL type-check warnings of ValidatingAdmissionPolicies: usually an expression which refers to a field that does not exist |  | k8sgpt |
| [`container-cpu-limit`](../checks/container-cpu-limit.ts) | hygiene | warning | on | containers with a CPU limit: the kernel throttles them (CFS quota) as soon as they use the limit within a scheduling period, even while the node has idle CPU | `allowGuaranteed=false` |  |
| [`container-cpu-request-missing`](../checks/container-cpu-request-missing.ts) | hygiene | warning | on | containers without a CPU request: the scheduler counts them as using no CPU, so a node can be packed with them, and under contention they get the smallest CPU share (2 of 1024) |  |  |
| [`container-image-tag`](../checks/container-image-tag.ts) | hygiene | info | on | container images without a tag or with the tag "latest": which version runs depends on when the node pulled it |  | Popeye |
| [`container-resources-missing`](../checks/container-resources-missing.ts) | hygiene | info | off | containers without any resource requests or limits: the scheduler cannot place them sensibly and they are evicted first |  | Popeye |
| [`container-resources-strange`](../checks/container-resources-strange.ts) | hygiene | info | on | resource requests and limits whose value is almost certainly a mistake: 1000 CPU cores, or 1m memory (a thousandth of a byte, instead of 1Mi) | `maxCpu=64` `maxMemory=1Ti` `minCpu=1m` `minMemory=4Mi` |  |
| [`orphaned`](../checks/orphaned.ts) | hygiene | info | off | resources which neither a controller nor a GitOps tool manages: created by hand (kubectl apply, a plain helm install, the Rancher UI) and not in git | `ignoreNames` `managerAnnotations` `managerLabels` `maxDebugPodAge=2h` `skipKinds` | korphan |
| [`pod-evicted`](../checks/pod-evicted.ts) | hygiene | info | on | evicted Pods (node pressure, preemption) |  | k8sgpt |
| [`pod-unmanaged`](../checks/pod-unmanaged.ts) | hygiene | info | on | Pods which no controller manages: they are not recreated when they or their node die |  | Popeye |
| [`pv-released`](../checks/pv-released.ts) | hygiene | info | on | PersistentVolumes which are Released (their claim was deleted, reclaim policy Retain) or Available (no claim): storage which is paid for and probably forgotten |  | k8sgpt, Popeye |
| [`rbac-binding-dangling`](../checks/rbac-binding-dangling.ts) | hygiene | warning | on | RoleBindings and ClusterRoleBindings which reference a Role or ClusterRole that does not exist |  | Popeye |
| [`selector-matches-no-pods`](../checks/selector-matches-no-pods.ts) | hygiene | warning | on | PodDisruptionBudgets and NetworkPolicies whose pod selector matches no Pod, and NetworkPolicy rules whose namespaceSelector matches no Namespace (the peer allows nothing): usually a typo or a leftover |  | k8sgpt, Popeye |
| [`statefulset-references-missing`](../checks/statefulset-references-missing.ts) | hygiene | warning | on | StatefulSets whose governing Service or the StorageClass of a volumeClaimTemplate does not exist |  | k8sgpt |
| [`storageclass-multiple-default`](../checks/storageclass-multiple-default.ts) | hygiene | warning | on | StorageClasses marked as default when there is more than one: which one a new claim gets is surprising |  | k8sgpt |
| [`suspended`](../checks/suspended.ts) | hygiene | info | on | suspended CronJobs and Jobs, and cordoned Nodes: often a manual action which was not undone |  | k8sgpt, Popeye |

<!-- END generated checks table -->

## Writing checks


A check is a TypeScript or JavaScript file. konmon bundles it with esbuild and runs it with [goja](https://github.com/dop251/goja), so no Node.js is needed.

```ts
import { defineCheck } from "konmon";

export default defineCheck({
  name: "ingress-backend-has-endpoints",
  category: "health",                         // "health" alerts, "hygiene" is report-only
  severity: "warning",                        // critical | warning | info
  // enabled: false,                          // off by default: needs --enable-check
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

- `check(obj, ctx)` runs for every object of the `match` types and returns findings: `message` (one line), and optionally `key` (to tell several findings of one object apart), `since` (konmon prints the age), `condition`, `container` and `details` (alert labels and annotations; objects and arrays are written as JSON).
- `ctx.get(resource, namespace, name)` and `ctx.list(resource, {namespace, labels, owner})` read konmon's cache, never the API server. A check may read the types in its `match` and `reads`. Reading a type which is not available throws, so a missing CRD never looks like "no objects"; use `ctx.has(resource)` for optional types. `ctx.resources()` lists the types of the cluster the check may read, including types which are not synced yet (e.g. to see which operators are installed); it returns the same frozen array for all calls of an evaluation, so a check can cache what it derives from it. With `konmon check -n` of a user who may only read some namespaces, a type can be listed in those namespaces only: `ctx.has` is true, but reading it in another namespace (or in all, or by an owner which is not in one of them) throws, and the object is reported as not checked instead of as a finding. A type whose objects `konmon run` does not know yet (its first list has not finished, e.g. a broken conversion webhook) is similar: `ctx.has` returns false, so the check reports what it can without the type, but the findings it reported before for the object stay open until the type is synced; reading the type throws.
- `forSeconds` in the definition overrides `--for` for a check whose findings are normal for a while (e.g. `workload-not-ready`: 10 minutes). A finding may set its own `severity`.
- `ctx.history.conditions(obj, since)` returns the recorded condition changes (needs `uses: ["history"]`), e.g. to report a condition which flaps.
- `ctx.containerUsage(pod)` returns the use of the containers of a Pod from the metrics API (metrics-server): `{time, containers: {NAME: {cpuCores, memoryBytes}}}`, `memoryBytes` being the working set, which counts for the memory limit. It is `null` if the metrics API has no measurement of the Pod (yet), or only one of an earlier Pod of the same name (needs `uses: ["container-usage"]`). `konmon run` reads it every `--usage-interval` (1m), together with the samples for `suggest-requests`; `konmon check` reads it once (with `-n` only in those namespaces; needs `list` on `pods.metrics.k8s.io`). If the use is not known, the call throws and the findings of the Pod stay as they are: before the first read, for a Pod without measurement while the metrics API cannot be read, and for usage which is out of date (the metrics API could not be read, or did not list the Pod, for 3 intervals plus a minute; a Pod it does not list for an hour gets `null`). Without metrics-server it is always `null`.
- `ctx.volumeStats(pvc)` returns the usage of the volume of a PersistentVolumeClaim (`usedBytes`, `capacityBytes`, `availableBytes`, `inodes`, `inodesUsed`, `inodesFree`, and `node`, `pod`, `volume`, `time` of the measurement), or `null` if no Pod mounts it (needs `uses: ["volume-stats"]`). konmon reads it from the summary API of every kubelet via the API server proxy (`get` on `nodes/proxy`, `list` on `nodes`), every `--volume-stats-interval` (1m); virtual kubelets are skipped (`--volume-stats-node-selector`). If the usage is not known, the call throws and the findings of the PVC stay as they are: before the first read, for a PVC without stats while some kubelet cannot be asked (it may be mounted there), and for stats which are out of date (not read for 3 intervals). For a `local` PV on a shared filesystem (e.g. local-path) the kubelet reports the whole filesystem, not the claim; for a `hostPath` PV it reports nothing. `pvc-almost-full` therefore skips volumes of local-path and OpenEBS LocalPV hostpath (`pv.kubernetes.io/provisioned-by`; `sharedFilesystems=true` checks them): their fill level is the node's.
- Resource names are `resource.group` (`deployments.apps`); a name without a dot is the core group (`pods`).
- Secrets are metadata only: a check gets `apiVersion`, `kind` and `metadata`, without `data`, `stringData`, `type` and the `kubectl.kubernetes.io/last-applied-configuration` annotation (which holds a copy of the data). konmon lists them with the metadata API, so the API server never sends `data`; the last-applied annotation does come with the metadata and is dropped before anything sees it. Other annotations are kept as they are. `"*"` does not include them: name `secrets` in `match` or `reads`. Test cases get Secrets stripped the same way.
- Objects are read-only views of konmon's cache (writes throw). Each property access returns a new view, so compare objects by `metadata.uid`, not with `===`. `ctx` is frozen and fresh for every call, but this is not isolation: module variables and globals are shared between calls, so don't keep state in them.
- `parseTime(s)` from `konmon` parses RFC3339 timestamps strictly; `new Date(s)` also accepts timestamps without a zone and reads them as local time.
- `Date` and `Math.random` are deterministic within an evaluation (`ctx.now`). There is no network or filesystem access, and the only import is `konmon`. A call which runs longer than `--check-timeout` fails.
- A check which throws keeps its previous findings for that object open, so `--for` does not start again, and opens one `check-error` finding per check ("failed for N objects, e.g. …").

A type in `reads` is kept in memory for the whole cluster: `configmap-key-missing` makes konmon watch all ConfigMaps (not stored in the database). Disable it on clusters with many large ConfigMaps. `secret-missing` likewise makes konmon watch the metadata of all Secrets (never their data); without `list` on secrets it is skipped.

Use your checks with `--checks DIR` (repeatable). `--disable-check NAME` (globs) turns checks off, `enabled: false` in the definition makes a check off by default so that only `--enable-check NAME` runs it, `--check-param check.param=value` changes parameters, and `konmon checks list` shows what is enabled (`--all` also the rest, with their state).

**Types:** `konmon checks types > konmon.d.ts` and a `tsconfig.json` like the one in [checks/](../checks/) give you completion and type errors in your editor.

**Tests:** put `NAME.test.yaml` next to a check. Each case has an object, optionally related objects, history, volume stats (`volumeStats`, for the object unless `namespace`/`pvc` are given; without it they are not collected yet, `[]` means not mounted; `stale` and `volumeStatsIncomplete` for the cases above), container usage (`containerUsage`, by container name, for the object; without it it is not collected yet, `{}` means not measured; `containerUsageTime`, `containerUsageStale`, `containerUsageIncomplete`), params and the time, and the expected findings (only the fields given are compared; `severity` is the effective one). `konmon checks test --checks DIR` runs them without a cluster. The core checks in [checks/](../checks/) are examples.

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

