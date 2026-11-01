# Checks

## Core checks

Health findings can alert; hygiene findings are only recorded and printed. Disable any of them with `--disable-check`, or replace one with your own file of the same name.

<!-- BEGIN generated checks table: make docs -->

| Check | Category | Severity | Reports | Parameters (`--check-param`) | From |
|---|---|---|---|---|---|
| [`admission-policy-missing`](../checks/admission-policy-missing.ts) | health | warning | ValidatingAdmissionPolicyBindings whose policy does not exist |  | k8sgpt |
| [`conditions`](../checks/conditions.ts) | health | warning | unhealthy entries of status.conditions | `podStartGraceSeconds=30` `rules` | check-conditions |
| [`configmap-key-missing`](../checks/configmap-key-missing.ts) | health | warning | Pods which reference a ConfigMap, or a key of it, which does not exist (and is not optional): the container cannot start |  | Popeye |
| [`cronjob-never-succeeded`](../checks/cronjob-never-succeeded.ts) | health | warning | CronJobs which have run but never succeeded, while their newest Job failed |  | Popeye |
| [`deletion-timestamp`](../checks/deletion-timestamp.ts) | health | warning | objects which have been in deletion for too long: usually a finalizer whose controller is gone or failing | `olderThanSeconds=600` | check-conditions |
| [`gateway-references-missing`](../checks/gateway-references-missing.ts) | health | warning | Gateway API objects which reference something that does not exist: a Gateway's GatewayClass, an HTTPRoute's parent Gateway, or an HTTPRoute backend Service (or its port) |  | k8sgpt, Popeye |
| [`hpa-target-missing`](../checks/hpa-target-missing.ts) | health | warning | HorizontalPodAutoscalers whose scale target does not exist |  | k8sgpt, Popeye |
| [`ingress-backend-missing`](../checks/ingress-backend-missing.ts) | health | warning | Ingress backends which point to a Service (or a Service port) which does not exist, and load balancer errors in the Ingress status |  | k8sgpt, Popeye |
| [`ingress-class-missing`](../checks/ingress-class-missing.ts) | health | warning | Ingresses whose ingressClassName names an IngressClass which does not exist, or which name no class at all while there is no default class: no controller serves them |  | k8sgpt |
| [`pod-container-restarts`](../checks/pod-container-restarts.ts) | health | warning | Pod containers which restarted often and are unhealthy right now | `restarts=5` | check-conditions |
| [`pod-container-waiting`](../checks/pod-container-waiting.ts) | health | warning | containers which cannot start: image pull errors, bad configuration (a missing ConfigMap key, an invalid image name), failing hooks |  | k8sgpt |
| [`pod-multiple-pdbs`](../checks/pod-multiple-pdbs.ts) | health | warning | Pods selected by more than one PodDisruptionBudget: the eviction API refuses to evict them, so node drains hang |  | Popeye |
| [`pv-failed`](../checks/pv-failed.ts) | health | warning | PersistentVolumes in phase Failed: automatic reclamation failed |  | k8sgpt |
| [`pvc-not-bound`](../checks/pvc-not-bound.ts) | health | warning | PersistentVolumeClaims which are Lost, or Pending for longer than a grace period | `pendingGraceSeconds=300` | k8sgpt |
| [`service-without-endpoints`](../checks/service-without-endpoints.ts) | health | warning | Services whose Pods exist but none is ready: traffic to them fails |  | k8sgpt |
| [`serviceaccount-missing`](../checks/serviceaccount-missing.ts) | health | warning | workloads whose Pods use a ServiceAccount which does not exist: the Pods cannot be created |  | Popeye |
| [`webhook-service-unavailable`](../checks/webhook-service-unavailable.ts) | health | critical | admission webhooks whose Service does not exist or has no ready endpoints |  | k8sgpt |
| [`workload-not-ready`](../checks/workload-not-ready.ts) | health | warning | StatefulSets and DaemonSets with fewer ready Pods than wanted |  | k8sgpt |
| [`admission-policy-warnings`](../checks/admission-policy-warnings.ts) | hygiene | info | CEL type-check warnings of ValidatingAdmissionPolicies: usually an expression which refers to a field that does not exist |  | k8sgpt |
| [`container-image-tag`](../checks/container-image-tag.ts) | hygiene | info | container images without a tag or with the tag "latest": which version runs depends on when the node pulled it |  | Popeye |
| [`container-resources-missing`](../checks/container-resources-missing.ts) | hygiene | info | containers without any resource requests or limits: the scheduler cannot place them sensibly and they are evicted first |  | Popeye |
| [`pod-evicted`](../checks/pod-evicted.ts) | hygiene | info | evicted Pods (node pressure, preemption) |  | k8sgpt |
| [`pod-unmanaged`](../checks/pod-unmanaged.ts) | hygiene | info | Pods which no controller manages: they are not recreated when they or their node die |  | Popeye |
| [`pv-released`](../checks/pv-released.ts) | hygiene | info | PersistentVolumes which are Released (their claim was deleted, reclaim policy Retain) or Available (no claim): storage which is paid for and probably forgotten |  | k8sgpt, Popeye |
| [`rbac-binding-dangling`](../checks/rbac-binding-dangling.ts) | hygiene | warning | RoleBindings and ClusterRoleBindings which reference a Role or ClusterRole that does not exist |  | Popeye |
| [`selector-matches-no-pods`](../checks/selector-matches-no-pods.ts) | hygiene | warning | PodDisruptionBudgets and NetworkPolicies whose pod selector matches no Pod: usually a typo or a leftover |  | k8sgpt, Popeye |
| [`statefulset-references-missing`](../checks/statefulset-references-missing.ts) | hygiene | warning | StatefulSets whose governing Service or the StorageClass of a volumeClaimTemplate does not exist |  | k8sgpt |
| [`storageclass-multiple-default`](../checks/storageclass-multiple-default.ts) | hygiene | warning | StorageClasses marked as default when there is more than one: which one a new claim gets is surprising |  | k8sgpt |
| [`suspended`](../checks/suspended.ts) | hygiene | info | suspended CronJobs and Jobs, and cordoned Nodes: often a manual action which was not undone |  | k8sgpt, Popeye |

<!-- END generated checks table -->

## Writing checks


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

- `check(obj, ctx)` runs for every object of the `match` types and returns findings: `message` (one line), and optionally `key` (to tell several findings of one object apart), `since` (konmon prints the age), `condition`, `container` and `details` (alert labels and annotations; objects and arrays are written as JSON).
- `ctx.get(resource, namespace, name)` and `ctx.list(resource, {namespace, labels, owner})` read konmon's cache, never the API server. A check may read the types in its `match` and `reads`. Reading a type which is not available throws, so a missing CRD never looks like "no objects"; use `ctx.has(resource)` for optional types. With `konmon check -n` of a user who may only read some namespaces, a type can be listed in those namespaces only: `ctx.has` is true, but reading it in another namespace (or in all, or by an owner which is not in one of them) throws, and the object is reported as not checked instead of as a finding. A type whose objects `konmon run` does not know yet (its first list has not finished, e.g. a broken conversion webhook) is similar: `ctx.has` returns false, so the check reports what it can without the type, but the findings it reported before for the object stay open until the type is synced; reading the type throws.
- `forSeconds` in the definition overrides `--for` for a check whose findings are normal for a while (e.g. `workload-not-ready`: 10 minutes). A finding may set its own `severity`.
- `ctx.history.conditions(obj, since)` returns the recorded condition changes (needs `uses: ["history"]`), e.g. to report a condition which flaps.
- Resource names are `resource.group` (`deployments.apps`); a name without a dot is the core group (`pods`).
- Objects are read-only views of konmon's cache (writes throw). Each property access returns a new view, so compare objects by `metadata.uid`, not with `===`. `ctx` is frozen and fresh for every call, but this is not isolation: module variables and globals are shared between calls, so don't keep state in them.
- `parseTime(s)` from `konmon` parses RFC3339 timestamps strictly; `new Date(s)` also accepts timestamps without a zone and reads them as local time.
- `Date` and `Math.random` are deterministic within an evaluation (`ctx.now`). There is no network or filesystem access, and the only import is `konmon`. A call which runs longer than `--check-timeout` fails.
- A check which throws keeps its previous findings for that object open, so `--for` does not start again, and opens one `check-error` finding per check ("failed for N objects, e.g. …").

A type in `reads` is kept in memory for the whole cluster: `configmap-key-missing` makes konmon watch all ConfigMaps (not stored in the database). Disable it on clusters with many large ConfigMaps.

Use your checks with `--checks DIR` (repeatable). `--disable-check NAME` (globs) turns checks off, `--check-param check.param=value` changes parameters, and `konmon checks list` shows what is enabled.

**Types:** `konmon checks types > konmon.d.ts` and a `tsconfig.json` like the one in [checks/](../checks/) give you completion and type errors in your editor.

**Tests:** put `NAME.test.yaml` next to a check. Each case has an object, optionally related objects, history, params and the time, and the expected findings (only the fields given are compared). `konmon checks test --checks DIR` runs them without a cluster. The core checks in [checks/](../checks/) are examples.

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

