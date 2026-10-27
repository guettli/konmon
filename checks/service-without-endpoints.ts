// Reports Services with a selector which have no ready endpoints: traffic
// to them fails. Services without selector (endpoints managed by hand or by
// an operator) and ExternalName Services are skipped.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/service.go, using EndpointSlices instead of the deprecated
// Endpoints.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "service-without-endpoints",
  match: { resources: ["services"] },
  reads: ["endpointslices.discovery.k8s.io"],
  check(svc, ctx) {
    if (svc.spec?.type === "ExternalName") return [];
    const selector: Record<string, string> = svc.spec?.selector ?? {};
    if (Object.keys(selector).length === 0) return [];
    const slices = ctx.list("endpointslices.discovery.k8s.io", {
      namespace: svc.metadata.namespace,
      labels: { "kubernetes.io/service-name": svc.metadata.name },
    });
    const endpoints = slices.flatMap((s) => s.endpoints ?? []);
    // An endpoint without conditions.ready counts as ready (API docs).
    const ready = endpoints.filter((e: any) => e.conditions?.ready !== false).length;
    if (ready > 0) return [];
    const sel = Object.keys(selector)
      .sort()
      .map((k) => `${k}=${selector[k]}`)
      .join(",");
    if (endpoints.length > 0) {
      return [{ message: `Service has ${endpoints.length} endpoints, none ready (selector ${sel})` }];
    }
    return [{ message: `Service has no endpoints: no Pod matches the selector ${sel}` }];
  },
});
