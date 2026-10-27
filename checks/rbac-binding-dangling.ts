// Reports RoleBindings and ClusterRoleBindings which reference a Role or
// ClusterRole that does not exist. Missing ServiceAccount subjects are not
// reported (Popeye does): they are common and harmless, e.g. bindings of
// controllers which k3s runs in-process or Flux controllers which are not
// installed.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/rb.go and crb.go (code 1300).
import { defineCheck } from "konmon";

export default defineCheck({
  name: "rbac-binding-dangling",
  category: "hygiene",
  severity: "warning",
  match: {
    resources: ["rolebindings.rbac.authorization.k8s.io", "clusterrolebindings.rbac.authorization.k8s.io"],
  },
  reads: ["roles.rbac.authorization.k8s.io", "clusterroles.rbac.authorization.k8s.io"],
  check(b, ctx) {
    const ref = b.roleRef ?? {};
    const resource = ref.kind === "Role" ? "roles.rbac.authorization.k8s.io" : "clusterroles.rbac.authorization.k8s.io";
    if (!ctx.has(resource)) return [];
    const role =
      ref.kind === "Role"
        ? ctx.get("roles.rbac.authorization.k8s.io", b.metadata.namespace ?? null, ref.name)
        : ctx.get("clusterroles.rbac.authorization.k8s.io", null, ref.name);
    if (role) return [];
    return [{ message: `binding references the ${ref.kind} ${ref.name} which does not exist` }];
  },
});
