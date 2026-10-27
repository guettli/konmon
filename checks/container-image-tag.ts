// Reports container images without a tag or with the tag "latest": which
// version runs depends on when the node pulled it. Images pinned by digest
// are fine.
//
// Ported from Popeye (github.com/derailed/popeye, Apache-2.0),
// internal/lint/container.go (codes 100, 101), with proper parsing of
// registry ports.
import { defineCheck, podSpec, Finding } from "konmon";

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
    resources: ["pods", "deployments.apps", "statefulsets.apps", "daemonsets.apps", "cronjobs.batch"],
  },
  check(obj, ctx) {
    // Pods of a workload are reported via the workload.
    if (ctx.resource.resource === "pods" && (obj.metadata.ownerReferences ?? []).length > 0) return [];
    const spec = podSpec(obj, ctx.resource.resource);
    const findings: Finding[] = [];
    for (const c of [...(spec?.initContainers ?? []), ...(spec?.containers ?? [])]) {
      const t = tag(String(c.image ?? ""));
      if (t === undefined) {
        findings.push({ key: c.name, container: c.name, message: `container ${c.name} uses image ${c.image} without a tag` });
      } else if (t === "latest") {
        findings.push({ key: c.name, container: c.name, message: `container ${c.name} uses image ${c.image}` });
      }
    }
    return findings;
  },
});
