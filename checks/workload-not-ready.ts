// Reports StatefulSets and DaemonSets with fewer ready Pods than wanted.
// Deployments report this in their Available condition; these two kinds
// have no conditions. Only after the controller has seen the latest spec
// (observedGeneration), so a rollout is not reported; konmon's --for delay
// covers the time Pods need to start.
//
// Ported from k8sgpt (github.com/k8sgpt-ai/k8sgpt, Apache-2.0),
// pkg/analyzer/statefulset.go and daemonset.go.
import { defineCheck } from "konmon";

export default defineCheck({
  name: "workload-not-ready",
  match: { resources: ["statefulsets.apps", "daemonsets.apps"] },
  check(obj, ctx) {
    const st = obj.status ?? {};
    if ((st.observedGeneration ?? 0) < (obj.metadata.generation ?? 0)) return [];
    if (ctx.resource.resource === "statefulsets") {
      const want = obj.spec?.replicas ?? 1;
      const ready = st.readyReplicas ?? 0;
      if (ready >= want) return [];
      return [{ message: `StatefulSet has ${ready}/${want} ready replicas`, details: { ready, desired: want } }];
    }
    const want = st.desiredNumberScheduled ?? 0;
    const ready = st.numberReady ?? 0;
    if (ready >= want) return [];
    return [{ message: `DaemonSet has ${ready}/${want} ready Pods`, details: { ready, desired: want } }];
  },
});
