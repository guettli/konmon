// Reports workloads whose Pods a Service sends traffic to, while the
// container serving it has no readiness probe: the Service sends traffic as
// soon as the process starts, before it can answer, and keeps sending it to
// a process which hangs. The serving container is the one whose port the
// Service's targetPort names or numbers; if no port matches (numeric ports
// need not be declared), the workload is reported only if none of its
// containers has a readiness probe (a Pod is ready only when all its
// containers are).
//
// Not reported: workloads which no Service selects (operators, workers),
// Services with publishNotReadyAddresses (they send traffic to unready Pods
// on purpose, e.g. for peer discovery), and a missing liveness probe (one
// which is too strict restarts healthy containers).
//
// Off by default: many apps start fast enough that it does not matter.
// Enable it with --enable-check container-probes-missing.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/container.go (codes 102, 104), narrowed to what a Service
// depends on.
import { defineCheck, podSpec, podLabels, matchesSelector, Finding } from "konmon";

function list(names: string[]): string {
  return names.length === 1 ? `Service ${names[0]} sends` : `Services ${names.join(", ")} send`;
}

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
      .filter((s) => s.spec?.type !== "ExternalName" && !s.spec?.publishNotReadyAddresses)
      .filter((s) => Object.keys(s.spec?.selector ?? {}).length > 0)
      .filter((s) => matchesSelector(labels, { matchLabels: s.spec.selector }));
    if (services.length === 0) return [];
    const containers: any[] = podSpec(obj, ctx.resource.resource)?.containers ?? [];
    // container name -> Services whose targetPort it serves
    const serving = new Map<string, Set<string>>();
    let unmatched = new Set<string>();
    for (const s of services) {
      for (const p of s.spec?.ports ?? []) {
        const target = p.targetPort ?? p.port;
        const protocol = p.protocol ?? "TCP";
        const c = containers.find((c) =>
          (c.ports ?? []).some(
            (cp: any) =>
              (cp.protocol ?? "TCP") === protocol &&
              (typeof target === "string" ? cp.name === target : cp.containerPort === target),
          ),
        );
        if (!c) {
          unmatched.add(s.metadata.name);
          continue;
        }
        const set = serving.get(c.name) ?? new Set<string>();
        set.add(s.metadata.name);
        serving.set(c.name, set);
      }
    }
    const findings: Finding[] = [];
    for (const c of containers) {
      const names = serving.get(c.name);
      if (!names || c.readinessProbe) continue;
      findings.push({
        key: c.name,
        container: c.name,
        message: `container ${c.name} has no readiness probe, but ${list([...names].sort())} traffic to it as soon as it starts`,
      });
    }
    // Ports which no container declares: the Pod as a whole.
    unmatched = new Set([...unmatched].filter((n) => ![...serving.values()].some((s) => s.has(n))));
    if (unmatched.size > 0 && findings.length === 0 && !containers.some((c) => c.readinessProbe)) {
      findings.push({
        message: `no container has a readiness probe, but ${list([...unmatched].sort())} traffic to the Pods as soon as they start`,
      });
    }
    return findings;
  },
});
