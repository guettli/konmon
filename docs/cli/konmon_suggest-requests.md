## konmon suggest-requests

Suggest CPU and memory requests from the observed usage

### Synopsis

Suggest CPU and memory requests per workload and container from the usage
'konmon run' recorded (sampled from the metrics API, metrics.k8s.io, once a
minute, kept as hourly buckets for --retention). With --live konmon samples
the cluster itself for that long instead, without a database.

The rule: CPU request = the --cpu-percentile (95) of the hourly average CPU
use, rounded up to 5m; memory request = the highest memory use seen plus
--memory-headroom (15%), rounded up to 1Mi. No CPU limit is suggested
(see check container-cpu-limit). Every Pod of a workload counts as one
sample: a request is per Pod.

DATA is the number of hours with samples (minutes with --live). Workloads
with less than --min-hours of it get no suggestion; for Jobs and CronJobs
the time span from their first to their last run counts instead. A day
covers the daily load pattern, a week the weekly one. A workload which does
real work only now and then (a nightly job) needs a window that contains it.

Examples:
  konmon suggest-requests --db /data/konmon.db
  konmon suggest-requests --db /data/konmon.db -n my-app --since 720h -o yaml
  konmon suggest-requests --live 1h -n my-app

```
konmon suggest-requests [flags]
```

### Options

```
      --context string           Kubeconfig context to use. Default: current context.
      --cpu-percentile float     Percentile of the average CPU use per hour (per minute with --live). (default 95)
  -h, --help                     help for suggest-requests
      --kubeconfig string        Path of the kubeconfig file. Default: $KUBECONFIG or ~/.kube/config, or the in-cluster config.
      --live duration            Sample the cluster for this long instead of reading the database, e.g. 30m.
      --live-interval duration   With --live: time between two samples. (default 15s)
      --memory-headroom float    Added to the peak memory use, as a fraction. (default 0.15)
      --min-hours int            Hours of data a workload needs for a suggestion (database mode). (default 24)
  -n, --namespace string         Only this namespace.
  -o, --output string            Output format: table, yaml (resources snippets) or json. (default "table")
      --since string             Use the usage recorded since then: a duration (168h) or an RFC3339 time. (default "168h")
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor

