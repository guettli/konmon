// Reports Deployments and StatefulSets with more than one replica which no
// PodDisruptionBudget covers: a node drain (or a cluster autoscaler) may
// evict all their Pods at once, although there are several. With one
// replica a budget cannot help (it would only block the drain, see
// pdb-blocks-drain).
//
// Off by default: many workloads tolerate a short outage. Enable it with
// --enable-check workload-pdb-missing.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/pod.go (code 206), checked per workload instead of per Pod.
import { defineCheck, matchesSelector, isEmptySelector, podLabels } from "konmon";

export default defineCheck({
  name: "workload-pdb-missing",
  category: "hygiene",
  severity: "info",
  enabled: false,
  match: { resources: ["deployments.apps", "statefulsets.apps"] },
  reads: ["poddisruptionbudgets.policy"],
  check(obj, ctx) {
    const replicas = obj.spec?.replicas ?? 1;
    if (replicas < 2) return [];
    const labels = podLabels(obj, ctx.resource.resource);
    const pdbs = ctx.list("poddisruptionbudgets.policy", { namespace: obj.metadata.namespace });
    // An empty selector of a policy/v1 budget selects every Pod of the
    // namespace.
    if (pdbs.some((b) => isEmptySelector(b.spec?.selector) || matchesSelector(labels, b.spec?.selector))) return [];
    return [{ message: `${replicas} replicas, but no PodDisruptionBudget: a node drain may evict all at once` }];
  },
});
