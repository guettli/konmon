// Reports objects which have been in deletion for too long: usually a
// finalizer whose controller is gone or failing.
//
// Ported from check-conditions (github.com/guettli/check-conditions).
import { defineCheck, parseTime } from "konmon";

export default defineCheck({
  name: "deletion-timestamp",
  match: {
    resources: ["*"],
    // Same types as the conditions check, so this check alone does not make
    // konmon watch (and store) events, configmaps and the like.
    exclude: [
      "bindings",
      "componentstatuses",
      "configmaps",
      "endpoints",
      "events",
      "limitranges",
      "persistentvolumes",
      "podtemplates",
      "resourcequotas",
      "serviceaccounts",
      "controllerrevisions.apps",
      "cronjobs.batch",
    ],
  },
  params: {
    // Report an object when its deletionTimestamp is older than this.
    // 0 disables the check.
    olderThanSeconds: 600,
  },
  check(obj, ctx) {
    const olderThan = ctx.params.olderThanSeconds;
    if (olderThan <= 0) return [];
    const since = parseTime(obj.metadata.deletionTimestamp);
    if (!since) return [];
    if (ctx.now.getTime() - since.getTime() <= olderThan * 1000) return [];
    const finalizers = obj.metadata.finalizers ?? [];
    return [
      {
        message: "DeletionTimestamp set",
        since,
        details: finalizers.length > 0 ? { finalizers: finalizers.join(",") } : undefined,
      },
    ];
  },
});
