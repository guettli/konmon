// Reports ValidatingAdmissionPolicyBindings whose policy does not exist,
// and CEL type-check warnings of ValidatingAdmissionPolicies.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/validating_admission_policy.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "admission-policy-missing",
  match: {
    resources: [
      "validatingadmissionpolicybindings.admissionregistration.k8s.io",
      "validatingadmissionpolicies.admissionregistration.k8s.io",
    ],
  },
  check(obj, ctx) {
    if (ctx.resource.resource === "validatingadmissionpolicies") {
      return (obj.status?.typeChecking?.expressionWarnings ?? []).map((w: any) => ({
        key: w.fieldRef,
        message: `CEL type-check warning at ${w.fieldRef}: ${w.warning}`,
        severity: "info",
      }));
    }
    const name = obj.spec?.policyName;
    if (!name || ctx.get("validatingadmissionpolicies.admissionregistration.k8s.io", null, name)) return [];
    return [{ message: `binding references the ValidatingAdmissionPolicy ${name} which does not exist` }];
  },
});
