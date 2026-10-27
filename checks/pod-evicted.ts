// Reports evicted Pods (node pressure, preemption). They stay until
// garbage-collected and hide that the workload lost a replica.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/pod.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "pod-evicted",
  match: { resources: ["pods"] },
  check(pod) {
    if (pod.status?.reason !== "Evicted") return [];
    return [{ message: `evicted: ${pod.status?.message || "no message"}` }];
  },
});
