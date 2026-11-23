// Reports PersistentVolumeClaims whose volume is almost full: the used
// space or the used inodes reach a threshold (default 90%). The numbers come
// from the kubelet of the Node which mounts the volume, so only mounted
// volumes are checked. Volumes which are read-only (access mode
// ReadOnlyMany only, or every Pod mounts them read-only) are skipped: a
// read-only image is full by design. On a shared filesystem (local-path,
// hostPath) the kubelet reports the whole filesystem.
import { defineCheck, type KubeObject } from "konmon";

function percent(used: number | undefined, free: number | undefined, total: number | undefined): number | undefined {
  // Like df: used / (used + free), so blocks reserved for root count as
  // not available.
  if (used !== undefined && free !== undefined && used + free > 0) return (100 * used) / (used + free);
  if (used !== undefined && total) return (100 * used) / total;
  return undefined;
}

function bytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${i === 0 ? n : n.toFixed(1)} ${units[i]}`;
}

// readOnly: the claim is only ReadOnlyMany, or all running Pods which use
// it mount it read-only (in the volume or in every volumeMount).
function readOnly(pvc: KubeObject, pods: KubeObject[]): boolean {
  const modes: string[] = pvc.spec?.accessModes ?? [];
  if (modes.length > 0 && modes.every((m) => m === "ReadOnlyMany")) return true;
  let users = 0;
  for (const p of pods) {
    if (p.status?.phase === "Succeeded" || p.status?.phase === "Failed") continue;
    for (const v of p.spec?.volumes ?? []) {
      const uses =
        v.persistentVolumeClaim?.claimName === pvc.metadata.name ||
        // A generic ephemeral volume's claim is named <pod>-<volume>.
        (v.ephemeral && `${p.metadata.name}-${v.name}` === pvc.metadata.name);
      if (!uses) continue;
      users++;
      if (v.persistentVolumeClaim?.readOnly) continue;
      const mounts = [
        ...(p.spec?.initContainers ?? []),
        ...(p.spec?.containers ?? []),
        ...(p.spec?.ephemeralContainers ?? []),
      ].flatMap((c: any) => (c.volumeMounts ?? []).filter((m: any) => m.name === v.name));
      if (mounts.length === 0 || mounts.some((m: any) => !m.readOnly)) return false;
    }
  }
  return users > 0;
}

export default defineCheck({
  name: "pvc-almost-full",
  severity: "warning",
  match: { resources: ["persistentvolumeclaims"] },
  // Pods are read to see whether the volume is mounted read-only.
  reads: ["pods"],
  uses: ["volume-stats"],
  params: { percent: 90 },
  check(pvc, ctx) {
    const st = ctx.volumeStats(pvc);
    if (!st) return [];
    if (readOnly(pvc, ctx.list("pods", { namespace: pvc.metadata.namespace }))) return [];
    const findings = [];
    const details = { node: st.node, pod: st.pod };
    const space = percent(st.usedBytes, st.availableBytes, st.capacityBytes);
    if (space !== undefined && space >= ctx.params.percent) {
      const free = st.availableBytes !== undefined ? `, ${bytes(st.availableBytes)} free` : "";
      const of = st.capacityBytes !== undefined ? ` of ${bytes(st.capacityBytes)}` : "";
      findings.push({
        key: "bytes",
        message: `volume is ${Math.floor(space)}% full (${bytes(st.usedBytes ?? 0)} used${of}${free})`,
        details: { ...details, usedBytes: st.usedBytes ?? 0, availableBytes: st.availableBytes ?? 0 },
      });
    }
    const inodes = percent(st.inodesUsed, st.inodesFree, st.inodes);
    if (inodes !== undefined && inodes >= ctx.params.percent) {
      findings.push({
        key: "inodes",
        message: `volume has used ${Math.floor(inodes)}% of its inodes (${st.inodesUsed} of ${st.inodes})`,
        details: { ...details, inodesUsed: st.inodesUsed ?? 0, inodes: st.inodes ?? 0 },
      });
    }
    return findings;
  },
});
