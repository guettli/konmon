## konmon check

Run all checks once and print the findings

### Synopsis

List all objects of the cluster once, run the checks and print the
findings, like check-conditions does. Nothing is stored. Health findings
come first, hygiene findings (which never alert) below them.

With CHECK arguments (names, glob patterns allowed) only these checks run,
and only the resource types they need are listed.

Exit codes: 0 no health findings; 1 health findings; 2 no health findings,
but some resource types or API groups could not be listed (other than
forbidden ones, which are printed); 3 konmon could not check (invalid
flags, cluster not reachable, --timeout).

--repeat runs the check again and again, --while until no health finding
matches a regular expression: 'konmon check --while StillProvisioning;
music' plays music once the condition is gone. In both, network errors
are printed and retried.

Checks which use condition history need --use-db: the history is read from
the database of a running 'konmon run' (--db).

Examples:
  konmon check --context prod
  konmon check -n my-app --category health
  konmon check conditions 'pod-*'
  konmon check -o json | jq '.findings[] | select(.severity == "critical")'
  konmon check --while . --timeout 10m   # wait until healthy
  konmon check --repeat --ignore-younger-than 5m

```
konmon check [CHECK...] [flags]
```

### Options

```
      --category strings                   Only show findings of these categories: health, hygiene (failing checks are always shown). Comma-separated. Default: both.
      --check-param stringArray            Override a check parameter: check.param=value, e.g. pod-container-restarts.restarts=10. Can be given multiple times.
      --check-timeout duration             Time limit for one call of a check function. (default 1s)
      --checks stringArray                 Directory with own checks (*.ts, *.js). A check with the name of a core check replaces it. Can be given multiple times.
      --config stringArray                 Config file with ignore rules and condition polarity (YAML, see docs/config.md). Can be given multiple times: the rules are merged.
      --context string                     Kubeconfig context to use. Default: current context.
      --disable-check strings              Checks to disable, by name. Comma-separated, glob patterns allowed.
      --exclude-namespace strings          Namespaces to ignore (not stored, not checked). Comma-separated, glob patterns allowed.
      --exclude-resource strings           Resource types not to check or store, as resource.group (core: resource). Comma-separated, glob patterns allowed. A check can still read them if it lists them in reads. Secrets are never read. (default [leases.coordination.k8s.io,endpointslices.discovery.k8s.io,events.events.k8s.io])
  -h, --help                               help for check
      --history-window duration            How much condition history checks can see. (default 24h0m0s)
      --ignore-finding-regex stringArray   Drop findings matching this regex (Go syntax). Matched against: 'check resource namespace/name message'. Can be given multiple times.
      --ignore-older-than string           Hide findings whose problem started before this: a duration (24h: unchanged for a day) or an RFC3339 timestamp. Combine with --ignore-younger-than for a time window.
      --ignore-younger-than string         Hide findings whose problem started more recently than this: a duration (5m: started in the last 5 minutes) or an RFC3339 timestamp. Findings without a start time (e.g. a missing resource request) are always shown.
      --interval duration                  With --while or --repeat: pause between two runs. (default 15s)
      --kubeconfig string                  Path of the kubeconfig file. Default: $KUBECONFIG or ~/.kube/config, or the in-cluster config.
      --name string                        Text for the summary line, to tell several terminals apart.
  -n, --namespace strings                  Only show findings of objects in these namespaces (cluster-scoped objects are left out; failing checks are always shown). Comma-separated, glob patterns allowed. A type which may not be listed in all namespaces (RBAC) is listed in each of them, if there is no glob pattern.
  -o, --output string                      Output format: text or json. (default "text")
      --repeat                             Repeat forever (until Ctrl-C).
      --retry-count int                    Network errors before the first successful run: retry this often, a second apart. Later runs of --repeat and --while retry forever. 0: retry forever. (default 5)
      --timeout duration                   Give up after this time, exit code 3. 0: no limit.
      --use-db                             Read condition history for checks from --db.
      --while string                       Repeat while a health finding matches this regular expression, then exit 0. Only matching findings are shown. '.' waits until there are no health findings.
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor

