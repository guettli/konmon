// Reports CronJobs which have run but never succeeded, while their newest
// Job failed. Once the newest Job succeeds (or a new one runs), the
// finding resolves; lastSuccessfulTime alone would keep it until the next
// successful schedule.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/cronjob.go (code 1502), only after a first schedule.
import { defineCheck, parseTime } from "konmon";

const failed = (j: any) => (j.status?.conditions ?? []).some((c: any) => c.type === "Failed" && c.status === "True");

export default defineCheck({
  name: "cronjob-never-succeeded",
  match: { resources: ["cronjobs.batch"] },
  reads: ["jobs.batch"],
  check(cj, ctx) {
    if (cj.spec?.suspend || cj.status?.lastSuccessfulTime) return [];
    const last = parseTime(cj.status?.lastScheduleTime);
    if (!last || !ctx.has("jobs.batch")) return [];
    const jobs = ctx.list("jobs.batch", { namespace: cj.metadata.namespace, owner: cj });
    if (jobs.length === 0) return [];
    const newest = jobs.reduce((a, b) =>
      (a.metadata.creationTimestamp ?? "") >= (b.metadata.creationTimestamp ?? "") ? a : b,
    );
    if (!failed(newest)) return [];
    return [{ message: `CronJob has never succeeded; its newest Job ${newest.metadata.name} failed`, since: last }];
  },
});
