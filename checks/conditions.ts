// Reports unhealthy entries of status.conditions.
//
// Ported from check-conditions (github.com/guettli/check-conditions). The
// hard part of a generic condition check is polarity: for most condition
// types True is good (Ready, Available), for some False is good
// (MemoryPressure, Degraded), and some are fine either way. The tables below
// collect that knowledge. If a condition is reported wrongly, teach konmon
// in the "conditions" section of the config file (param "rules"), add it
// here, or override this check with your own file of the same name.
import { defineCheck, parseTime, Condition, KubeObject } from "konmon";

// Condition types which can be True or False, and both values are fine.
const typesToSkip = [
  "DisruptionAllowed",
  "LoadBalancerAttachedToNetwork",
  "NetworkAttached",
  "PodReadyToStartContainers", // completed pods have "False".
  "RefVersionsUpToDate", // happens during api version transition.
];

// True is healthy, per resource (name of the resource, any group).
const positiveByResource: Record<string, string[]> = {
  extensionconfigs: ["Discovered"], // runtime.cluster.x-k8s.io
  hetznerclusters: ["ControlPlaneEndpointSet"],
  hetznerbaremetalmachines: ["AssociateBMHCondition"],
  horizontalpodautoscalers: ["AbleToScale", "ScalingActive"],
  hetznerbaremetalhosts: ["RootDeviceHintsValidated", "NodeBootIDRetrieved"],
  clusters: [
    "ConsistentSystemID", // postgresql.cnpg.io/v1
    "ContinuousArchiving", // postgresql.cnpg.io/v1
    "RemoteConnectionProbe", // capi
  ],
  clusteraddons: ["ClusterAddonConfigValidated", "ClusterAddonHelmChartUntarred"],
  engineimages: ["ready"], // Longhorn
  gitrepositories: ["ArtifactInStorage"], // source.toolkit.fluxcd.io/v1
  pods: ["mysql.oracle.com/configured"], // mysql-operator
  nodes: [
    "Schedulable", // Longhorn
    "MountPropagation", // Longhorn
    "RequiredPackages", // Longhorn
    "KernelModulesLoaded", // Longhorn
    "EtcdIsVoter",
  ],
  machines: ["NodeKubeadmLabelsAndTaintsSet"],
  autopilotclusters: ["ClusterRunning"],
  applicationsets: ["ParametersGenerated"],
};

// True is healthy for any resource. Matched exactly.
const positiveAnyResource = [
  "Connected", // link/cluster is connected
  "DataCleanable", // Longhorn: informational, not a problem
  "Resolved", // dependency resolution succeeded (e.g. crossplane locks)
  "Responsive", // watch circuit closed / responsive
  "ValidPipeline", // crossplane composition pipeline is valid
];

const positiveSuffixes = [
  "Applied",
  "Approved",
  "Available",
  "Built",
  "Complete",
  "Created",
  "Deployed",
  "Downloaded",
  "Established",
  "Healthy",
  "Initialized",
  "Install",
  "Installed",
  "LoadBalancerAttached",
  "NamesAccepted",
  "Passed",
  "PodScheduled",
  "Progressing",
  "Provided",
  "ProviderUpgraded",
  "Provisioned",
  "Reachable",
  "Ready",
  "Reconciled",
  "Reconciler",
  "RemediationAllowed",
  "Resized",
  "Succeeded",
  "Synced",
  "Upgrade",
  "UpToDate",
  "Valid",
  "SuccessCriteriaMet",
  "RunningDesiredVersion", // elasticsearches
  "ready", // perconaservermongodbs
  "sharding", // perconaservermongodbs
  "Conformant", // customresourcedefinitions KubernetesAPIApprovalPolicyConformant
  "NoWarnings", // rabbitmqclusters
  "ReconcileSuccess", // rabbitmqclusters
];

const positivePrefixes = ["Created"];

// False is healthy for any resource. Matched exactly.
const negativeAnyResource = [
  "Error", // an Error condition is only interesting when not False
  "Suspended", // e.g. agents.x-k8s.io sandboxes; added after the port from check-conditions
];

const capiRollout = ["RollingOut", "ScalingDown", "ScalingUp", "Remediating"];

// False is healthy, per resource.
const negativeByResource: Record<string, string[]> = {
  nodes: [
    "KernelDeadlock",
    "ReadonlyFilesystem",
    "FrequentUnregisterNetDevice",
    "NTPProblem",
    "CperHardwareErrorFatal",
    "DisksFailure",
    "KubeletNeedsRestart",
    "XfsShutdown",
    // Custom node health conditions (e.g. cloud-exit nodes): False means healthy.
    "CertRenewalFailing",
    "ClockNotSynchronized",
    "DiskTemperatureHigh",
    "DiskUsageHigh",
    "DiskWearHigh",
    "GpuFallenOffBus",
    "NodeInfoStale",
    "NodeServiceDown",
    "NodeTampered",
    "ProxyNotServing",
    "SealedOSTampered",
    "ServiceNotRecovering",
    "TunnelDisconnected",
    "VerityCorruption",
  ],
  horizontalpodautoscalers: ["ScalingLimited"],
  clusters: capiRollout, // capi
  kubeadmcontrolplanes: capiRollout,
  machinedeployments: capiRollout,
  machinesets: ["ScalingDown", "ScalingUp", "Remediating"],
  machines: ["Updating"],
  applicationsets: ["ErrorOccurred"],
};

const negativeSuffixes = ["Unavailable", "Pressure", "Dangling", "Unhealthy", "Paused", "Deleting", "Failed", "Degraded"];

// Healthy states which the rules above would report. Matched against
// `resource Type=Status Reason "message"`. To add one, take a reported line
// and remove namespace, name and age.
const ignore: RegExp[] = [
  // Cluster API
  /machinesets MachinesReady=False Deleted @.*/,
  /machinesets (MachinesReady|Ready)=False DrainingFailed @./,
  /machinesets Ready=False Deleted @.*/,
  /machinesets (MachinesReady|Ready)=False NodeNotFound.*/,
  /machinedeployments (MachineSetReady|Ready)=False Deleted.*/,

  // Longhorn
  /backuptargets Unavailable=True Unavailable "backup target URL is empty"/,
  /engines InstanceCreation=True/,
  /engines FilesystemReadOnly=False/,
  /replicas InstanceCreation=True/,
  /replicas FilesystemReadOnly=False/,
  /replicas WaitForBackingImage=False/,
  /volumes WaitForBackingImage=False/,
  /volumes TooManySnapshots=False/,
  /volumes Scheduled=True/,
  /volumes Restore=False/,

  /perconaxtradbclusters tls=enabled/,

  // liqo reports health via non-boolean status values ("Accepted",
  // "Running"). Each regex pins the healthy value: an unhealthy liqo
  // condition has a different status and is still reported.
  /foreignclusters APIServerStatus=Established/,
  /configurations NetworkCIDRsConfigured=True NetworkCIDRsConfigured/, // networking.liqo.io
  /resourceslices Authentication=Accepted ResourceSliceAuthenticationAccepted/, // authentication.liqo.io
  /resourceslices Resources=Accepted ResourceSliceResourcesAccepted/, // authentication.liqo.io
  /virtualnodes (Node|VirtualKubelet)=Running /, // offloading.liqo.io

  // An HPA at minReplicas which would like fewer replicas: normal. (k8sgpt
  // ignores it too; added after the port from check-conditions.)
  /horizontalpodautoscalers ScalingLimited=True TooFewReplicas/,

  // Tigera / Calico operator
  /tigerastatuses Progressing=False AllObjectsAvailable/,
  /installations Progressing=False AllObjectsAvailable/,
];

// Polarity taught by the user (config file, section "conditions"), keyed
// by "resource.group" (core: "resource") or "*" for any resource; the rules
// of the resource are looked at before "*". It wins over the built-in
// polarity tables; the ignore list and done() below still apply. Other
// statuses than True and False (Unknown) are reported unless the type is in
// ignoreTypes.
interface Rules {
  healthyWhenTrue?: string[];
  healthyWhenFalse?: string[];
  ignoreTypes?: string[];
}

// taught returns the healthy status of a condition type, "skip" if both
// values are fine, or undefined if the user did not teach it.
function taught(rules: Record<string, Rules>, resource: string, t: string): "True" | "False" | "skip" | undefined {
  // Arrays only: on a string (a wrong --check-param) includes() would
  // match substrings.
  const has = (list: unknown, t: string) => Array.isArray(list) && list.includes(t);
  for (const r of [rules[resource], rules["*"]]) {
    if (r == null) continue;
    if (has(r.ignoreTypes, t)) return "skip";
    if (has(r.healthyWhenTrue, t)) return "True";
    if (has(r.healthyWhenFalse, t)) return "False";
  }
  return undefined;
}

function positive(resource: string, t: string): boolean {
  return (
    positiveAnyResource.includes(t) ||
    (positiveByResource[resource] ?? []).includes(t) ||
    positiveSuffixes.some((s) => t.endsWith(s)) ||
    positivePrefixes.some((p) => t.startsWith(p))
  );
}

function negative(resource: string, t: string): boolean {
  return (
    negativeAnyResource.includes(t) ||
    (negativeByResource[resource] ?? []).includes(t) ||
    negativeSuffixes.some((s) => t.endsWith(s)) ||
    (t.startsWith("Frequent") && t.endsWith("Restart"))
  );
}

// A finished object (completed Pod, deleted Machine) is not unhealthy.
function done(t: string, status: string, reason: string): boolean {
  if (t === "MachinesReady") {
    // machinesets ... MachinesReady=False Deleted @ Machine/name
    t = reason.split("@")[0].trim();
  }
  return (
    ["Ready", "ContainersReady", "InfrastructureReady", "MachinesReady"].includes(t) &&
    ["PodCompleted", "InstanceTerminated", "Deleted"].includes(reason) &&
    status === "False"
  );
}

interface Row {
  type: string;
  status: string;
  reason: string;
  message: string;
  since?: Date;
}

function hasRestarts(pod: KubeObject): boolean {
  for (const key of ["containerStatuses", "initContainerStatuses"]) {
    for (const s of pod.status?.[key] ?? []) {
      if ((s.restartCount ?? 0) > 0) return true;
    }
  }
  return false;
}

export default defineCheck({
  name: "conditions",
  match: {
    resources: ["*"],
    // Types without status.conditions.
    exclude: [
      "bindings",
      "componentstatuses",
      "configmaps",
      "endpoints",
      "events",
      "limitranges",
      "persistentvolumes",
      "podtemplates",
      "resourcequotas",
      "serviceaccounts",
      "controllerrevisions.apps",
      "cronjobs.batch",
    ],
  },
  params: {
    // A Pod which has not restarted yet may have ContainersReady=False and
    // Initialized=False for this long while it starts. 0 disables it.
    podStartGraceSeconds: 30,
    // Polarity of condition types, from the "conditions" section of the
    // config file: {"widgets.example.com": {"healthyWhenFalse": ["Stalled"]}}.
    rules: {} as Record<string, Rules>,
  },
  check(obj, ctx) {
    const resource = ctx.resource.resource;
    const fullResource = ctx.resource.group === "" ? resource : `${resource}.${ctx.resource.group}`;
    // hetznerbaremetalhosts store their conditions in spec.status.
    const raw = resource === "hetznerbaremetalhosts" ? obj.spec?.status?.conditions : obj.status?.conditions;
    if (raw == null) return [];
    if (!Array.isArray(raw)) {
      return [{ key: "invalid", message: "status.conditions is not a list" }];
    }
    const conditions = raw as Condition[];

    let rows: Row[] = [];
    for (const c of conditions) {
      if (c == null || typeof c !== "object") continue;
      const type = typeof c.type === "string" ? c.type : "";
      const status = typeof c.status === "string" ? c.status : "";
      const healthy = taught(ctx.params.rules, fullResource, type);
      if (healthy === "skip" || healthy === status) continue;
      if (healthy === undefined) {
        if (typesToSkip.includes(type)) continue;
        if (status === "True" && positive(resource, type)) continue;
        if (status === "False" && negative(resource, type)) continue;
      }
      const reason = typeof c.reason === "string" ? c.reason : "";
      const message = typeof c.message === "string" ? c.message : "";
      const line = `${resource} ${type}=${status} ${reason} ${JSON.stringify(message)}`;
      if (ignore.some((r) => r.test(line))) continue;
      if (done(type, status, reason)) continue;
      rows.push({ type, status, reason, message, since: parseTime(c.lastTransitionTime) });
    }

    // A Pod which starts for the first time is expected to be not ready.
    const grace = ctx.params.podStartGraceSeconds * 1000;
    if (resource === "pods" && grace > 0 && !hasRestarts(obj)) {
      rows = rows.filter(
        (r) =>
          !(
            r.status === "False" &&
            (r.type === "ContainersReady" || r.type === "Initialized") &&
            r.since !== undefined &&
            ctx.now.getTime() - r.since.getTime() < grace
          ),
      );
    }

    // Drop the summary Ready condition if another condition says the same.
    // https://pkg.go.dev/sigs.k8s.io/cluster-api/util/conditions#SetSummary
    const ready = rows.find((r) => r.type === "Ready");
    const skipReady =
      ready !== undefined &&
      rows.some(
        (r) => r.type !== "Ready" && r.message === ready.message && r.reason === ready.reason && r.status === ready.status,
      );

    // One finding per (status, reason, message): e.g. Failed and
    // FailureTarget both reporting BackoffLimitExceeded.
    const groups = new Map<string, { row: Row; types: string[] }>();
    for (const r of rows) {
      if (skipReady && r.type === "Ready") continue;
      const k = JSON.stringify([r.status, r.reason, r.message]);
      const g = groups.get(k);
      if (!g) {
        groups.set(k, { row: { ...r }, types: [r.type] });
        continue;
      }
      g.types.push(r.type);
      if (r.since && (!g.row.since || r.since < g.row.since)) g.row.since = r.since;
    }

    // Normally the condition types are the key. Some operators write the
    // same type twice; then status, reason and message keep them apart.
    const used = new Set<string>();
    return [...groups.values()].map(({ row, types }) => {
      types.sort();
      const condition = types.join("/");
      // The message is left out: it may change while the problem stays.
      let key = condition;
      if (used.has(key)) key = `${condition} ${row.status} ${row.reason}`;
      for (let i = 2; used.has(key); i++) key = `${condition} ${row.status} ${row.reason} ${i}`;
      used.add(key);
      let message = `Condition ${condition}=${row.status} ${row.reason} ${JSON.stringify(row.message)}`;
      if (resource === "deployments" && row.reason === "MinimumReplicasUnavailable") {
        const desired = obj.spec?.replicas ?? 1;
        const available = obj.status?.availableReplicas ?? 0;
        message += ` available ${available}/${desired}`;
      }
      return {
        key,
        condition,
        message,
        since: row.since,
        details: { status: row.status, reason: row.reason, message: row.message },
      };
    });
  },
});
