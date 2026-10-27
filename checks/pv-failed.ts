// Reports PersistentVolumes in phase Failed: automatic reclamation failed.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/storage.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "pv-failed",
  match: { resources: ["persistentvolumes"] },
  check(pv) {
    if (pv.status?.phase !== "Failed") return [];
    return [{ message: `PersistentVolume is Failed${pv.status?.message ? ": " + pv.status.message : ""}` }];
  },
});
