// Reports CronJobs which have run but never succeeded.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/cronjob.go (code 1502), only after a first schedule.
import { defineCheck, parseTime } from "konmon";

export default defineCheck({
  name: "cronjob-never-succeeded",
  match: { resources: ["cronjobs.batch"] },
  reads: ["jobs.batch"],
  check(cj, ctx) {
    if (cj.spec?.suspend || cj.status?.lastSuccessfulTime) return [];
    const last = parseTime(cj.status?.lastScheduleTime);
    if (!last) return [];
    // A first Job which is still running is not a failure.
    if ((cj.status?.active ?? []).length > 0) return [];
    const failed = ctx.list("jobs.batch", { namespace: cj.metadata.namespace, owner: cj }).filter((j) =>
      (j.status?.conditions ?? []).some((c: any) => c.type === "Failed" && c.status === "True"),
    );
    if (failed.length === 0) return [];
    return [{ message: `CronJob has never succeeded (${failed.length} failed Jobs)`, since: last }];
  },
});
