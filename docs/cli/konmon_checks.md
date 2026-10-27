## konmon checks

List, test and write checks

### Options

```
      --check-param stringArray            Override a check parameter: check.param=value, e.g. pod-container-restarts.restarts=10. Can be given multiple times.
      --check-timeout duration             Time limit for one call of a check function. (default 1s)
      --checks stringArray                 Directory with own checks (*.ts, *.js). A check with the name of a core check replaces it. Can be given multiple times.
      --config stringArray                 Config file with ignore rules (YAML, see README). Can be given multiple times: the rules are merged.
      --disable-check strings              Checks to disable, by name. Comma-separated, glob patterns allowed.
      --exclude-namespace strings          Namespaces to ignore (not stored, not checked). Comma-separated, glob patterns allowed.
      --exclude-resource strings           Resource types not to check or store, as resource.group (core: resource). Comma-separated, glob patterns allowed. A check can still read them if it lists them in reads. Secrets are never read. (default [leases.coordination.k8s.io,endpointslices.discovery.k8s.io,events.events.k8s.io])
  -h, --help                               help for checks
      --ignore-finding-regex stringArray   Drop findings matching this regex (Go syntax). Matched against: 'check resource namespace/name message'. Can be given multiple times.
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor
* [konmon checks list](konmon_checks_list.md)	 - List the enabled checks
* [konmon checks test](konmon_checks_test.md)	 - Run the *.test.yaml files of the core checks and of --checks directories
* [konmon checks types](konmon_checks_types.md)	 - Print konmon.d.ts, the TypeScript types for writing checks

