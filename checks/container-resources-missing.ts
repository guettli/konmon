// Reports containers without any resource requests or limits: the
// scheduler cannot place them sensibly and they are evicted first.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/container.go (code 106). Missing limits alone (code 107)
// are not reported: CPU limits are often left out on purpose.
import { defineCheck, podSpec, Finding, KubeObject } from "konmon";

// Pods of a workload, ReplicaSets of a Deployment and Jobs of a CronJob
// are reported via the workload. Objects of other owners (an operator's
// custom resource, an Argo Rollout, a static Pod's Node) are reported
// themselves.
function reportedViaOwner(obj: KubeObject, resource: string): boolean {
  const kinds: Record<string, string[]> = {
    pods: ["ReplicaSet", "StatefulSet", "DaemonSet", "Job"],
    replicasets: ["Deployment"],
    jobs: ["CronJob"],
  };
  return (obj.metadata.ownerReferences ?? []).some((r) => r.controller && (kinds[resource] ?? []).includes(r.kind));
}

export default defineCheck({
  name: "container-resources-missing",
  category: "hygiene",
  severity: "info",
  match: {
    resources: [
      "pods",
      "deployments.apps",
      "statefulsets.apps",
      "daemonsets.apps",
      "replicasets.apps",
      "jobs.batch",
      "cronjobs.batch",
    ],
  },
  check(obj, ctx) {
    if (reportedViaOwner(obj, ctx.resource.resource)) return [];
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
