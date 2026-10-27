// Reports Pod containers which restarted often and are unhealthy right now.
// A container which starts again and again does not show up in
// status.conditions. Once it is ready again the finding resolves:
// restartCount only grows, so without that gate a recovered Pod would be
// reported forever.
//
// Ported from check-conditions (github.com/guettli/check-conditions).
import { defineCheck } from "konmon";

export default defineCheck({
  name: "pod-container-restarts",
  match: { resources: ["pods"] },
  params: {
    // Report a container after this many restarts. 0 disables the check.
    restarts: 5,
  },
  check(pod, ctx) {
    const limit = ctx.params.restarts;
    if (limit <= 0) return [];
    const findings = [];
    for (const key of ["containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"]) {
      for (const s of pod.status?.[key] ?? []) {
        const restarts = s.restartCount ?? 0;
        if (typeof restarts !== "number" || restarts < limit) continue;
        const waiting = s.state?.waiting;
        // Only while the container is in trouble: waiting (backing off) or
        // explicitly not ready.
        if (!waiting && s.ready !== false) continue;
        const state = waiting?.reason || "not ready";
        const name = s.name || "<unknown>";
        findings.push({
          key: name,
          container: name,
          message: `container ${name} restarted ${restarts} times, currently ${state}`,
          details: { restartCount: restarts, state },
        });
      }
    }
    return findings;
  },
});
