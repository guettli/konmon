// Reports Service ports whose named targetPort is not a port of any Pod
// the Service selects: the EndpointSlices list those Pods without that
// port, so traffic to it fails although the Pods are ready. Only names are
// checked: a numeric targetPort works whether or not a container declares
// it. Like the EndpointSlice controller, the name and the protocol must
// match a port of a container or of a sidecar (an init container with
// restartPolicy Always). Services without selector, ExternalName Services
// and Services without running Pods are skipped.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/svc.go (code 1106).
import { defineCheck, Finding, KubeObject } from "konmon";

export default defineCheck({
  name: "service-target-port-missing",
  match: { resources: ["services"] },
  reads: ["pods"],
  check(svc, ctx) {
    if (svc.spec?.type === "ExternalName") return [];
    const selector: Record<string, string> = svc.spec?.selector ?? {};
    if (Object.keys(selector).length === 0) return [];
    const named = (svc.spec?.ports ?? []).filter((p: any) => typeof p.targetPort === "string");
    if (named.length === 0) return [];
    const pods = ctx
      .list("pods", { namespace: svc.metadata.namespace, labels: selector })
      .filter((p) => p.status?.phase !== "Succeeded" && p.status?.phase !== "Failed");
    if (pods.length === 0) return [];
    const has = (pod: KubeObject, name: string, protocol: string) => {
      const spec = pod.spec ?? {};
      const containers = [
        ...(spec.containers ?? []),
        ...(spec.initContainers ?? []).filter((c: any) => c.restartPolicy === "Always"),
      ];
      return containers.some((c: any) =>
        (c.ports ?? []).some((p: any) => p.name === name && (p.protocol ?? "TCP") === protocol),
      );
    };
    const findings: Finding[] = [];
    for (const p of named) {
      const protocol = p.protocol ?? "TCP";
      if (pods.some((pod) => has(pod, p.targetPort, protocol))) continue;
      const port = p.name ? `${p.name} (${p.port})` : `${p.port}`;
      findings.push({
        key: p.name || `${p.port}`,
        message: `Service port ${port} targets the port ${p.targetPort}/${protocol}, which none of the ${pods.length} selected Pods has`,
      });
    }
    return findings;
  },
});
