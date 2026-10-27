// Reports StatefulSets whose governing Service or the StorageClass of a
// volumeClaimTemplate does not exist.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/statefulset.go.
import { defineCheck, Finding } from "konmon";

export default defineCheck({
  name: "statefulset-references-missing",
  match: { resources: ["statefulsets.apps"] },
  reads: ["services", "storageclasses.storage.k8s.io"],
  check(sts, ctx) {
    const findings: Finding[] = [];
    const svc = sts.spec?.serviceName;
    // serviceName is optional since Kubernetes 1.30.
    if (svc && !ctx.get("services", sts.metadata.namespace ?? null, svc)) {
      findings.push({ key: `service/${svc}`, message: `StatefulSet uses the Service ${svc} which does not exist` });
    }
    for (const t of sts.spec?.volumeClaimTemplates ?? []) {
      const sc = t.spec?.storageClassName;
      if (sc && !ctx.get("storageclasses.storage.k8s.io", null, sc)) {
        findings.push({
          key: `storageclass/${sc}`,
          message: `StatefulSet uses the StorageClass ${sc} which does not exist`,
        });
      }
    }
    return findings;
  },
});
