// Reports Pods which reference a ConfigMap, or a key of it, which does not
// exist (and is not optional): the container cannot start.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/cm.go (code 401), checked from the Pod's side.
import { defineCheck, Finding, KubeObject } from "konmon";

export default defineCheck({
  name: "configmap-key-missing",
  match: { resources: ["pods"] },
  reads: ["configmaps"],
  check(pod, ctx) {
    // A finished Pod (e.g. of a Job) may reference a ConfigMap which was
    // pruned since; it will not start again.
    const phase = pod.status?.phase;
    if (phase === "Succeeded" || phase === "Failed" || !ctx.has("configmaps")) return [];
    const ns = pod.metadata.namespace ?? "";
    const findings: Finding[] = [];
    const seen = new Set<string>();
    const report = (cm: string, key: string | undefined) => {
      const id = `${cm}/${key ?? ""}`;
      if (seen.has(id)) return;
      seen.add(id);
      findings.push({
        key: id,
        message: key === undefined ? `ConfigMap ${cm} does not exist` : `ConfigMap ${cm} has no key ${key}`,
      });
    };
    const check = (name: string | undefined, keys: (string | undefined)[], optional: boolean | undefined) => {
      if (!name || optional) return;
      const cm: KubeObject | null = ctx.get("configmaps", ns, name);
      if (!cm) return report(name, undefined);
      for (const k of keys) {
        if (k !== undefined && !(k in (cm.data ?? {})) && !(k in (cm.binaryData ?? {}))) report(name, k);
      }
    };
    const spec = pod.spec ?? {};
    for (const c of [...(spec.initContainers ?? []), ...(spec.containers ?? []), ...(spec.ephemeralContainers ?? [])]) {
      for (const e of c.env ?? []) {
        const ref = e.valueFrom?.configMapKeyRef;
        if (ref) check(ref.name, [ref.key], ref.optional);
      }
      for (const ef of c.envFrom ?? []) {
        if (ef.configMapRef) check(ef.configMapRef.name, [], ef.configMapRef.optional);
      }
    }
    for (const v of spec.volumes ?? []) {
      if (v.configMap)
        check(
          v.configMap.name,
          (v.configMap.items ?? []).map((i: any) => i.key),
          v.configMap.optional,
        );
      for (const src of v.projected?.sources ?? []) {
        if (src.configMap)
          check(
            src.configMap.name,
            (src.configMap.items ?? []).map((i: any) => i.key),
            src.configMap.optional,
          );
      }
    }
    return findings;
  },
});
