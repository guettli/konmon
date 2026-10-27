// Reports workloads whose Pods use a ServiceAccount which does not exist:
// the Pods cannot be created.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/dp.go, sts.go, ds.go, job.go, cronjob.go (codes 307/507).
import { defineCheck, podSpec } from "konmon";

export default defineCheck({
  name: "serviceaccount-missing",
  match: {
    resources: ["deployments.apps", "statefulsets.apps", "daemonsets.apps", "jobs.batch", "cronjobs.batch"],
  },
  reads: ["serviceaccounts"],
  check(obj, ctx) {
    const sa = podSpec(obj, ctx.resource.resource)?.serviceAccountName;
    if (!sa || sa === "default") return [];
    if (ctx.get("serviceaccounts", obj.metadata.namespace ?? null, sa)) return [];
    return [{ message: `Pods use the ServiceAccount ${sa} which does not exist` }];
  },
});
