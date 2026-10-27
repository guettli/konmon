// Reports ValidatingAdmissionPolicyBindings whose policy does not exist.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/validating_admission_policy.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "admission-policy-missing",
  match: { resources: ["validatingadmissionpolicybindings.admissionregistration.k8s.io"] },
  reads: ["validatingadmissionpolicies.admissionregistration.k8s.io"],
  check(binding, ctx) {
    const name = binding.spec?.policyName;
    if (!name || !ctx.has("validatingadmissionpolicies.admissionregistration.k8s.io")) return [];
    if (ctx.get("validatingadmissionpolicies.admissionregistration.k8s.io", null, name)) return [];
    return [{ message: `binding references the ValidatingAdmissionPolicy ${name} which does not exist` }];
  },
});
