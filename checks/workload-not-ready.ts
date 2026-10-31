// Reports StatefulSets and DaemonSets with fewer ready Pods than wanted.
// Deployments report this in their Available condition; these two kinds
// have no conditions. Only after 10 minutes: an ordered StatefulSet start
// or a node reboot takes a while. A DaemonSet Pod on a NotReady node is
// also reported by the Node's conditions.
//
// During a rollout the number of updated Pods is part of the key: each
// step of a progressing rollout starts the 10 minutes again, a rollout
// which is stuck (a new revision which crash-loops) is reported.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/statefulset.go and daemonset.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "workload-not-ready",
  match: { resources: ["statefulsets.apps", "daemonsets.apps"] },
  forSeconds: 600,
  check(obj, ctx) {
    const st = obj.status ?? {};
    if ((st.observedGeneration ?? 0) < (obj.metadata.generation ?? 0)) return [];
    if (ctx.resource.resource === "statefulsets") {
      const want = obj.spec?.replicas ?? 1;
      const ready = st.readyReplicas ?? 0;
      if (ready >= want) return [];
      const updated = st.updatedReplicas ?? want;
      const rollingOut =
        (st.updateRevision && st.currentRevision && st.updateRevision !== st.currentRevision) || updated < want;
      return [
        {
          ...(rollingOut ? { key: `rollout-${updated}` } : {}),
          message: `StatefulSet has ${ready}/${want} ready replicas` + (rollingOut ? `, ${updated} updated` : ""),
          details: { ready, desired: want },
        },
      ];
    }
    const want = st.desiredNumberScheduled ?? 0;
    const ready = st.numberReady ?? 0;
    if (ready >= want) return [];
    const updated = st.updatedNumberScheduled ?? want;
    return [
      {
        ...(updated < want ? { key: `rollout-${updated}` } : {}),
        message: `DaemonSet has ${ready}/${want} ready Pods` + (updated < want ? `, ${updated} updated` : ""),
        details: { ready, desired: want },
      },
    ];
  },
});
