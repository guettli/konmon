// Reports suspended CronJobs and Jobs, and cordoned Nodes: often a manual
// action which was not undone.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/cronjob.go and job.go, and Popeye
// (github.com/derailed/popeye, Apache-2.0), internal/lint/node.go (code 711).
import { defineCheck } from "konmon";

export default defineCheck({
  name: "suspended",
  category: "hygiene",
  severity: "info",
  match: { resources: ["cronjobs.batch", "jobs.batch", "nodes"] },
  check(obj, ctx) {
    if (ctx.resource.resource === "nodes") {
      return obj.spec?.unschedulable ? [{ message: "Node is cordoned (unschedulable)" }] : [];
    }
    return obj.spec?.suspend === true ? [{ message: `${obj.kind ?? "object"} is suspended` }] : [];
  },
});
