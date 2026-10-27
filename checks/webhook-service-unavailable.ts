// Reports admission webhooks whose Service does not exist or has no ready
// endpoints. With failurePolicy Fail (the default) the API server rejects
// every request the webhook matches: that is critical.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/mutating_webhook.go and validating_webhook.go, using
// EndpointSlices instead of Pod phases.
import { defineCheck, Finding } from "konmon";

export default defineCheck({
  name: "webhook-service-unavailable",
  severity: "critical",
  match: {
    resources: [
      "mutatingwebhookconfigurations.admissionregistration.k8s.io",
      "validatingwebhookconfigurations.admissionregistration.k8s.io",
    ],
  },
  reads: ["services", "endpointslices.discovery.k8s.io"],
  check(cfg, ctx) {
    const findings: Finding[] = [];
    if (!ctx.has("services")) return [];
    for (const wh of cfg.webhooks ?? []) {
      const ref = wh.clientConfig?.service;
      if (!ref) continue; // URL webhooks are outside the cluster
      const policy = wh.failurePolicy ?? "Fail";
      const details = { webhook: wh.name, failurePolicy: policy };
      // With Ignore the API server skips the webhook: degraded, not blocked.
      const severity: "critical" | "warning" = policy === "Fail" ? "critical" : "warning";
      const svc = ctx.get("services", ref.namespace, ref.name);
      if (!svc) {
        findings.push({
          key: wh.name,
          message: `webhook ${wh.name} uses the Service ${ref.namespace}/${ref.name} which does not exist (failurePolicy ${policy})`,
          details,
          severity,
        });
        continue;
      }
      if (Object.keys(svc.spec?.selector ?? {}).length === 0 || svc.spec?.type === "ExternalName") continue;
      if (!ctx.has("endpointslices.discovery.k8s.io")) continue;
      const ready = ctx
        .list("endpointslices.discovery.k8s.io", {
          namespace: ref.namespace,
          labels: { "kubernetes.io/service-name": ref.name },
        })
        .flatMap((s) => s.endpoints ?? [])
        .some((e: any) => e.conditions?.ready !== false);
      if (!ready) {
        findings.push({
          key: wh.name,
          message: `webhook ${wh.name}: Service ${ref.namespace}/${ref.name} has no ready endpoints (failurePolicy ${policy})`,
          details,
          severity,
        });
      }
    }
    return findings;
  },
});
