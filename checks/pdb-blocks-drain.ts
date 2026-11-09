// Reports PodDisruptionBudgets which make `kubectl drain` hang right now:
// the eviction API refuses (429) to evict one of their Pods, and drain
// retries forever.
//
// Only Pods drain waits for count: bound to a node, Running, not terminating,
// not a mirror Pod, not owned by a DaemonSet, and not an unready Pod the
// budget lets through (unhealthyPodEvictionPolicy). The message names the
// cause:
//
//   - the budget can never allow an eviction, even with all Pods healthy
//     (maxUnavailable: 0, minAvailable: 100%, one replica with
//     minAvailable: 1, ...);
//   - too few of its Pods are healthy right now;
//   - it counts no Pods (expectedPods 0: Pods without a controller, or
//     neither minAvailable nor maxUnavailable set);
//   - the disruption controller cannot compute it (SyncFailed) or has not
//     observed its spec yet.
//
// One key for all causes: during a rollout a budget moves between "never"
// and "too few healthy", which must not resolve and re-fire the finding.
//
// Fires at the next evaluation (forSeconds: 1, not konmon's --for): a drain
// hangs as soon as this is true. Rollouts and drains themselves can open it
// for a while.
//
// Ad hoc, before draining a node (`konmon check` ignores forSeconds):
//
//   konmon check pdb-blocks-drain --check-param pdb-blocks-drain.node=NODE
//
// The node param keeps only budgets with a blocking Pod on that node. Each
// finding names the nodes its blocking Pods run on (message and "nodes"
// annotation).
import { defineCheck, matchesSelector, KubeObject } from "konmon";

export default defineCheck({
  name: "pdb-blocks-drain",
  severity: "warning",
  match: { resources: ["poddisruptionbudgets.policy"] },
  reads: ["pods"],
  forSeconds: 1,
  params: {
    // Only report budgets with a blocking Pod on this node. "": all nodes.
    node: "",
  },
  check(pdb, ctx) {
    const status = pdb.status ?? {};
    const expected = Number(status.expectedPods ?? 0);
    const desired = Number(status.desiredHealthy ?? 0);
    const healthy = Number(status.currentHealthy ?? 0);
    const allowed = Number(status.disruptionsAllowed ?? 0);
    const generation = Number(pdb.metadata.generation ?? 0);
    const observed = Number(status.observedGeneration ?? 0);
    const condition = (status.conditions ?? []).find(
      (c: { type?: string; status?: string }) => c.type === "DisruptionAllowed" && c.status === "False",
    );
    const notObserved = observed < generation;
    if (allowed > 0 && !notObserved) return [];

    // eviction.go: an unready Pod is evicted without asking the budget if
    // the policy is AlwaysAllow, or (IfHealthyBudget, the default) if the
    // budget is met.
    const unreadyEvictable =
      pdb.spec?.unhealthyPodEvictionPolicy === "AlwaysAllow" || (desired > 0 && healthy >= desired);
    const nodes = [
      ...new Set(
        ctx
          .list("pods", { namespace: pdb.metadata.namespace })
          .filter((p) => blocksDrain(p, unreadyEvictable))
          .filter((p) => matchesSelector(p.metadata.labels, pdb.spec?.selector))
          .map((p) => p.spec.nodeName as string),
      ),
    ].sort();
    if (nodes.length === 0) return [];
    const node = ctx.params.node;
    if (node !== "" && !nodes.includes(node)) return [];

    let cause: string;
    if (condition?.reason === "SyncFailed") {
      cause = `cannot be computed (SyncFailed: ${condition.message ?? ""})`;
    } else if (notObserved) {
      cause = `is not observed yet (generation ${generation}, observed ${observed})`;
    } else if (expected <= 0) {
      cause = "counts no Pods (Pods without a controller, or neither minAvailable nor maxUnavailable set)";
    } else if (desired >= expected) {
      cause = `${budget(pdb.spec)} with ${expected} ${expected === 1 ? "Pod" : "Pods"} never allows an eviction`;
    } else {
      cause = `needs ${desired} healthy Pods, ${healthy} of ${expected} are`;
    }
    return [
      {
        message: `PodDisruptionBudget ${cause}: draining ${nodes.length === 1 ? "node" : "nodes"} ${nodes.join(", ")} hangs`,
        since: condition?.lastTransitionTime,
        details: {
          nodes: nodes.join(","),
          expectedPods: expected,
          desiredHealthy: desired,
          currentHealthy: healthy,
        },
      },
    ];
  },
});

// Whether kubectl drain waits for the eviction of this Pod: it deletes
// Pending and terminating Pods without asking the budget (canIgnorePDB),
// skips mirror and DaemonSet Pods, and finished Pods need no eviction.
function blocksDrain(pod: KubeObject, unreadyEvictable: boolean): boolean {
  if (!pod.spec?.nodeName || pod.status?.phase !== "Running" || pod.metadata.deletionTimestamp) return false;
  if (pod.metadata.annotations?.["kubernetes.io/config.mirror"] !== undefined) return false;
  if ((pod.metadata.ownerReferences ?? []).some((o) => o.controller && o.kind === "DaemonSet")) return false;
  const ready = (pod.status?.conditions ?? []).some(
    (c: { type?: string; status?: string }) => c.type === "Ready" && c.status === "True",
  );
  return ready || !unreadyEvictable;
}

function budget(spec: { minAvailable?: unknown; maxUnavailable?: unknown } | undefined): string {
  if (spec?.maxUnavailable !== undefined) return `maxUnavailable ${spec.maxUnavailable}`;
  return `minAvailable ${spec?.minAvailable}`;
}
