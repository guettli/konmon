// Reports StatefulSets and DaemonSets with fewer ready Pods than wanted.
// Deployments report this in their Available condition; these two kinds
// have no conditions. Not during a rollout, and only after 10 minutes:
// an ordered StatefulSet start or a node reboot takes a while. A DaemonSet
// Pod on a NotReady node is also reported by the Node's conditions.
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
      const rollingOut =
        (st.updateRevision && st.currentRevision && st.updateRevision !== st.currentRevision) ||
        (st.updatedReplicas ?? want) < want;
      if (rollingOut) return [];
      if (ready >= want) return [];
      return [{ message: `StatefulSet has ${ready}/${want} ready replicas`, details: { ready, desired: want } }];
    }
    const want = st.desiredNumberScheduled ?? 0;
    const ready = st.numberReady ?? 0;
    if ((st.updatedNumberScheduled ?? want) < want) return [];
    if (ready >= want) return [];
    return [{ message: `DaemonSet has ${ready}/${want} ready Pods`, details: { ready, desired: want } }];
  },
});
