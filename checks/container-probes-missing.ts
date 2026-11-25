// Reports containers without a readiness probe in long-running workloads
// whose Pods a Service selects: the Service sends traffic to a Pod as soon
// as its process started, before it can answer, and keeps sending it to a
// Pod which hangs. Workloads which no Service selects (operators,
// controllers, workers) are not reported, and neither is a missing liveness
// probe: one which is too strict restarts healthy containers.
//
// Off by default: many apps start fast enough that it does not matter.
// Enable it with --enable-check container-probes-missing.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/container.go (codes 102, 104), narrowed to what a Service
// depends on.
import { defineCheck, podSpec, podLabels, matchesSelector, Finding } from "konmon";

export default defineCheck({
  name: "container-probes-missing",
  category: "hygiene",
  severity: "info",
  enabled: false,
  // Jobs run to completion. Pods are reported via their workload.
  match: { resources: ["deployments.apps", "statefulsets.apps", "daemonsets.apps"] },
  reads: ["services"],
  check(obj, ctx) {
    const labels = podLabels(obj, ctx.resource.resource);
    const services = ctx
      .list("services", { namespace: obj.metadata.namespace })
      .filter((s) => s.spec?.type !== "ExternalName" && Object.keys(s.spec?.selector ?? {}).length > 0)
      .filter((s) => matchesSelector(labels, { matchLabels: s.spec.selector }))
      .map((s) => s.metadata.name)
      .sort();
    if (services.length === 0) return [];
    const spec = podSpec(obj, ctx.resource.resource);
    const findings: Finding[] = [];
    // Sidecars (init containers with restartPolicy Always) do not gate
    // readiness of the Pod unless they have a probe; only the main
    // containers are checked.
    for (const c of spec?.containers ?? []) {
      if (c.readinessProbe) continue;
      findings.push({
        key: c.name,
        container: c.name,
        message: `container ${c.name} has no readiness probe, but Service ${services.join(", ")} sends traffic to it as soon as it started`,
      });
    }
    return findings;
  },
});
