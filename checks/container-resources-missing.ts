// Reports containers without any resource requests or limits: the
// scheduler cannot place them sensibly and they are evicted first.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/container.go (code 106). Missing limits alone (code 107)
// are not reported: CPU limits are often left out on purpose.
import { defineCheck, podSpec, Finding } from "konmon";

export default defineCheck({
  name: "container-resources-missing",
  category: "hygiene",
  severity: "info",
  match: {
    resources: ["pods", "deployments.apps", "statefulsets.apps", "daemonsets.apps", "cronjobs.batch"],
  },
  check(obj, ctx) {
    if (ctx.resource.resource === "pods" && (obj.metadata.ownerReferences ?? []).length > 0) return [];
    const spec = podSpec(obj, ctx.resource.resource);
    const findings: Finding[] = [];
    for (const c of spec?.containers ?? []) {
      const r = c.resources ?? {};
      if (Object.keys(r.requests ?? {}).length === 0 && Object.keys(r.limits ?? {}).length === 0) {
        findings.push({
          key: c.name,
          container: c.name,
          message: `container ${c.name} has no resource requests or limits`,
        });
      }
    }
    return findings;
  },
});
