// Reports Gateway API objects which reference something that does not
// exist: a Gateway's GatewayClass, an HTTPRoute's parent Gateway, or an
// HTTPRoute backend Service (or its port).
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/gateway.go and httproute.go, and Popeye
// (github.com/derailed/popeye, Apache-2.0), internal/lint/gw.go and gwr.go.
import { defineCheck, Finding } from "konmon";

export default defineCheck({
  name: "gateway-references-missing",
  match: { resources: ["gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io"] },
  reads: ["gatewayclasses.gateway.networking.k8s.io", "services"],
  check(obj, ctx) {
    const ns = obj.metadata.namespace ?? "";
    if (ctx.resource.resource === "gateways") {
      const cls = obj.spec?.gatewayClassName;
      if (
        cls &&
        ctx.has("gatewayclasses.gateway.networking.k8s.io") &&
        !ctx.get("gatewayclasses.gateway.networking.k8s.io", null, cls)
      ) {
        return [{ message: `Gateway uses the GatewayClass ${cls} which does not exist` }];
      }
      return [];
    }
    const findings: Finding[] = [];
    if (!ctx.has("services")) return [];
    for (const p of obj.spec?.parentRefs ?? []) {
      const kind = p.kind ?? "Gateway";
      if ((p.group ?? "gateway.networking.k8s.io") !== "gateway.networking.k8s.io" || kind !== "Gateway") continue;
      const pns = p.namespace ?? ns;
      if (!ctx.get("gateways.gateway.networking.k8s.io", pns, p.name)) {
        findings.push({
          key: `parent/${pns}/${p.name}`,
          message: `HTTPRoute uses the Gateway ${pns}/${p.name} which does not exist`,
        });
      }
    }
    const seen = new Set<string>();
    for (const rule of obj.spec?.rules ?? []) {
      for (const b of rule.backendRefs ?? []) {
        if ((b.kind ?? "Service") !== "Service" || (b.group ?? "") !== "") continue;
        const bns = b.namespace ?? ns;
        const key = `backend/${bns}/${b.name}:${b.port ?? ""}`;
        if (seen.has(key)) continue;
        seen.add(key);
        const svc = ctx.get("services", bns, b.name);
        if (!svc) {
          findings.push({ key, message: `HTTPRoute uses the Service ${bns}/${b.name} which does not exist` });
        } else if (b.port !== undefined && !(svc.spec?.ports ?? []).some((sp: any) => sp.port === b.port)) {
          findings.push({
            key,
            message: `HTTPRoute uses port ${b.port} of Service ${bns}/${b.name}, which the Service does not define`,
          });
        }
      }
    }
    return findings;
  },
});
