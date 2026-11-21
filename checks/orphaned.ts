// Reports resources which neither a controller nor a GitOps tool manages:
// created by hand (kubectl apply, a plain helm install, the Rancher UI) and
// not in git. It assumes that everything in the cluster comes from git, so
// it is off by default.
//
// A resource is managed, and not reported, if it has an ownerReference;
// carries the tracking label or annotation of a GitOps tool which is
// installed (Flux, Argo CD, Fleet; a stale label of an uninstalled tool does
// not count); was created by the control plane, the kubelet or the
// distribution; or is reconciled by an operator which sets no
// ownerReference (a label in the domain of an installed operator's API
// group, except on a custom resource of that group; cert-manager Secrets,
// GitOps credentials, kinds on the skip list).
// A Pod or Job without owner is only reported once it is older than
// maxDebugPodAge: `kubectl run` for debugging is fine for a while.
//
// Unlike korphan: a tool or operator counts as installed if konmon watches
// one of its types (ctx.resources()), not if discovery lists its group. So
// with RBAC that hides cert-manager's types, or --exclude-resource of all of
// them, its TLS Secrets are reported. konmon strips managedFields, so there
// are no "manager X" hints, and Secrets are metadata only, so a Secret
// applied with kubectl apply gets no "kubectl apply" hint. An operator's
// label on a custom resource of its own group does not make it managed
// (korphan v0.0.15 still trusts it, #68).
//
// The annotation korphan.guettli.github.io/ignore with a reason exempts a
// resource; without a reason it does not.
//
// Ported from korphan (github.com/guettli/korphan, MIT),
// internal/korphan/classify.go and kubenorm.go.
import { defineCheck, Context, Finding, KubeObject } from "konmon";

type Labels = Readonly<Record<string, string>>;

const ignoreAnnotation = "korphan.guettli.github.io/ignore";

// The API groups of Kubernetes itself. Every other group was added by an
// operator (a CRD or an aggregated API server).
const builtinGroups = new Set([
  "",
  "apps",
  "batch",
  "autoscaling",
  "policy",
  "extensions",
  "admissionregistration.k8s.io",
  "apiextensions.k8s.io",
  "apiregistration.k8s.io",
  "apidiscovery.k8s.io",
  "authentication.k8s.io",
  "authorization.k8s.io",
  "certificates.k8s.io",
  "coordination.k8s.io",
  "discovery.k8s.io",
  "events.k8s.io",
  "flowcontrol.apiserver.k8s.io",
  "internal.apiserver.k8s.io",
  "networking.k8s.io",
  "node.k8s.io",
  "rbac.authorization.k8s.io",
  "scheduling.k8s.io",
  "storage.k8s.io",
  "storagemigration.k8s.io",
  "resource.k8s.io",
  "metrics.k8s.io",
  "admission.k8s.io",
]);

// Objects here belong to the control plane or the distribution. GitOps
// objects in these namespaces are recognized by their label first.
const systemNamespaces = new Set(["kube-system", "kube-public", "kube-node-lease"]);

// How a GitOps tool marks what it applies, and how to tell it is installed.
const managers: { name: string; labels: string[]; annotations: string[]; installed(groups: Set<string>): boolean }[] = [
  {
    name: "flux",
    labels: ["kustomize.toolkit.fluxcd.io/name", "helm.toolkit.fluxcd.io/name"],
    annotations: [],
    installed: (g) => [...g].some((group) => group.endsWith("toolkit.fluxcd.io")),
  },
  {
    name: "argocd",
    labels: ["argocd.argoproj.io/instance"],
    annotations: ["argocd.argoproj.io/tracking-id"],
    installed: (g) => g.has("argoproj.io"),
  },
  {
    name: "fleet",
    labels: ["fleet.cattle.io/bundle-name"],
    annotations: [],
    installed: (g) => g.has("fleet.cattle.io"),
  },
  {
    // cert-manager sets no ownerReference on the TLS Secret of a Certificate.
    name: "cert-manager",
    labels: [],
    annotations: ["cert-manager.io/certificate-name"],
    installed: (g) => g.has("cert-manager.io"),
  },
  {
    // Rancher/Wrangler apply controllers (the k3s deploy and helm
    // controllers, Fleet) reconcile and prune what carries this label.
    name: "objectset",
    labels: ["objectset.rio.cattle.io/hash"],
    annotations: [],
    installed: () => true,
  },
];

// Custom resources an operator creates without ownerReference or label:
// liqo's peering state. More with the param skipKinds.
const liqo: Record<string, string[]> = {
  "core.liqo.io": ["ForeignCluster"],
  "authentication.liqo.io": ["Identity", "Renew", "ResourceSlice", "Tenant"],
  "ipam.liqo.io": ["IP", "Network"],
  "networking.liqo.io": [
    "Configuration",
    "Connection",
    "FirewallConfiguration",
    "GatewayClient",
    "GatewayServer",
    "GeneveTunnel",
    "InternalFabric",
    "InternalNode",
    "PublicKey",
    "RouteConfiguration",
    "WgGatewayClient",
    "WgGatewayClientTemplate",
    "WgGatewayServer",
    "WgGatewayServerTemplate",
  ],
  "offloading.liqo.io": [
    "NamespaceMap",
    "NamespaceOffloading",
    "Quota",
    "ShadowEndpointSlice",
    "ShadowPod",
    "VirtualNode",
    "VkOptionsTemplate",
  ],
};
const defaultSkipKinds = new Set(Object.entries(liqo).flatMap(([group, kinds]) => kinds.map((k) => `${group}/${k}`)));

// Runtime state of an operator in a core kind (a ConfigMap), matched by a
// distinctive label: the key alone, or key=value when the key is generic.
const operatorStateLabels: [string, string][] = [
  ["reloader.stakater.com/meta-info", ""], // Stakater Reloader
  ["app.kubernetes.io/name", "clusterid-telemetry-configmap"], // liqo telemetry identity
];

const rancherGroups = ["management.cattle.io", "cluster.cattle.io"];

// Rancher labels whose value names the owner of an RBAC object.
const rancherOwnerLabels: [string, string][] = [
  ["authz.cluster.cattle.io/rtb-owner-updated", "rtb"],
  ["authz.cluster.cattle.io/rtb-owner", "rtb uid"],
  ["authz.cluster.cattle.io/crtb-owner", "crtb"],
  ["authz.cluster.cattle.io/clusterrole-owner", "roletemplate"],
  ["authz.management.cattle.io/gr-owner", "globalrole"],
  ["authz.management.cattle.io/grb-owner", "globalrolebinding"],
];

// Rancher label values whose key names the owner ("<ns>_<name>" or a UID).
const rancherOwnerValues = [
  "membership-binding-owner",
  "memberhsip-binding-owner", // Rancher's legacy misspelling
  "prtb-in-cluster-binding-owner",
  "crtb-in-project-binding-owner",
  "owner-user",
];

function groupOf(resource: string): string {
  const dot = resource.indexOf(".");
  return dot < 0 ? "" : resource.slice(dot + 1);
}

const globs = new Map<string, RegExp | null>();

// A RegExp for a pattern of Go's path.Match: * and ? (not across "/"),
// [classes] with ^ for negation, and \ to escape. null if malformed.
function glob(pattern: string): RegExp | null {
  let re = globs.get(pattern);
  if (re !== undefined) return re;
  let out = "";
  let ok = true;
  for (let i = 0; i < pattern.length && ok; i++) {
    const c = pattern[i];
    if (c === "*") out += "[^/]*";
    else if (c === "?") out += "[^/]";
    else if (c === "\\") {
      if (++i >= pattern.length) ok = false;
      else out += pattern[i].replace(/[.*+?^${}()|[\]\\/-]/g, "\\$&");
    } else if (c === "[") {
      // Go and JavaScript share the syntax of a class: ranges, ^ and escapes.
      const end = pattern.indexOf("]", i + 1);
      if (end < 0) ok = false;
      else {
        out += pattern.slice(i, end + 1);
        i = end;
      }
    } else out += c.replace(/[.*+?^${}()|[\]\\/-]/g, "\\$&");
  }
  re = null;
  if (ok) {
    try {
      re = new RegExp(`^${out}$`);
    } catch {
      re = null;
    }
  }
  globs.set(pattern, re);
  return re;
}

const units: Record<string, number> = { ns: 1e-9, us: 1e-6, µs: 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };

const durations = new Map<string, number | null>();

// Seconds of a Go duration ("2h", "1h30m", "1.5h", "0"); null if malformed.
function parseDuration(s: string): number | null {
  let total = durations.get(s);
  if (total !== undefined) return total;
  total = null;
  if (s === "0") total = 0;
  else if (/^(\d+(\.\d*)?(ns|us|µs|ms|s|m|h))+$/.test(s)) {
    total = 0;
    for (const m of s.matchAll(/(\d+(?:\.\d*)?)(ns|us|µs|ms|s|m|h)/g)) total += Number(m[1]) * units[m[2]];
  }
  durations.set(s, total);
  return total;
}

// A Kubernetes-wide convention, not a sign of an operator: these keys are on
// hand-made objects too. cattle.io/creator=norman marks what a human made in
// the Rancher UI.
function conventionDomain(d: string): boolean {
  return (
    d === "kubernetes.io" ||
    d.endsWith(".kubernetes.io") ||
    d === "k8s.io" ||
    d.endsWith(".k8s.io") ||
    d === "helm.sh" ||
    d.endsWith(".helm.sh") ||
    d === "cattle.io"
  );
}

// A label in the domain of an installed operator's API group (liqo.io/x
// for networking.liqo.io). Annotations do not count: operators read
// annotations of objects they do not own (cert-manager.io/cluster-issuer on
// an Ingress). Neither does a label on a custom resource of the operator's
// own group: that is its input, and a label it adds there says "I read
// this", not "I made this" (the agent-sandbox controller labels each
// SandboxTemplate it uses). The custom resources an operator creates are on
// the skip list.
function operatorLabel(labels: Labels, group: string, cl: Cluster): string | null {
  for (const key of Object.keys(labels)) {
    const slash = key.indexOf("/");
    if (slash < 0) continue;
    const d = key.slice(0, slash);
    if (!d.includes(".") || conventionDomain(d) || sameDomain(d, group)) continue;
    if (cl.operatorDomain(d)) return d;
  }
  return null;
}

// Whether a label domain and an API group are the same, or one is a parent
// domain of the other (agents.x-k8s.io, extensions.agents.x-k8s.io).
function sameDomain(d: string, group: string): boolean {
  return group !== "" && (d === group || group.endsWith("." + d) || d.endsWith("." + group));
}

function looksLikeUID(s: string): boolean {
  if (s.startsWith("u-")) return /^u-[a-z0-9]+$/.test(s);
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(s);
}

function rancherOwnerKey(k: string): boolean {
  if (k.includes("/")) return false;
  const i = k.indexOf("_");
  if (i > 0 && i < k.length - 1) return true;
  return looksLikeUID(k);
}

// "<namespace>_<name>" to "<namespace>/<name>". Rancher shortens values
// longer than 63 characters.
function rancherRTB(v: string): string {
  const i = v.indexOf("_");
  const out = i >= 0 ? v.slice(0, i) + "/" + v.slice(i + 1) : v;
  return v.length === 63 ? out + " (truncated)" : out;
}

// The Rancher object which reconciles an RBAC object without ownerReference.
function rancherOwned(labels: Labels): string | null {
  for (const [key, what] of rancherOwnerLabels) {
    if (labels[key]) return `rancher ${what} ${rancherRTB(labels[key])}`;
  }
  for (const k of Object.keys(labels).sort()) {
    const v = labels[k];
    for (const prefix of ["prtb", "crtb"]) {
      const p = `authz.cluster.cattle.io/${prefix}-owner-`;
      if (k.startsWith(p) && v === "true") return `rancher ${prefix} ${rancherRTB(k.slice(p.length))}`;
    }
    if (rancherOwnerValues.includes(v) && rancherOwnerKey(k)) return `rancher ${v} ${rancherRTB(k)}`;
  }
  return null;
}

// Created by the control plane, the kubelet or the distribution.
function kubeInternal(obj: KubeObject, group: string): boolean {
  const { name, namespace = "" } = obj.metadata;
  const labels: Labels = obj.metadata.labels ?? {};
  const annotations: Labels = obj.metadata.annotations ?? {};
  const kind = obj.kind ?? "";
  if (systemNamespaces.has(namespace)) return true;
  // k3s bootstrap: the roots the deploy controller applies from disk.
  if (group === "flowcontrol.apiserver.k8s.io" || group === "k3s.cattle.io" || group === "helm.cattle.io") {
    return true;
  }
  switch (kind) {
    case "Node":
    case "APIService":
    case "CertificateSigningRequest":
    case "IPAddress":
    case "ServiceCIDR":
      return true;
    case "PersistentVolume":
      return "pv.kubernetes.io/provisioned-by" in annotations;
    case "CustomResourceDefinition":
      return name.endsWith(".k3s.cattle.io") || name.endsWith(".helm.cattle.io");
    case "Namespace":
      return name === "default" || name.startsWith("kube-");
    case "PriorityClass":
      return name.startsWith("system-");
    case "ClusterRole":
    case "ClusterRoleBinding":
    case "Role":
    case "RoleBinding":
      return (
        labels["kubernetes.io/bootstrapping"] === "rbac-defaults" ||
        name.startsWith("system:") ||
        (kind === "ClusterRole" && obj.aggregationRule != null)
      );
    case "ConfigMap":
      // liqo reflects it with a suffix.
      return name.startsWith("kube-root-ca.crt");
    case "Secret":
      return name.startsWith("sh.helm.release.v1.");
    case "ServiceAccount":
      return name === "default";
    case "Service":
    case "Endpoints":
      return namespace === "default" && name === "kubernetes";
    case "EndpointSlice":
      return namespace === "default" && labels["kubernetes.io/service-name"] === "kubernetes";
    case "Pod":
      return "kubernetes.io/config.mirror" in annotations;
    case "Lease":
      return group === "coordination.k8s.io";
  }
  return false;
}

// A Secret a Flux source uses to pull (secretRef), or a Kustomization to
// decrypt (decryption.secretRef): it cannot be in git.
function fluxCredential(secret: KubeObject, ctx: Context<Params>, fluxTypes: string[]): boolean {
  const { name, namespace } = secret.metadata;
  for (const r of fluxTypes) {
    const g = groupOf(r);
    for (const o of ctx.list(r, { namespace })) {
      const ref = g === "source.toolkit.fluxcd.io" ? o.spec?.secretRef?.name : o.spec?.decryption?.secretRef?.name;
      if (ref === name) return true;
    }
  }
  return false;
}

// Reconciled by an operator which sets no ownerReference.
function operatorOwned(obj: KubeObject, group: string, ctx: Context<Params>, cl: Cluster): boolean {
  const labels: Labels = obj.metadata.labels ?? {};
  const kind = obj.kind ?? "";
  const gk = group ? `${group}/${kind}` : kind;
  if (defaultSkipKinds.has(gk) || ctx.params.skipKinds.includes(gk)) return true;
  if (kind === "Secret" && group === "") {
    if ("argocd.argoproj.io/secret-type" in labels) return true;
    if (fluxCredential(obj, ctx, cl.fluxTypes)) return true;
  }
  if ((labels["app.kubernetes.io/managed-by"] ?? "").startsWith("cert-manager")) return true;
  if (operatorStateLabels.some(([k, v]) => k in labels && (v === "" || labels[k] === v))) return true;
  if (group === "rbac.authorization.k8s.io" && cl.rancher && rancherOwned(labels)) {
    return true;
  }
  return operatorLabel(labels, group, cl) !== null;
}

// How the object most likely got into the cluster, so you know where to
// look. None of these make it managed.
function hints(obj: KubeObject): string[] {
  const labels: Labels = obj.metadata.labels ?? {};
  const annotations: Labels = obj.metadata.annotations ?? {};
  const out: string[] = [];
  const add = (s: string) => {
    if (!out.includes(s)) out.push(s);
  };
  const release = annotations["meta.helm.sh/release-name"];
  const managedBy = labels["app.kubernetes.io/managed-by"];
  if (release) {
    const ns = annotations["meta.helm.sh/release-namespace"];
    add(`helm release ${ns ? ns + "/" : ""}${release}`);
  } else if (managedBy === "Helm") {
    add("helm");
  }
  if (managedBy && managedBy !== "Helm") add(`managed-by ${managedBy}`);
  // Labels of a GitOps tool which is not installed.
  for (const [key, what] of [
    ["kustomize.toolkit.fluxcd.io/name", "flux kustomization"],
    ["helm.toolkit.fluxcd.io/name", "flux helmrelease"],
    ["argocd.argoproj.io/instance", "argocd app"],
    ["fleet.cattle.io/bundle-name", "fleet bundle"],
  ]) {
    if (labels[key]) add(`${what} ${labels[key]}`);
  }
  const tracking = (annotations["argocd.argoproj.io/tracking-id"] ?? "").split(":")[0];
  if (tracking) add(`argocd app ${tracking}`);
  const rancher = rancherOwned(labels);
  if (rancher) add(rancher);
  if (labels["cattle.io/creator"] === "norman") {
    const user = annotations["field.cattle.io/creatorId"];
    add(`rancher UI/API (norman)${user ? " by " + user : ""}`);
  }
  if (labels["kapp.k14s.io/app"]) add(`kapp (app id ${labels["kapp.k14s.io/app"]})`);
  if ("kubectl.kubernetes.io/last-applied-configuration" in annotations) add("kubectl apply");
  // Values come from the object: keep the message on one line.
  return out.map((h) => (/[\x00-\x1f\x7f]/.test(h) ? JSON.stringify(h) : h));
}

// What the check derives from ctx.resources(), which returns the same array
// for all calls of an evaluation.
type Cluster = {
  resources: string[];
  installed: typeof managers;
  rancher: boolean;
  // The types of Flux which reference Secrets.
  fluxTypes: string[];
  // Whether a label domain belongs to an installed operator's API group,
  // or is a parent or child domain of one (liqo.io, networking.liqo.io).
  operatorDomain(domain: string): boolean;
};

let last: Cluster | null = null;

function cluster(resources: string[]): Cluster {
  if (last?.resources !== resources) {
    const groups = new Set(resources.map(groupOf));
    const operatorGroups = [...groups].filter((g) => !builtinGroups.has(g));
    const domains = new Map<string, boolean>();
    last = {
      resources,
      installed: managers.filter((m) => m.installed(groups)),
      rancher: rancherGroups.some((g) => groups.has(g)),
      fluxTypes: resources.filter((r) =>
        ["source.toolkit.fluxcd.io", "kustomize.toolkit.fluxcd.io"].includes(groupOf(r)),
      ),
      operatorDomain(d) {
        let v = domains.get(d);
        if (v === undefined) {
          v = operatorGroups.some((g) => sameDomain(d, g));
          domains.set(d, v);
        }
        return v;
      },
    };
  }
  return last;
}

type Params = {
  maxDebugPodAge: string;
  managerLabels: string[];
  managerAnnotations: string[];
  skipKinds: string[];
  ignoreNames: string[];
};

export default defineCheck<Params>({
  name: "orphaned",
  category: "hygiene",
  severity: "info",
  enabled: false,
  match: {
    resources: ["*", "secrets"],
    exclude: ["events", "events.events.k8s.io", "componentstatuses", "bindings"],
  },
  params: {
    // A Pod or Job without owner younger than this is not reported.
    maxDebugPodAge: "2h",
    // Keys whose presence marks an object as managed, for tools konmon does
    // not know. ["meta.helm.sh/release-name"] accepts plain helm installs.
    managerLabels: [],
    managerAnnotations: [],
    // More operator kinds as "group/Kind" ("Kind" for the core group).
    skipKinds: [],
    // Globs of object names which are never reported.
    ignoreNames: [],
  },
  check(obj, ctx): Finding[] {
    const meta = obj.metadata;
    if ((meta.ownerReferences ?? []).length > 0) return [];
    const labels: Labels = meta.labels ?? {};
    const annotations: Labels = meta.annotations ?? {};
    const group = ctx.resource.group;
    const cl = cluster(ctx.resources());
    const p = ctx.params;
    const maxAge = parseDuration(p.maxDebugPodAge);
    if (maxAge === null)
      throw new Error(`param maxDebugPodAge: want a duration like 2h or 1h30m, got ${p.maxDebugPodAge}`);

    for (const m of cl.installed) {
      if (m.labels.some((k) => k in labels) || m.annotations.some((k) => k in annotations)) return [];
    }
    if (p.managerLabels.some((k) => k in labels) || p.managerAnnotations.some((k) => k in annotations)) return [];
    if (kubeInternal(obj, group)) return [];
    if (operatorOwned(obj, group, ctx, cl)) return [];
    if (p.ignoreNames.some((g) => glob(g)?.test(meta.name))) return [];

    const ignore = annotations[ignoreAnnotation];
    if (ignore !== undefined && ignore.trim() !== "") return [];

    const kind = obj.kind ?? ctx.resource.resource;
    let message = "not managed by a controller or GitOps tool";
    if ((kind === "Pod" && group === "") || (kind === "Job" && group === "batch")) {
      const created = meta.creationTimestamp ? Date.parse(meta.creationTimestamp) : NaN;
      if (!isNaN(created) && ctx.now.getTime() - created < maxAge * 1000) return [];
      message = `${kind} without owner, older than ${p.maxDebugPodAge}`;
    }
    const h = hints(obj);
    if (h.length > 0) message += `; likely: ${h.join(", ")}`;
    if (ignore !== undefined) message += `; ${ignoreAnnotation} is empty, give a reason`;
    return [{ message, since: meta.creationTimestamp }];
  },
});
