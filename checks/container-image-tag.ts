// Reports container images without a tag or with the tag "latest": which
// version runs depends on when the node pulled it. Images pinned by digest
// are fine.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/container.go (codes 100, 101), with proper parsing of
// registry ports.
import { defineCheck, podSpec, Finding, KubeObject } from "konmon";

// Pods of a workload, ReplicaSets of a Deployment and Jobs of a CronJob
// are reported via the workload. Objects of other owners (an operator's
// custom resource, an Argo Rollout, a static Pod's Node) are reported
// themselves.
function reportedViaOwner(obj: KubeObject, resource: string): boolean {
  const kinds: Record<string, string[]> = {
    pods: ["ReplicaSet", "StatefulSet", "DaemonSet", "Job"],
    replicasets: ["Deployment"],
    jobs: ["CronJob"],
  };
  return (obj.metadata.ownerReferences ?? []).some((r) => r.controller && (kinds[resource] ?? []).includes(r.kind));
}

function tag(image: string): string | undefined {
  if (image.includes("@")) return "@digest";
  const lastSlash = image.lastIndexOf("/");
  const colon = image.indexOf(":", lastSlash + 1);
  return colon < 0 ? undefined : image.slice(colon + 1);
}

export default defineCheck({
  name: "container-image-tag",
  category: "hygiene",
  severity: "info",
  match: {
    resources: [
      "pods",
      "deployments.apps",
      "statefulsets.apps",
      "daemonsets.apps",
      "replicasets.apps",
      "jobs.batch",
      "cronjobs.batch",
    ],
  },
  check(obj, ctx) {
    if (reportedViaOwner(obj, ctx.resource.resource)) return [];
    const spec = podSpec(obj, ctx.resource.resource);
    const findings: Finding[] = [];
    for (const c of [...(spec?.initContainers ?? []), ...(spec?.containers ?? [])]) {
      const t = tag(String(c.image ?? ""));
      if (t === undefined) {
        findings.push({
          key: c.name,
          container: c.name,
          message: `container ${c.name} uses image ${c.image} without a tag`,
        });
      } else if (t === "latest") {
        findings.push({ key: c.name, container: c.name, message: `container ${c.name} uses image ${c.image}` });
      }
    }
    return findings;
  },
});
