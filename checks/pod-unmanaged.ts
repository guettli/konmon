// Reports Pods which no controller manages: they are not recreated when
// they or their node die. A Pod without owner is strange: usually it is a
// leftover of kubectl run, kubectl debug (a debug copy, or the Pod of
// `kubectl debug node/...`) or of a Deployment someone wrote by hand.
// Static Pods (mirror Pods, owned by the Node) are managed by the kubelet.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/pod.go (code 208).
import { defineCheck } from "konmon";

export default defineCheck({
  name: "pod-unmanaged",
  category: "hygiene",
  severity: "warning",
  match: { resources: ["pods"] },
  check(pod) {
    if ((pod.metadata.ownerReferences ?? []).some((r) => r.controller)) return [];
    if (pod.metadata.annotations?.["kubernetes.io/config.mirror"]) return [];
    const phase = pod.status?.phase;
    if (phase === "Succeeded" || phase === "Failed") return [];
    return [{ message: "Pod is not managed by a controller" }];
  },
});
