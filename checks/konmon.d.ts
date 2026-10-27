// Types for konmon checks. Print them with `konmon checks types`.
declare module "konmon" {
  /** A Kubernetes object as returned by the API server. Read-only. */
  export interface KubeObject {
    readonly apiVersion?: string;
    readonly kind?: string;
    readonly metadata: ObjectMeta;
    readonly spec?: any;
    readonly status?: any;
    readonly [key: string]: any;
  }

  export interface ObjectMeta {
    readonly name: string;
    readonly namespace?: string;
    readonly uid: string;
    readonly labels?: Readonly<Record<string, string>>;
    readonly annotations?: Readonly<Record<string, string>>;
    readonly creationTimestamp?: string;
    readonly deletionTimestamp?: string;
    readonly finalizers?: readonly string[];
    readonly ownerReferences?: readonly OwnerReference[];
    readonly [key: string]: any;
  }

  export interface OwnerReference {
    readonly apiVersion: string;
    readonly kind: string;
    readonly name: string;
    readonly uid: string;
    readonly controller?: boolean;
  }

  /** One entry of status.conditions. */
  export interface Condition {
    readonly type: string;
    readonly status: string;
    readonly reason?: string;
    readonly message?: string;
    readonly lastTransitionTime?: string;
    readonly [key: string]: any;
  }

  export interface Finding {
    /** Distinguishes several findings of one check on one object. */
    key?: string;
    /** One line, without an age: konmon adds the age from `since`. */
    message: string;
    /** When the problem started: a Date, an RFC3339 string or Unix ms. */
    since?: Date | string | number;
    /** Condition type(s), becomes the `condition` alert label. */
    condition?: string;
    /** Container name, becomes the `container` alert label. */
    container?: string;
    /** Extra values, become alert annotations. */
    details?: Record<string, string | number | boolean>;
    /** Overrides the severity of the check for this finding. */
    severity?: "critical" | "warning" | "info";
  }

  /** The resource type of the checked object. */
  export interface Resource {
    readonly group: string;
    readonly version: string;
    readonly resource: string;
  }

  export interface ListOptions {
    namespace?: string;
    /** Equality label selector. */
    labels?: Record<string, string>;
    /** Only objects with an ownerReference to this object (or uid). */
    owner?: KubeObject | string;
  }

  export interface ConditionChange {
    readonly observedAt: Date;
    readonly change: "set" | "removed";
    readonly type: string;
    readonly status: string;
    readonly reason: string;
    readonly message: string;
    readonly lastTransitionTime: string;
  }

  export interface Context<P> {
    /** The time of this evaluation. Date and Math.random are deterministic. */
    readonly now: Date;
    readonly params: Readonly<P>;
    readonly resource: Resource;
    /**
     * Reads one object from konmon's cache, never from the API server.
     * resource is "pods", "deployments.apps", ... and must be in match or
     * reads. Use null as namespace for cluster-scoped objects. Throws if the
     * type is not available; returns null if the object does not exist.
     */
    get(resource: string, namespace: string | null, name: string): KubeObject | null;
    list(resource: string, options?: ListOptions): KubeObject[];
    /** Whether a resource type is available (e.g. an optional CRD). */
    has(resource: string): boolean;
    readonly history: {
      /** Condition changes of an object. Needs uses: ["history"]. */
      conditions(obj: KubeObject | string, since?: Date): ConditionChange[];
    };
    /** Debug logging (konmon --log-level debug). */
    log(...args: unknown[]): void;
  }

  export interface CheckDefinition<P = Record<string, never>> {
    /** Unique name; a user check with the name of a core check replaces it. */
    name: string;
    /** "health" (default) may alert; "hygiene" is report-only. */
    category?: "health" | "hygiene";
    severity?: "critical" | "warning" | "info";
    match: {
      /** "*", "pods", "deployments.apps", "ingresses.networking.k8s.io", ... */
      resources: string[];
      exclude?: string[];
    };
    /** Other resource types the check reads with ctx.get/ctx.list. */
    reads?: string[];
    uses?: "history"[];
    /**
     * How long a finding must be open before it fires, for this check;
     * default: konmon's --for (2m). For states which are normal for a
     * while, e.g. Pods starting during a rollout.
     */
    forSeconds?: number;
    /** Defaults; override with --check-param name.key=value. */
    params?: P;
    check(obj: KubeObject, ctx: Context<P>): Finding[] | void;
  }

  export function defineCheck<P>(check: CheckDefinition<P>): CheckDefinition<P>;

  /**
   * Parses an RFC3339 timestamp like Kubernetes writes them. Unlike
   * new Date(s) it rejects timestamps without a zone (which Date reads as
   * local time). Returns undefined for anything else.
   */
  export function parseTime(s: unknown): Date | undefined;

  export interface LabelSelector {
    readonly matchLabels?: Readonly<Record<string, string>>;
    readonly matchExpressions?: readonly {
      readonly key: string;
      readonly operator: "In" | "NotIn" | "Exists" | "DoesNotExist";
      readonly values?: readonly string[];
    }[];
  }

  /** Whether labels match a metav1.LabelSelector. A null selector matches nothing. */
  export function matchesSelector(
    labels: Readonly<Record<string, string>> | undefined,
    selector: LabelSelector | null | undefined,
  ): boolean;

  /** Whether a selector is set but empty (it matches everything). */
  export function isEmptySelector(selector: LabelSelector | null | undefined): boolean;

  /**
   * The pod spec of a Pod or of the template of a workload (deployments,
   * statefulsets, daemonsets, replicasets, replicationcontrollers, jobs,
   * cronjobs); resource is ctx.resource.resource. undefined otherwise.
   */
  export function podSpec(obj: KubeObject, resource: string): any;

  /** The labels of a Pod or of the pod template of a workload. */
  export function podLabels(obj: KubeObject, resource: string): Readonly<Record<string, string>>;
}
