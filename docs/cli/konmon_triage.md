## konmon triage

Walk through the findings and write ignore rules and condition polarity to a config file

### Synopsis

Walk through the open findings, grouped by pattern, and decide for each
group: teach konmon the polarity of a condition, ignore findings like it (with
a scope you choose, a comment and an optional expiry), snooze them, or keep
them as a real problem. The answers are written to the config file (the last
--config, created if missing) after each decision, keeping its comments.
The cluster is never changed: review the file and deploy it like any config.

Findings come from listing the cluster once, like 'konmon check' (same flags),
or with --from-db from the database of a running 'konmon run' (--db).

With --baseline nothing is asked: every open finding gets a narrow ignore
rule (this object) which expires after --baseline-until, so the cluster is
quiet now and only new problems are reported. Running it again adds only new
findings. Failing checks and acknowledged findings are never baselined.

Examples:
  konmon triage --context prod --config konmon.yaml
  konmon triage --config shared.yaml --config prod.yaml -n 'team-*'
  konmon triage --from-db --db /data/konmon.db --config konmon.yaml
  konmon triage --context prod --config konmon.yaml --baseline --category hygiene

```
konmon triage [CHECK...] [flags]
```

### Options

```
      --baseline                           Do not ask: write one narrow ignore rule per open finding, so only new problems are reported. Like the baseline of a linter.
      --baseline-until string              Expiry of the --baseline rules: YYYY-MM-DD or a number of days (90d), so the findings come back for a real triage. Empty: never. (default "90d")
      --category strings                   Only show findings of these categories: health, hygiene (failing checks are always shown). Comma-separated. Default: both.
      --check-param stringArray            Override a check parameter: check.param=value, e.g. pod-container-restarts.restarts=10. Can be given multiple times.
      --check-timeout duration             Time limit for one call of a check function. (default 1s)
      --checks stringArray                 Directory with own checks (*.ts, *.js). A check with the name of a core check replaces it. Can be given multiple times.
      --config stringArray                 Config file with ignore rules and condition polarity (YAML, see docs/config.md). Can be given multiple times: the rules are merged.
      --context string                     Kubeconfig context to use. Default: current context.
      --disable-check strings              Checks to disable, by name. Comma-separated, glob patterns allowed.
      --exclude-namespace strings          Namespaces to ignore (not stored, not checked). Comma-separated, glob patterns allowed.
      --exclude-resource strings           Resource types not to check or store, as resource.group (core: resource). Comma-separated, glob patterns allowed. A check can still read them if it lists them in reads. Secrets are never read. (default [leases.coordination.k8s.io,endpointslices.discovery.k8s.io,events.events.k8s.io])
      --from-db                            Take the open findings from the database of 'konmon run' (--db) instead of listing the cluster.
  -h, --help                               help for triage
      --history-window duration            How much condition history checks can see. (default 24h0m0s)
      --ignore-finding-regex stringArray   Drop findings matching this regex (Go syntax). Matched against: 'check resource namespace/name message'. Can be given multiple times.
      --ignore-older-than string           Hide findings whose problem started before this: a duration (24h: unchanged for a day) or an RFC3339 timestamp. Combine with --ignore-younger-than for a time window.
      --ignore-younger-than string         Hide findings whose problem started more recently than this: a duration (5m: started in the last 5 minutes) or an RFC3339 timestamp. Findings without a start time (e.g. a missing resource request) are always shown.
      --kubeconfig string                  Path of the kubeconfig file. Default: $KUBECONFIG or ~/.kube/config, or the in-cluster config.
  -n, --namespace strings                  Only show findings of objects in these namespaces (cluster-scoped objects are left out; failing checks are always shown). Comma-separated, glob patterns allowed. A type which may not be listed in all namespaces (RBAC) is listed in each of them, if there is no glob pattern.
      --use-db                             Read condition history for checks from --db.
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor

