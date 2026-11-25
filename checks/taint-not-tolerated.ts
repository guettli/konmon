// Reports Node taints (NoSchedule, NoExecute) which no Pod tolerates:
// nothing can run on that Node besides DaemonSets which tolerate everything,
// usually a leftover of maintenance or of a removed workload. Taints Kubernetes
// sets itself (node.kubernetes.io/*: conditions, unschedulable, ...), the
// control-plane taints and those of cloud providers' initialisation are
// skipped.
//
// Off by default: a taint which reserves a Node for a workload that is not
// running right now is legitimate. Enable it with
// --enable-check taint-not-tolerated.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/node.go (code 700).
import { defineCheck, Finding } from "konmon";

const system = [
  /^node\.kubernetes\.io\//,
  /^node-role\.kubernetes\.io\/(control-plane|master)$/,
  /^node\.cloudprovider\.kubernetes\.io\//,
  /^ToBeDeletedByClusterAutoscaler$/,
  /^DeletionCandidateOfClusterAutoscaler$/,
];

function tolerates(t: any, taint: any): boolean {
  if (t.effect && t.effect !== taint.effect) return false;
  // An empty key with operator Exists tolerates everything.
  if (!t.key) return t.operator === "Exists";
  if (t.key !== taint.key) return false;
  return t.operator === "Exists" || (t.value ?? "") === (taint.value ?? "");
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
      (t: any) => t.effect !== "PreferNoSchedule" && !system.some((r) => r.test(t.key)),
    );
    if (taints.length === 0) return [];
    // Pods which tolerate everything (DaemonSets of the CNI, log agents)
    // say nothing about whether the taint is still wanted.
    const pods = ctx
      .list("pods")
      .map((p) => p.spec?.tolerations ?? [])
      .filter((ts: any[]) => !ts.some((t) => !t.key && t.operator === "Exists" && !t.effect));
    const findings: Finding[] = [];
    for (const taint of taints) {
      if (pods.some((ts: any[]) => ts.some((t) => tolerates(t, taint)))) continue;
      const kv = taint.value ? `${taint.key}=${taint.value}` : taint.key;
      findings.push({
        key: `${kv}:${taint.effect}`,
        message: `taint ${kv}:${taint.effect} is tolerated by no Pod: only Pods which tolerate everything run there`,
      });
    }
    return findings;
  },
});
