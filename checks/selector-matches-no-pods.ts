// Reports PodDisruptionBudgets and NetworkPolicies whose pod selector
// matches no Pod: usually a typo or a leftover. An empty selector (all
// Pods, e.g. default-deny) is fine.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/netpol.go, and Popeye (github.com/derailed/popeye,
// Apache-2.0), internal/lint/pdb.go (code 900) and np.go (code 1200).
import { defineCheck, matchesSelector, isEmptySelector } from "konmon";

export default defineCheck({
  name: "selector-matches-no-pods",
  category: "hygiene",
  match: { resources: ["poddisruptionbudgets.policy", "networkpolicies.networking.k8s.io"] },
  reads: ["pods"],
  check(obj, ctx) {
    const selector = ctx.resource.resource === "poddisruptionbudgets" ? obj.spec?.selector : obj.spec?.podSelector;
    if (selector == null || isEmptySelector(selector)) return [];
    const pods = ctx.list("pods", { namespace: obj.metadata.namespace });
    if (pods.some((p) => matchesSelector(p.metadata.labels, selector))) return [];
    return [{ message: `${obj.kind ?? ctx.resource.resource} selects no Pods` }];
  },
});
