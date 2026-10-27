## konmon findings

Show the findings which are open now, or were open at a given time

### Synopsis

Show the findings which are open now, or with --at, which were open at that
time. The output has the same format as 'konmon check', with the time the
finding was first seen.

Examples:
  konmon findings
  konmon findings --at 2h -n my-app
  konmon findings --at 2026-10-24T03:00:00Z --category health -o json

```
konmon findings [flags]
```

### Options

```
      --at string            Point in time: a duration ago (2h) or an RFC3339 timestamp. Default: now.
      --category strings     Only show findings of these categories: health, hygiene (failing checks are always shown). Comma-separated. Default: both.
      --config stringArray   Config file with ignore rules: hide the findings they match, to try rules before deploying them. Can be given multiple times.
  -h, --help                 help for findings
  -n, --namespace strings    Only show findings of objects in these namespaces (cluster-scoped objects are left out; failing checks are always shown). Comma-separated, glob patterns allowed.
  -o, --output string        Output format: text or json. (default "text")
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor

