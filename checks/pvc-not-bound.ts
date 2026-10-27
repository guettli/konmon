// Reports PersistentVolumeClaims which are Lost, or Pending for longer
// than a grace period. A Pending claim of a WaitForFirstConsumer
// StorageClass which no Pod uses is fine: it binds when a Pod needs it.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/storage.go, plus the WaitForFirstConsumer exception.
import { defineCheck, parseTime } from "konmon";

export default defineCheck({
  name: "pvc-not-bound",
  match: { resources: ["persistentvolumeclaims"] },
  // Pods are read to see whether a WaitForFirstConsumer claim is used.
  reads: ["storageclasses.storage.k8s.io", "pods"],
  params: { pendingGraceSeconds: 300 },
  check(pvc, ctx) {
    const phase = pvc.status?.phase;
    if (phase === "Lost") return [{ message: "PersistentVolumeClaim is Lost: its PersistentVolume is gone" }];
    if (phase !== "Pending") return [];
    const created = parseTime(pvc.metadata.creationTimestamp);
    if (created && ctx.now.getTime() - created.getTime() < ctx.params.pendingGraceSeconds * 1000) return [];
    const scName = pvc.spec?.storageClassName;
    if (scName && ctx.has("storageclasses.storage.k8s.io")) {
      const sc = ctx.get("storageclasses.storage.k8s.io", null, scName);
      if (!sc) return [{ message: `PersistentVolumeClaim is Pending: StorageClass ${scName} does not exist`, since: created }];
      if (sc.volumeBindingMode === "WaitForFirstConsumer") {
        const used = ctx
          .list("pods", { namespace: pvc.metadata.namespace })
          .some((p) =>
            (p.spec?.volumes ?? []).some(
              (v: any) =>
                v.persistentVolumeClaim?.claimName === pvc.metadata.name ||
                // A generic ephemeral volume's claim is named <pod>-<volume>.
                (v.ephemeral && `${p.metadata.name}-${v.name}` === pvc.metadata.name),
            ),
          );
        if (!used) return [];
      }
    }
    return [{ message: "PersistentVolumeClaim is Pending", since: created }];
  },
});
