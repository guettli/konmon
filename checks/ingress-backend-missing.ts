// Reports Ingress backends which point to a Service (or a Service port)
// which does not exist, and load balancer errors in the Ingress status.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/ingress.go, and Popeye (github.com/derailed/popeye,
// Apache-2.0), internal/lint/ing.go (codes 1400, 1402, 1404).
import { defineCheck, Finding, KubeObject } from "konmon";

export default defineCheck({
  name: "ingress-backend-missing",
  match: { resources: ["ingresses.networking.k8s.io"] },
  reads: ["services"],
  check(ing, ctx) {
    const ns = ing.metadata.namespace ?? "";
    const findings: Finding[] = [];
    if (!ctx.has("services")) return [];
    const seen = new Set<string>();
    const checkBackend = (backend: any) => {
      const svc = backend?.service;
      if (!svc) return; // backend.resource: not a Service
      const key = `${svc.name}:${svc.port?.name ?? svc.port?.number ?? ""}`;
      if (seen.has(key)) return;
      seen.add(key);
      const s: KubeObject | null = ctx.get("services", ns, svc.name);
      if (!s) {
        findings.push({ key, message: `Ingress uses the Service ${svc.name} which does not exist` });
        return;
      }
      if (s.spec?.type === "ExternalName") return; // usually without ports
      const port = svc.port ?? {};
      if (port.name === undefined && port.number === undefined) {
        findings.push({ key, message: `Ingress backend ${svc.name} has no port name or number` });
        return;
      }
      const ports: any[] = s.spec?.ports ?? [];
      const ok = ports.some((p) => (port.name !== undefined ? p.name === port.name : p.port === port.number));
      if (!ok) {
        findings.push({
          key,
          message: `Ingress uses port ${port.name ?? port.number} of Service ${svc.name}, which the Service does not define`,
        });
      }
    };
    checkBackend(ing.spec?.defaultBackend);
    for (const rule of ing.spec?.rules ?? []) {
      for (const path of rule.http?.paths ?? []) checkBackend(path.backend);
    }
    for (const lb of ing.status?.loadBalancer?.ingress ?? []) {
      for (const p of lb.ports ?? []) {
        if (p.error) {
          const key = `lb:${lb.ip ?? lb.hostname ?? ""}:${p.port}`;
          findings.push({ key, message: `load balancer port ${p.port}: ${p.error}` });
        }
      }
    }
    return findings;
  },
});
