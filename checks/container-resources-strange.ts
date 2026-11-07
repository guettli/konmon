// Reports resource requests and limits whose value is almost certainly a
// mistake: 1000 CPU cores, or 1m memory (a thousandth of a byte, instead of
// 1Mi). Only cpu and memory are looked at; an explicit 0 means "no request"
// and is legal, and a missing value is the business of
// container-resources-missing.
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

// The grammar of resource.Quantity: a number, then a binary suffix (group
// 2), a decimal SI suffix (group 2) or a decimal exponent (group 3).
const quantity = /^([+-]?(?:\d+\.?\d*|\.\d+))(?:(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E)|([eE][+-]?\d+))?$/;

const binary: Record<string, number> = {
  Ki: 1024,
  Mi: 1048576,
  Gi: 1073741824,
  Ti: 1099511627776,
  Pi: 1125899906842624,
  Ei: 1152921504606846976,
};

// A power of ten, applied to the digits: Number("100e-6") is 0.0001, while
// 100 * 1e-6 is 0.00009999999999999999 and reads badly in a message.
const decimal: Record<string, number> = {
  n: -9,
  u: -6,
  m: -3,
  k: 3,
  M: 6,
  G: 9,
  T: 12,
  P: 15,
  E: 18,
};

// A Kubernetes quantity ("100m", "1.5Gi", "1e3", 512) as cores or bytes.
// undefined for anything resource.Quantity rejects. A float is enough: the
// values are only compared against thresholds.
function parseQuantity(v: unknown): number | undefined {
  if (typeof v === "number") return isFinite(v) ? v : undefined;
  if (typeof v !== "string") return undefined;
  const m = quantity.exec(v);
  if (!m) return undefined;
  // A decimal exponent is a JavaScript number literal as it stands.
  if (m[3]) return Number(v);
  const suffix = m[2];
  if (!suffix) return Number(m[1]);
  if (suffix in binary) return Number(m[1]) * binary[suffix];
  return Number(`${m[1]}e${decimal[suffix]}`);
}

function threshold(name: string, value: string): number {
  const v = parseQuantity(value);
  if (v === undefined) throw new Error(`param ${name}: ${value} is not a Kubernetes quantity`);
  return v;
}

// What the author of a tiny memory value probably meant: "1m" and "512"
// (bytes) become "1Mi" and "512Mi". No guess for a written exponent or a
// binary suffix, where the digits alone say little. Only plain digits: the
// API server stores quantities in canonical form, so a written "1.5" arrives
// as "1500m" -- 1000 or more of a sub-unit suffix was a decimal, and
// "1500Mi" would be a wrong guess.
function memoryHint(value: string): string {
  const m = quantity.exec(value);
  if (!m || m[3]) return "";
  const suffix = m[2] ?? "";
  if (suffix !== "" && suffix !== "n" && suffix !== "u" && suffix !== "m") return "";
  if (!/^\d+$/.test(m[1])) return "";
  if (suffix !== "" && Number(m[1]) >= 1000) return "";
  return ` (did you mean ${m[1]}Mi?)`;
}

export default defineCheck({
  name: "container-resources-strange",
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
  // Quantities, not numbers, so that --check-param
  // container-resources-strange.maxMemory=256Gi works. 0 disables a rule.
  params: {
    // More CPU than this many cores: "cpu: 1000" asks for 1000 cores.
    maxCpu: "64",
    // Less CPU than this, but more than nothing.
    minCpu: "1m",
    maxMemory: "1Ti",
    // Less memory than this: usually a missing Mi or Gi suffix.
    minMemory: "4Mi",
  },
  check(obj, ctx) {
    if (reportedViaOwner(obj, ctx.resource.resource)) return [];
    const spec = podSpec(obj, ctx.resource.resource);
    const maxCpu = threshold("maxCpu", ctx.params.maxCpu);
    const minCpu = threshold("minCpu", ctx.params.minCpu);
    const maxMemory = threshold("maxMemory", ctx.params.maxMemory);
    const minMemory = threshold("minMemory", ctx.params.minMemory);
    const findings: Finding[] = [];
    for (const c of [...(spec?.initContainers ?? []), ...(spec?.containers ?? [])]) {
      for (const section of ["requests", "limits"] as const) {
        for (const name of ["cpu", "memory"] as const) {
          const written = c.resources?.[section]?.[name];
          if (written === undefined || written === null) continue;
          const value = parseQuantity(written);
          // An explicit 0 is legal, and a value the API server would have
          // rejected cannot be fixed by this check.
          if (value === undefined || value <= 0) continue;
          let problem = "";
          if (name === "cpu") {
            if (maxCpu > 0 && value > maxCpu) {
              problem = `is ${value} CPU cores, more than maxCpu ${ctx.params.maxCpu}`;
            } else if (minCpu > 0 && value < minCpu) {
              problem = `is ${value} CPU cores, less than minCpu ${ctx.params.minCpu}`;
            }
          } else {
            if (maxMemory > 0 && value > maxMemory) {
              problem = `is more than maxMemory ${ctx.params.maxMemory}`;
            } else if (minMemory > 0 && value < minMemory) {
              problem = `is ${value} bytes${memoryHint(String(written))}`;
            }
          }
          if (problem === "") continue;
          findings.push({
            key: `${c.name}/${section}.${name}`,
            container: c.name,
            message: `container ${c.name}: ${section}.${name} ${written} ${problem}`,
            details: { resource: `${section}.${name}`, value: String(written) },
          });
        }
      }
    }
    return findings;
  },
});
