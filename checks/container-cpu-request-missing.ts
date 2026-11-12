// Reports containers without a CPU request. The scheduler counts them as
// using no CPU, so a node can be packed with them; under contention they get
// the smallest CPU share (2 of 1024), and their Pod lands in the BestEffort or
// Burstable QoS class.
//
// A container with a CPU limit but no request gets request = limit from
// Kubernetes: that is reported by container-cpu-limit, not here.
//
// Long-running containers only: regular containers and native sidecars (init
// containers with restartPolicy Always).
import { defineCheck, podSpec, Finding, KubeObject } from "konmon";

// Pods of a workload, ReplicaSets of a Deployment and Jobs of a CronJob
// are reported via the workload.
function reportedViaOwner(obj: KubeObject, resource: string): boolean {
  const kinds: Record<string, string[]> = {
    pods: ["ReplicaSet", "StatefulSet", "DaemonSet", "Job"],
    replicasets: ["Deployment"],
    jobs: ["CronJob"],
  };
  return (obj.metadata.ownerReferences ?? []).some((r) => r.controller && (kinds[resource] ?? []).includes(r.kind));
}

export default defineCheck({
  name: "container-cpu-request-missing",
  category: "hygiene",
  severity: "warning",
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
    const containers = [
      ...(spec?.containers ?? []),
      ...(spec?.initContainers ?? []).filter((c: { restartPolicy?: string }) => c.restartPolicy === "Always"),
    ];
    const findings: Finding[] = [];
    for (const c of containers) {
      const r = c.resources ?? {};
      if (r.requests?.cpu !== undefined && r.requests?.cpu !== null) continue;
      if (r.limits?.cpu !== undefined && r.limits?.cpu !== null) continue;
      findings.push({
        key: c.name,
        container: c.name,
        message: `container ${c.name} has no CPU request: the scheduler counts it as using no CPU, and it gets the smallest CPU share under contention`,
      });
    }
    return findings;
  },
});
