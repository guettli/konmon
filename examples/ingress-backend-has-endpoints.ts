// Example from the README: an Ingress whose backend Service has no ready
// endpoints. Run its tests with: konmon checks test --checks examples
import { defineCheck } from "konmon";

export default defineCheck({
  name: "ingress-backend-has-endpoints",
  category: "health",                         // "health" alerts, "hygiene" is report-only
  severity: "warning",                        // critical | warning | info
  match: { resources: ["ingresses.networking.k8s.io"] },
  reads: ["endpointslices.discovery.k8s.io"], // other types this check reads
  params: { minEndpoints: 1 },                // --check-param ingress-backend-has-endpoints.minEndpoints=2
  check(ingress, ctx) {
    const findings = [];
    for (const rule of ingress.spec?.rules ?? []) {
      for (const p of rule.http?.paths ?? []) {
        const svc = p.backend?.service?.name;
        if (!svc) continue;
        const slices = ctx.list("endpointslices.discovery.k8s.io", {
          namespace: ingress.metadata.namespace,
          labels: { "kubernetes.io/service-name": svc },
        });
        const ready = slices.flatMap((s) => s.endpoints ?? []).filter((e) => e.conditions?.ready).length;
        if (ready < ctx.params.minEndpoints) {
          findings.push({ key: svc, message: `backend ${svc} has ${ready} ready endpoints` });
        }
      }
    }
    return findings;
  },
});
