// Reports Services whose Pods exist but none is ready: traffic to them
// fails. A Service without any matching (running) Pod is not reported:
// scale-to-zero, a suspended app or a Service for Job Pods is normal.
// Services without selector (endpoints managed by hand or by an operator)
// and ExternalName Services are skipped.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/service.go, using EndpointSlices instead of the deprecated
// Endpoints.
import { defineCheck, matchesSelector } from "konmon";

export default defineCheck({
  name: "service-without-endpoints",
  match: { resources: ["services"] },
  reads: ["endpointslices.discovery.k8s.io", "pods"],
  check(svc, ctx) {
    if (svc.spec?.type === "ExternalName") return [];
    const selector: Record<string, string> = svc.spec?.selector ?? {};
    if (Object.keys(selector).length === 0) return [];
    if (!ctx.has("endpointslices.discovery.k8s.io")) return [];
    const pods = ctx
      .list("pods", { namespace: svc.metadata.namespace })
      .filter((p) => p.status?.phase !== "Succeeded" && p.status?.phase !== "Failed")
      .filter((p) => matchesSelector(p.metadata.labels, { matchLabels: selector }));
    if (pods.length === 0) return [];
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
    return [{ message: `Service has no ready endpoints, but ${pods.length} Pods match its selector ${sel}` }];
  },
});
