// Reports HorizontalPodAutoscalers whose scale target does not exist.
// Targets of other kinds (e.g. Argo Rollouts) are not checked.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/hpa.go, and Popeye (github.com/derailed/popeye,
// Apache-2.0), internal/lint/hpa.go (code 600).
import { defineCheck } from "konmon";

const kinds: Record<string, string> = {
  deployment: "deployments.apps",
  statefulset: "statefulsets.apps",
  replicaset: "replicasets.apps",
  replicationcontroller: "replicationcontrollers",
};

export default defineCheck({
  name: "hpa-target-missing",
  match: { resources: ["horizontalpodautoscalers.autoscaling"] },
  reads: ["deployments.apps", "statefulsets.apps", "replicasets.apps", "replicationcontrollers"],
  check(hpa, ctx) {
    const ref = hpa.spec?.scaleTargetRef;
    if (!ref?.kind || !ref?.name) return [];
    const resource = kinds[String(ref.kind).toLowerCase()];
    if (!resource) return [];
    if (ctx.get(resource, hpa.metadata.namespace ?? null, ref.name)) return [];
    return [{ message: `HorizontalPodAutoscaler scales ${ref.kind} ${ref.name} which does not exist` }];
  },
});
