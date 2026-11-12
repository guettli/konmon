// Reports containers without a CPU request: the scheduler counts them as
// using no CPU, so a node can be packed with them, and under contention they
// get the smallest CPU share (2 of 1024). An explicit request of 0 is the same
// and is reported too.
//
// A container with a CPU limit (above 0) but no request gets request = limit
// from Kubernetes: that is reported by container-cpu-limit, not here.
//
// Pods of workloads are skipped (reported via the workload), so a
// defaultRequest from a LimitRange, applied to the Pods only, is not seen
// here: such a workload is reported although its Pods have a request.
//
// Overlaps with container-resources-missing (off by default), which reports
// containers without any requests or limits.
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
      if ((cores(r.requests?.cpu) ?? 0) > 0) continue;
      // Without a request, a limit above 0 becomes the request.
      if (r.requests?.cpu == null && (cores(r.limits?.cpu) ?? 0) > 0) continue;
      findings.push({
        key: c.name,
        container: c.name,
        message: `container ${c.name} has no CPU request: the scheduler counts it as using no CPU, and it gets the smallest CPU share under contention`,
      });
    }
    return findings;
  },
});
