// Reports containers with a CPU limit: the kernel throttles them (CFS quota)
// as soon as they use the limit within a scheduling period, even while the
// node has idle CPU. That shows up as latency spikes and slow starts, not as
// an error. Without a limit a container can use idle CPU and is still held to
// its share under contention by its CPU request. Drop the limit and set a
// request.
//
// Param allowGuaranteed: skip containers whose CPU request equals the limit
// (Guaranteed QoS on purpose, e.g. for the static CPU manager).
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
  name: "container-cpu-limit",
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
  params: {
    // Skip containers whose CPU request equals the CPU limit.
    allowGuaranteed: false,
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
      const limit = c.resources?.limits?.cpu;
      if (limit === undefined || limit === null) continue;
      const request = c.resources?.requests?.cpu;
      if (ctx.params.allowGuaranteed && request !== undefined && String(request) === String(limit)) continue;
      findings.push({
        key: c.name,
        container: c.name,
        message: `container ${c.name} has a CPU limit (${limit}): it is throttled even while the node has idle CPU; drop the limit and keep a CPU request`,
        details: { limit: String(limit) },
      });
    }
    return findings;
  },
});
