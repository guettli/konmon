// Reports PersistentVolumes which are Released (their claim was deleted,
// reclaim policy Retain) or Available (no claim): storage which is paid for
// and probably forgotten.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/storage.go, and Popeye (github.com/derailed/popeye,
// Apache-2.0), internal/lint/pv.go (code 1000).
import { defineCheck } from "konmon";

export default defineCheck({
  name: "pv-released",
  category: "hygiene",
  severity: "info",
  match: { resources: ["persistentvolumes"] },
  check(pv) {
    const phase = pv.status?.phase;
    if (phase !== "Released" && phase !== "Available") return [];
    return [{ message: `PersistentVolume is ${phase} and not used by a claim` }];
  },
});
