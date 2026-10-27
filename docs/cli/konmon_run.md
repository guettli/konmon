## konmon run

Watch the cluster, record history and send alerts

### Synopsis

Watch all resource types of the cluster and store every change in the
SQLite database. Every --eval-interval all checks run on all objects. A
health finding which is open for at least --for fires and is sent to
Alertmanager (if --alertmanager-url is given).

The kubeconfig is found like kubectl does (KUBECONFIG, ~/.kube/config), or
in-cluster config is used when running in a Pod.

```
konmon run [flags]
```

### Options

```
      --alertmanager-url strings           Alertmanager base URL, e.g. http://alertmanager:9093. Can be given multiple times.
      --alertmanager-watchdog              Send an always-firing KonmonWatchdog alert, for a dead man's switch.
      --check-param stringArray            Override a check parameter: check.param=value, e.g. pod-container-restarts.restarts=10. Can be given multiple times.
      --check-timeout duration             Time limit for one call of a check function. (default 1s)
      --checks stringArray                 Directory with own checks (*.ts, *.js). A check with the name of a core check replaces it. Can be given multiple times.
      --cluster-name string                Value of the 'cluster' label of alerts. Empty: no label.
      --config stringArray                 Config file with ignore rules and condition polarity (YAML, see docs/config.md). Can be given multiple times: the rules are merged.
      --context string                     Kubeconfig context to use. Default: current context.
      --disable-check strings              Checks to disable, by name. Comma-separated, glob patterns allowed.
      --discovery-interval duration        Time between two discoveries of resource types (new CRDs). (default 5m0s)
      --eval-interval duration             Time between two evaluations of all objects. (default 30s)
      --exclude-namespace strings          Namespaces to ignore (not stored, not checked). Comma-separated, glob patterns allowed.
      --exclude-resource strings           Resource types not to check or store, as resource.group (core: resource). Comma-separated, glob patterns allowed. A check can still read them if it lists them in reads. Secrets are never read. (default [leases.coordination.k8s.io,endpointslices.discovery.k8s.io,events.events.k8s.io])
      --for duration                       How long a finding must be open before it fires. (default 2m0s)
  -h, --help                               help for run
      --history-window duration            How much condition history checks which use ctx.history can see. (default 24h0m0s)
      --ignore-finding-regex stringArray   Drop findings matching this regex (Go syntax). Matched against: 'check resource namespace/name message'. Can be given multiple times.
      --kubeconfig string                  Path of the kubeconfig file. Default: $KUBECONFIG or ~/.kube/config, or the in-cluster config.
      --listen string                      Address for /healthz. Empty disables it. (default ":8080")
      --no-revisions-for strings           Resource types (resource.group, globs allowed) to store without revisions.
      --resend-resolved duration           How long resolved alerts are re-sent to Alertmanager. (default 15m0s)
      --retention duration                 How long to keep history (revisions, condition changes, resolved findings, deleted objects). 0 keeps everything. (default 720h0m0s)
      --revisions                          Store a compressed copy of every changed object, not only the latest version. (default true)
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor

