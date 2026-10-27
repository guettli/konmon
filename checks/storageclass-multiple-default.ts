// Reports StorageClasses marked as default when there is more than one:
// which one a new claim gets is surprising.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/storage.go.
import { defineCheck } from "konmon";

const isDefault = (sc: any) => sc.metadata?.annotations?.["storageclass.kubernetes.io/is-default-class"] === "true";

export default defineCheck({
  name: "storageclass-multiple-default",
  category: "hygiene",
  severity: "warning",
  match: { resources: ["storageclasses.storage.k8s.io"] },
  check(sc, ctx) {
    if (!isDefault(sc)) return [];
    const defaults = ctx
      .list("storageclasses.storage.k8s.io")
      .filter(isDefault)
      .map((s) => s.metadata.name)
      .sort();
    if (defaults.length < 2) return [];
    return [{ message: `${defaults.length} StorageClasses are marked as default: ${defaults.join(", ")}` }];
  },
});
