// Reports CEL type-check warnings of ValidatingAdmissionPolicies: usually
// an expression which refers to a field that does not exist.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/validating_admission_policy.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "admission-policy-warnings",
  category: "hygiene",
  severity: "info",
  match: { resources: ["validatingadmissionpolicies.admissionregistration.k8s.io"] },
  check(policy) {
    return (policy.status?.typeChecking?.expressionWarnings ?? []).map((w: any) => ({
      key: w.fieldRef,
      message: `CEL type-check warning at ${w.fieldRef}: ${w.warning}`,
    }));
  },
});
