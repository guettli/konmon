// Reports Deployments and StatefulSets with more than one replica which no
// PodDisruptionBudget covers: a node drain (or a cluster autoscaler) may
// evict all their Pods at once if they share a Node or several Nodes are
// drained together. With one replica a budget cannot help (it would only
// block the drain, see pdb-blocks-drain). With a HorizontalPodAutoscaler its
// minReplicas counts, so the finding does not come and go with the load.
// Whether a budget is useful (minAvailable 0, maxUnavailable 100%) is not
// judged: any budget which selects the Pods counts.
//
// Off by default: many workloads tolerate a short outage. Enable it with
// --enable-check workload-pdb-missing.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/pod.go (code 206), checked per workload instead of per Pod.
import { defineCheck, matchesSelector, podLabels } from "konmon";

const hpas = "horizontalpodautoscalers.autoscaling";

export default defineCheck({
  name: "workload-pdb-missing",
  category: "hygiene",
  severity: "info",
  enabled: false,
  match: { resources: ["deployments.apps", "statefulsets.apps"] },
  reads: ["poddisruptionbudgets.policy", hpas],
  check(obj, ctx) {
    let replicas: number = obj.spec?.replicas ?? 1;
    if (ctx.has(hpas)) {
      const kind = ctx.resource.resource === "deployments" ? "Deployment" : "StatefulSet";
      const hpa = ctx
        .list(hpas, { namespace: obj.metadata.namespace })
        .find((h) => h.spec?.scaleTargetRef?.kind === kind && h.spec?.scaleTargetRef?.name === obj.metadata.name);
      if (hpa) replicas = hpa.spec?.minReplicas ?? 1;
    }
    if (replicas < 2) return [];
    const labels = podLabels(obj, ctx.resource.resource);
    // policy/v1: an empty selector selects every Pod of the namespace, a
    // missing one none (matchesSelector does both).
    const pdbs = ctx.list("poddisruptionbudgets.policy", { namespace: obj.metadata.namespace });
    if (pdbs.some((b) => matchesSelector(labels, b.spec?.selector))) return [];
    return [
      {
        message: `${replicas} replicas, but no PodDisruptionBudget: a node drain may evict them all at once (if they share a node, or several nodes are drained together)`,
      },
    ];
  },
});
