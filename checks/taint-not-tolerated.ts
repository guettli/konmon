// Reports Node taints (NoSchedule, NoExecute) which no Pod tolerates: only
// Pods which tolerate all taints (CNI, log and monitoring agents) can be
// scheduled there, usually a leftover of maintenance or of a removed
// workload. Taints which Kubernetes, Cluster API, cluster autoscalers or
// agents set by themselves are skipped.
//
// Only tolerations of a key count: a wildcard toleration (no key, operator
// Exists; also one per effect, as many DaemonSets have) tolerates every
// taint and says nothing about whether this one is still wanted.
//
// Off by default: a taint which reserves a Node for a workload that is not
// running right now is legitimate. Enable it with
// --enable-check taint-not-tolerated.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/node.go (code 700).
import { defineCheck, Finding, type Context } from "konmon";

const system = [
  /^node\.kubernetes\.io\//,
  /^node-role\.kubernetes\.io\/(control-plane|master)$/,
  /^node\.cloudprovider\.kubernetes\.io\//,
  /^node\.cluster\.x-k8s\.io\//,
  /^karpenter\.sh\//,
  /^node\.cilium\.io\//,
  /^cloud\.google\.com\/impending-node-termination$/,
  /^ToBeDeletedByClusterAutoscaler$/,
];

// The tolerations of a key of all Pods, built once per evaluation: the
// check runs per Node, the Pods are the same for all. An evaluation is told
// by its time and by its (frozen, per evaluation) resources() array.
let cache: { at: number; resources: string[]; byKey: Map<string, any[]> } | null = null;

function keyedTolerations(ctx: Context<unknown>): Map<string, any[]> {
  const at = ctx.now.getTime();
  const resources = ctx.resources();
  if (cache?.at !== at || cache.resources !== resources) {
    const byKey = new Map<string, any[]>();
    for (const p of ctx.list("pods")) {
      for (const t of p.spec?.tolerations ?? []) {
        if (!t.key) continue;
        const list = byKey.get(t.key) ?? [];
        list.push(t);
        byKey.set(t.key, list);
      }
    }
    cache = { at, resources, byKey };
  }
  return cache.byKey;
}

function tolerates(t: any, taint: any): boolean {
  if (t.effect && t.effect !== taint.effect) return false;
  switch (t.operator ?? "Equal") {
    case "Exists":
      return true;
    case "Equal":
      return (t.value ?? "") === (taint.value ?? "");
    default:
      // Operators konmon does not know (newer Kubernetes): rather miss a
      // finding than report a tolerated taint.
      return true;
  }
}

export default defineCheck({
  name: "taint-not-tolerated",
  category: "hygiene",
  severity: "info",
  enabled: false,
  match: { resources: ["nodes"] },
  reads: ["pods"],
  check(node, ctx) {
    const taints = (node.spec?.taints ?? []).filter(
      (t: any) => t.key && t.effect !== "PreferNoSchedule" && !system.some((r) => r.test(t.key)),
    );
    if (taints.length === 0) return [];
    const byKey = keyedTolerations(ctx);
    const findings: Finding[] = [];
    for (const taint of taints) {
      if ((byKey.get(taint.key) ?? []).some((t) => tolerates(t, taint))) continue;
      const kv = taint.value ? `${taint.key}=${taint.value}` : taint.key;
      findings.push({
        key: `${kv}:${taint.effect}`,
        message: `no Pod tolerates taint ${kv}:${taint.effect}: only Pods which tolerate all taints can be scheduled there`,
      });
    }
    return findings;
  },
});
