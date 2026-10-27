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
forbidden ones, which are printed). Other errors also exit with 1.

Checks which use condition history need --use-db: the history is read from
the database of a running 'konmon run' (--db).

Examples:
  konmon check --context prod
  konmon check -n my-app --category health
  konmon check conditions 'pod-*'
  konmon check -o json | jq '.findings[] | select(.severity == "critical")'

```
konmon check [CHECK...] [flags]
```

### Options

```
      --category strings                   Only show findings of these categories: health, hygiene (failing checks are always shown). Comma-separated. Default: both.
      --check-param stringArray            Override a check parameter: check.param=value, e.g. pod-container-restarts.restarts=10. Can be given multiple times.
      --check-timeout duration             Time limit for one call of a check function. (default 1s)
      --checks stringArray                 Directory with own checks (*.ts, *.js). A check with the name of a core check replaces it. Can be given multiple times.
      --config stringArray                 Config file with ignore rules (YAML, see README). Can be given multiple times: the rules are merged.
      --context string                     Kubeconfig context to use. Default: current context.
      --disable-check strings              Checks to disable, by name. Comma-separated, glob patterns allowed.
      --exclude-namespace strings          Namespaces to ignore (not stored, not checked). Comma-separated, glob patterns allowed.
      --exclude-resource strings           Resource types not to check or store, as resource.group (core: resource). Comma-separated, glob patterns allowed. A check can still read them if it lists them in reads. Secrets are never read. (default [leases.coordination.k8s.io,endpointslices.discovery.k8s.io,events.events.k8s.io])
  -h, --help                               help for check
      --history-window duration            How much condition history checks can see. (default 24h0m0s)
      --ignore-finding-regex stringArray   Drop findings matching this regex (Go syntax). Matched against: 'check resource namespace/name message'. Can be given multiple times.
      --kubeconfig string                  Path of the kubeconfig file. Default: $KUBECONFIG or ~/.kube/config, or the in-cluster config.
  -n, --namespace strings                  Only show findings of objects in these namespaces (cluster-scoped objects are left out; failing checks are always shown). Comma-separated, glob patterns allowed.
  -o, --output string                      Output format: text or json. (default "text")
      --use-db                             Read condition history for checks from --db.
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor

