// Reports PodDisruptionBudgets which allow no disruption right now: the
// eviction API refuses to evict their Pods, so `kubectl drain` of a node
// running one of them hangs (it retries the eviction forever).
//
// Two findings, told apart by key:
//
//   - "config": the budget can never allow an eviction, even with all Pods
//     healthy (maxUnavailable: 0, minAvailable: 100%, or minAvailable >= the
//     number of Pods, e.g. one replica with minAvailable: 1).
//   - "unhealthy": the budget would allow an eviction, but too few of its
//     Pods are healthy right now.
//
// Both fire at the next evaluation (forSeconds: 1, i.e. not konmon's --for):
// a drain hangs as soon as this is true. During a rollout the "unhealthy"
// finding can show up briefly.
//
// Ad hoc, before draining a node (`konmon check` ignores forSeconds):
//
//   konmon check pdb-blocks-drain --check-param pdb-blocks-drain.node=NODE
//
// The node param keeps only budgets with a selected Pod on that node. Each
// finding names the nodes its Pods run on (message and "nodes" annotation).
import { defineCheck, matchesSelector } from "konmon";

export default defineCheck({
  name: "pdb-blocks-drain",
  severity: "warning",
  match: { resources: ["poddisruptionbudgets.policy"] },
  reads: ["pods"],
  forSeconds: 1,
  params: {
    // Only report budgets with a selected Pod on this node. "": all nodes.
    node: "",
  },
  check(pdb, ctx) {
    const status = pdb.status ?? {};
    const expected = Number(status.expectedPods ?? 0);
    // No Pods: nothing to evict (selector-matches-no-pods reports typos).
    if (!(expected > 0)) return [];
    // The disruption controller has not seen this spec yet.
    if (Number(status.observedGeneration ?? 0) < Number(pdb.metadata.generation ?? 0)) return [];
    if (Number(status.disruptionsAllowed ?? 0) > 0) return [];

    const nodes = [
      ...new Set(
        ctx
          .list("pods", { namespace: pdb.metadata.namespace })
          .filter((p) => p.spec?.nodeName && p.status?.phase !== "Succeeded" && p.status?.phase !== "Failed")
          .filter((p) => matchesSelector(p.metadata.labels, pdb.spec?.selector))
          .map((p) => p.spec.nodeName as string),
      ),
    ].sort();
    // Only Pods bound to a node are evicted by a drain.
    if (nodes.length === 0) return [];
    const node = ctx.params.node;
    if (node !== "" && !nodes.includes(node)) return [];

    const desired = Number(status.desiredHealthy ?? 0);
    const healthy = Number(status.currentHealthy ?? 0);
    const since = (status.conditions ?? []).find(
      (c: { type?: string; status?: string }) => c.type === "DisruptionAllowed" && c.status === "False",
    )?.lastTransitionTime;
    const drains = `draining ${nodes.length === 1 ? "node" : "nodes"} ${nodes.join(", ")} hangs`;
    const details = {
      nodes: nodes.join(","),
      expectedPods: expected,
      desiredHealthy: desired,
      currentHealthy: healthy,
    };

    if (desired >= expected) {
      return [
        {
          key: "config",
          message: `PodDisruptionBudget ${budget(pdb.spec)} with ${expected} ${expected === 1 ? "Pod" : "Pods"} never allows an eviction: ${drains}`,
          since,
          details,
        },
      ];
    }
    return [
      {
        key: "unhealthy",
        message: `PodDisruptionBudget needs ${desired} healthy Pods, ${healthy} of ${expected} are: no eviction allowed, ${drains}`,
        since,
        details,
      },
    ];
  },
});

function budget(spec: { minAvailable?: unknown; maxUnavailable?: unknown } | undefined): string {
  if (spec?.maxUnavailable !== undefined) return `maxUnavailable ${spec.maxUnavailable}`;
  if (spec?.minAvailable !== undefined) return `minAvailable ${spec.minAvailable}`;
  return "without a budget";
}
