## konmon history

Show the condition changes and findings of one object over time

### Synopsis

Show what happened to one object: every recorded change of its conditions,
and when findings were opened and resolved. RESOURCE is the plural resource
name or the kind, optionally with group (deployments.apps/my-app).

Examples:
  konmon history -n kube-system pods/coredns-abc
  konmon history machines.cluster.x-k8s.io/my-machine --since 24h

```
konmon history RESOURCE/NAME [flags]
```

### Options

```
  -h, --help               help for history
  -n, --namespace string   Namespace of the object. Default: all namespaces.
      --since string       Only show changes after this time: a duration ago (24h) or an RFC3339 timestamp. Default: everything.
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor

