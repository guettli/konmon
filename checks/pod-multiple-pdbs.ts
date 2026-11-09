// Reports Pods selected by more than one PodDisruptionBudget: the eviction
// API refuses to evict them (500), so a node drain fails.
//
// Before draining a node, the node param keeps only its Pods:
//   konmon check pod-multiple-pdbs --check-param pod-multiple-pdbs.node=NODE
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/pod.go (code 209).
import { defineCheck, matchesSelector } from "konmon";

export default defineCheck({
  name: "pod-multiple-pdbs",
  match: { resources: ["pods"] },
  reads: ["poddisruptionbudgets.policy"],
  params: {
    // Only report Pods on this node. "": all nodes.
    node: "",
  },
  check(pod, ctx) {
    if (!ctx.has("poddisruptionbudgets.policy")) return [];
    if (ctx.params.node !== "" && pod.spec?.nodeName !== ctx.params.node) return [];
    const names = ctx
      .list("poddisruptionbudgets.policy", { namespace: pod.metadata.namespace })
      // An empty selector selects every Pod of the namespace.
      .filter((pdb) => matchesSelector(pod.metadata.labels, pdb.spec?.selector))
      .map((pdb) => pdb.metadata.name)
      .sort();
    if (names.length < 2) return [];
    return [
      {
        message: `Pod is selected by ${names.length} PodDisruptionBudgets (${names.join(", ")}): it cannot be evicted`,
      },
    ];
  },
});
