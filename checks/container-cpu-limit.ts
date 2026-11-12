// Reports containers with a CPU limit: the kernel throttles them (CFS quota)
// as soon as they use the limit within a scheduling period, even while the
// node has idle CPU. That shows up as latency spikes and slow starts, not as
// an error. Without a limit a container can use idle CPU and is still held to
// its share under contention by its CPU request. Drop the limit and set a
// request.
//
// A limit of 0 is no limit (no CFS quota) and is not reported.
//
// Param allowGuaranteed: skip containers whose CPU request equals the limit
// (Guaranteed QoS on purpose, e.g. for the static CPU manager). A missing
// request counts as equal: Kubernetes then sets request = limit.
//
// Pods of workloads are skipped (reported via the workload), so a default CPU
// limit from a LimitRange, applied to the Pods only, is not seen here.
//
// Long-running containers only: regular containers and native sidecars (init
// containers with restartPolicy Always).
import { defineCheck, podSpec, Finding, KubeObject } from "konmon";

const quantity = /^([+-]?(?:\d+\.?\d*|\.\d+))(?:(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E)|([eE][+-]?\d+))?$/;

// A decimal power of ten per suffix (binary suffixes do not occur for CPU).
const decimal: Record<string, number> = { n: -9, u: -6, m: -3, k: 3, M: 6, G: 9, T: 12, P: 15, E: 18 };

// A CPU quantity ("500m", "1", 2, "1e3") in cores; undefined if unparsable.
// Same rules as container-resources-strange.
function cores(v: unknown): number | undefined {
  if (typeof v === "number") return isFinite(v) ? v : undefined;
  if (typeof v !== "string") return undefined;
  const m = quantity.exec(v);
  if (!m) return undefined;
  if (m[3]) return Number(v);
  const suffix = m[2];
  if (!suffix) return Number(m[1]);
  if (!(suffix in decimal)) return undefined;
  return Number(`${m[1]}e${decimal[suffix]}`);
}

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
      const limitCores = cores(limit);
      if (limitCores === undefined || limitCores <= 0) continue;
      const request = c.resources?.requests?.cpu;
      const requestCores = request === undefined || request === null ? limitCores : cores(request);
      if (ctx.params.allowGuaranteed && requestCores === limitCores) continue;
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
