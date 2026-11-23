// Reports PodDisruptionBudgets and NetworkPolicies whose pod selector
// matches no Pod, and NetworkPolicy rules whose namespaceSelector matches no
// Namespace (the peer allows nothing): usually a typo or a leftover. An
// empty selector (all Pods, e.g. default-deny; all Namespaces) is fine. Peer pod selectors are not checked: allowing
// traffic from Pods which run only now and then (Jobs) is normal.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/netpol.go, and Popeye (github.com/derailed/popeye,
// Apache-2.0), internal/lint/pdb.go (code 900) and np.go (codes 1200,
// 1201).
import { defineCheck, matchesSelector, isEmptySelector, Finding, KubeObject, LabelSelector } from "konmon";

function selectorString(s: LabelSelector): string {
  const labels = Object.entries(s.matchLabels ?? {}).map(([k, v]) => `${k}=${v}`);
  const exprs = (s.matchExpressions ?? []).map((e) => {
    switch (e.operator) {
      case "Exists":
        return e.key;
      case "DoesNotExist":
        return `!${e.key}`;
      default:
        return `${e.key} ${e.operator.toLowerCase()} (${(e.values ?? []).join(",")})`;
    }
  });
  return [...labels.sort(), ...exprs].join(",");
}

export default defineCheck({
  name: "selector-matches-no-pods",
  category: "hygiene",
  match: { resources: ["poddisruptionbudgets.policy", "networkpolicies.networking.k8s.io"] },
  reads: ["pods", "namespaces"],
  check(obj, ctx) {
    const findings: Finding[] = [];
    const isPDB = ctx.resource.resource === "poddisruptionbudgets";
    const selector = isPDB ? obj.spec?.selector : obj.spec?.podSelector;
    if (selector != null && !isEmptySelector(selector)) {
      const pods = ctx.list("pods", { namespace: obj.metadata.namespace });
      if (!pods.some((p) => matchesSelector(p.metadata.labels, selector))) {
        findings.push({ message: `${obj.kind ?? ctx.resource.resource} selects no Pods` });
      }
    }
    if (isPDB || !ctx.has("namespaces")) return findings;
    let namespaces: KubeObject[] | undefined;
    const seen = new Set<string>();
    const rules = [
      ...(obj.spec?.ingress ?? []).map((r: any) => ({ dir: "ingress", peers: r.from ?? [] })),
      ...(obj.spec?.egress ?? []).map((r: any) => ({ dir: "egress", peers: r.to ?? [] })),
    ];
    for (const { dir, peers } of rules) {
      for (const peer of peers) {
        const ns = peer.namespaceSelector;
        if (ns == null || isEmptySelector(ns)) continue;
        const sel = selectorString(ns);
        if (seen.has(`${dir} ${sel}`)) continue;
        seen.add(`${dir} ${sel}`);
        namespaces ??= ctx.list("namespaces");
        if (namespaces.some((n) => matchesSelector(n.metadata.labels, ns))) continue;
        findings.push({
          key: `${dir} namespaces ${sel}`,
          message: `${dir} namespaceSelector ${sel} selects no Namespaces`,
        });
      }
    }
    return findings;
  },
});
