// Reports containers whose memory use (working set) stays at a share of
// their memory limit (default 90%) for 15 minutes: at the limit the kernel
// OOM-kills them unless it can reclaim page cache. The working set includes
// active page cache, so databases and programs which size their heap to the
// limit (JVM, GOMEMLIMIT) may sit there harmlessly; raise percent, or
// disable the check for them in the config file. The use comes from the
// metrics API (metrics-server), sampled every --usage-interval; the latest
// sample counts. The limit is the applied one (status, after an in-place
// resize), else the one of the spec. Containers without a memory limit are
// not checked: they are bounded by the Node, not by the limit.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/container.go (code 112).
import { defineCheck, Finding } from "konmon";

// The grammar of resource.Quantity: a number, then a binary suffix (group
// 2), a decimal SI suffix (group 2) or a decimal exponent (group 3).
const quantity = /^([+-]?(?:\d+\.?\d*|\.\d+))(?:(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E)|([eE][+-]?\d+))?$/;
const binary: Record<string, number> = { Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, Pi: 2 ** 50, Ei: 2 ** 60 };
const decimal: Record<string, number> = { n: -9, u: -6, m: -3, k: 3, M: 6, G: 9, T: 12, P: 15, E: 18 };

// A Kubernetes quantity in bytes; undefined if it is none.
function parseQuantity(v: unknown): number | undefined {
  if (typeof v === "number") return isFinite(v) ? v : undefined;
  if (typeof v !== "string") return undefined;
  const m = quantity.exec(v);
  if (!m) return undefined;
  if (m[3]) return Number(v);
  const suffix = m[2];
  if (!suffix) return Number(m[1]);
  if (suffix in binary) return Number(m[1]) * binary[suffix];
  return Number(`${m[1]}e${decimal[suffix]}`);
}

function bytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${i === 0 ? n : n.toFixed(1)} ${units[i]}`;
}

export default defineCheck({
  name: "container-memory-near-limit",
  match: { resources: ["pods"] },
  uses: ["container-usage"],
  // A short peak is normal; a level which stays is not.
  forSeconds: 900,
  params: { percent: 90 },
  check(pod, ctx) {
    const phase = pod.status?.phase;
    if (phase !== "Running" || pod.metadata.deletionTimestamp) return [];
    const spec = pod.spec ?? {};
    // Sidecars (init containers with restartPolicy Always) run alongside.
    const applied = new Map<string, unknown>();
    for (const s of [...(pod.status?.containerStatuses ?? []), ...(pod.status?.initContainerStatuses ?? [])]) {
      if (s.name && s.resources) applied.set(s.name, s.resources.limits?.memory);
    }
    const limits = new Map<string, number>();
    for (const c of [
      ...(spec.containers ?? []),
      ...(spec.initContainers ?? []).filter((c: any) => c.restartPolicy === "Always"),
    ]) {
      const limit = parseQuantity(applied.has(c.name) ? applied.get(c.name) : c.resources?.limits?.memory);
      if (c.name && limit !== undefined && limit > 0) limits.set(c.name, limit);
    }
    if (limits.size === 0) return [];
    const usage = ctx.containerUsage(pod);
    if (!usage) return [];
    const findings: Finding[] = [];
    for (const [name, limit] of limits) {
      const used = usage.containers[name]?.memoryBytes;
      if (used === undefined) continue;
      const pct = (100 * used) / limit;
      if (pct < ctx.params.percent) continue;
      findings.push({
        key: name,
        container: name,
        message: `container uses ${Math.floor(pct)}% of its memory limit (${bytes(used)} working set of ${bytes(limit)}): at the limit it is OOM-killed unless the kernel can reclaim page cache`,
        details: { memoryBytes: used, limitBytes: limit },
      });
    }
    return findings;
  },
});
