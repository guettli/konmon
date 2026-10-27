// Reports Ingresses whose IngressClass does not exist, or which name no
// class while the cluster has no default class: no controller serves them.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/ingress.go, plus the default-class lookup.
import { defineCheck } from "konmon";

// Built-in classes of GKE, which have no IngressClass object.
const builtin = ["gce", "gce-internal"];

export default defineCheck({
  name: "ingress-class-missing",
  match: { resources: ["ingresses.networking.k8s.io"] },
  reads: ["ingressclasses.networking.k8s.io"],
  check(ing, ctx) {
    const cls: string | undefined =
      ing.spec?.ingressClassName ?? ing.metadata.annotations?.["kubernetes.io/ingress.class"];
    if (cls && builtin.includes(cls)) return [];
    if (cls) {
      if (ctx.get("ingressclasses.networking.k8s.io", null, cls)) return [];
      return [{ message: `Ingress uses the IngressClass ${cls} which does not exist` }];
    }
    const hasDefault = ctx
      .list("ingressclasses.networking.k8s.io")
      .some((c) => c.metadata.annotations?.["ingressclass.kubernetes.io/is-default-class"] === "true");
    if (hasDefault) return [];
    return [{ message: "Ingress names no IngressClass and there is no default IngressClass" }];
  },
});
