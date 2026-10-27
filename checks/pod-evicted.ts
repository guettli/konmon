// Reports evicted Pods (node pressure, preemption). Hygiene, not health:
// the controller has usually replaced the Pod, and the evicted one stays
// until garbage collection (by default only above 12,500 terminated Pods).
// The pressure itself shows up in the Node conditions.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/pod.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "pod-evicted",
  category: "hygiene",
  severity: "info",
  match: { resources: ["pods"] },
  check(pod) {
    if (pod.status?.reason !== "Evicted") return [];
    return [{ message: `evicted: ${pod.status?.message || "no message"}` }];
  },
});
