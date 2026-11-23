// Reports PersistentVolumeClaims whose volume is almost full: the used
// space or the used inodes reach a threshold (default 90%). The numbers come
// from the kubelet of the Node which mounts the volume, so only mounted
// volumes are checked. Volumes which are read-only (access mode
// ReadOnlyMany only, or every Pod mounts them read-only) are skipped: a
// read-only image is full by design. On a shared filesystem (local-path,
// hostPath) the kubelet reports the whole filesystem of a local PV and
// nothing for a hostPath PV.
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
    const space = percent(st.usedBytes, st.availableBytes, st.capacityBytes);
    const inodes = percent(st.inodesUsed, st.inodesFree, st.inodes);
    const full = (p: number | undefined): p is number => p !== undefined && p >= ctx.params.percent;
    if (!full(space) && !full(inodes)) return [];
    // Only now: it lists the Pods of the namespace.
    if (readOnly(pvc, ctx.list("pods", { namespace: pvc.metadata.namespace }))) return [];
    // Unknown numbers are left out, not reported as 0.
    const known = (o: Record<string, number | undefined>): Record<string, number> => {
      const r: Record<string, number> = {};
      for (const [k, v] of Object.entries(o)) if (v !== undefined) r[k] = v;
      return r;
    };
    const findings = [];
    if (full(space)) {
      const free = st.availableBytes !== undefined ? `, ${bytes(st.availableBytes)} free` : "";
      const of = st.capacityBytes !== undefined ? ` of ${bytes(st.capacityBytes)}` : "";
      findings.push({
        key: "bytes",
        message: `volume is ${Math.floor(space)}% full (${bytes(st.usedBytes ?? 0)} used${of}${free})`,
        details: {
          node: st.node,
          pod: st.pod,
          ...known({ usedBytes: st.usedBytes, availableBytes: st.availableBytes, capacityBytes: st.capacityBytes }),
        },
      });
    }
    if (full(inodes)) {
      const of = st.inodes !== undefined ? ` of ${st.inodes}` : "";
      const free = st.inodesFree !== undefined ? `, ${st.inodesFree} free` : "";
      findings.push({
        key: "inodes",
        message: `volume has used ${Math.floor(inodes)}% of its inodes (${st.inodesUsed ?? 0} used${of}${free})`,
        details: {
          node: st.node,
          pod: st.pod,
          ...known({ inodesUsed: st.inodesUsed, inodesFree: st.inodesFree, inodes: st.inodes }),
        },
      });
    }
    return findings;
  },
});
