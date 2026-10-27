// Reports containers which cannot start: image pull errors, bad
// configuration (a missing ConfigMap key, an invalid image name), failing
// hooks. CrashLoopBackOff is left to pod-container-restarts.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/pod.go.
import { defineCheck } from "konmon";

const errorReasons = [
  "ImagePullBackOff",
  "ErrImagePull",
  "ErrImageNeverPull",
  "InvalidImageName",
  "ImageInspectError",
  "CreateContainerConfigError",
  "CreateContainerError",
  "PreCreateHookError",
  "PreStartHookError",
  "RunContainerError",
];

export default defineCheck({
  name: "pod-container-waiting",
  match: { resources: ["pods"] },
  check(pod) {
    const findings = [];
    for (const key of ["initContainerStatuses", "containerStatuses"]) {
      for (const s of pod.status?.[key] ?? []) {
        const waiting = s.state?.waiting;
        if (!waiting || !errorReasons.includes(waiting.reason)) continue;
        const name: string = s.name || "<unknown>";
        findings.push({
          key: name,
          container: name,
          message: `container ${name} is waiting: ${waiting.reason}${waiting.message ? ": " + waiting.message : ""}`,
          details: { reason: waiting.reason },
        });
      }
    }
    return findings;
  },
});
