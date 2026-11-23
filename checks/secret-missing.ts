// Reports Pods which reference a Secret which does not exist (and is not
// optional): a container which starts (again) cannot get it. Only the name
// is checked: konmon never reads the data of Secrets, so a missing key is
// not seen. A missing image pull Secret is info only: the kubelet pulls
// without it, which works for public images.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/pod.go and sa.go (codes 304/305), checked from the Pod's
// side.
import { defineCheck, Finding } from "konmon";

export default defineCheck({
  name: "secret-missing",
  match: { resources: ["pods"] },
  reads: ["secrets"],
  check(pod, ctx) {
    // A finished Pod (e.g. of a Job) may reference a Secret which was
    // pruned since; it will not start again.
    const phase = pod.status?.phase;
    if (phase === "Succeeded" || phase === "Failed" || !ctx.has("secrets")) return [];
    const ns = pod.metadata.namespace ?? "";
    const findings: Finding[] = [];
    const seen = new Set<string>();
    const check = (name: string | undefined, optional: boolean | undefined, pull = false) => {
      if (!name || optional || seen.has(name)) return;
      seen.add(name);
      if (ctx.get("secrets", ns, name)) return;
      findings.push(
        pull
          ? { key: name, message: `image pull Secret ${name} does not exist`, severity: "info" }
          : { key: name, message: `Secret ${name} does not exist` },
      );
    };
    const spec = pod.spec ?? {};
    for (const c of [...(spec.initContainers ?? []), ...(spec.containers ?? []), ...(spec.ephemeralContainers ?? [])]) {
      for (const e of c.env ?? []) {
        const ref = e.valueFrom?.secretKeyRef;
        if (ref) check(ref.name, ref.optional);
      }
      for (const ef of c.envFrom ?? []) {
        if (ef.secretRef) check(ef.secretRef.name, ef.secretRef.optional);
      }
    }
    for (const v of spec.volumes ?? []) {
      if (v.secret) check(v.secret.secretName, v.secret.optional);
      for (const src of v.projected?.sources ?? []) {
        if (src.secret) check(src.secret.name, src.secret.optional);
      }
    }
    // Last, so a Secret which is also mounted is reported as such.
    for (const s of spec.imagePullSecrets ?? []) check(s.name, false, true);
    return findings;
  },
});
