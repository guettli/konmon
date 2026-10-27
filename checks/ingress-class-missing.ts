// Reports Ingresses whose ingressClassName names an IngressClass which does
// not exist, or which name no class at all while there is no default class:
// no controller serves them. The legacy kubernetes.io/ingress.class
// annotation names a controller class, which needs no IngressClass object
// (Kong, HAProxy, older nginx), so it is not checked. An Ingress with a
// load balancer address in its status is served.
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
    if ((ing.status?.loadBalancer?.ingress ?? []).length > 0) return [];
    if (ing.metadata.annotations?.["kubernetes.io/ingress.class"]) return [];
    if (!ctx.has("ingressclasses.networking.k8s.io")) return [];
    const cls: string | undefined = ing.spec?.ingressClassName;
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
