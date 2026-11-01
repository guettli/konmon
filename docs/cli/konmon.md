## konmon

Kubernetes Condition Monitor

### Synopsis

konmon checks the resources of a Kubernetes cluster: status.conditions
(the rules of check-conditions) and more checks, written in TypeScript or
JavaScript, and your own ones.

Ad hoc:      konmon check --context prod
Monitoring:  konmon run, which keeps a local copy with history in SQLite
             and sends alerts to Alertmanager; query it with 'konmon
             findings' and 'konmon history'.
False positives: 'konmon triage' fixes them in your config file, and
'konmon report' tells the maintainers.

### Options

```
      --db string          Path of the SQLite database. (default "konmon.db")
  -h, --help               help for konmon
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon check](konmon_check.md)	 - Run all checks once and print the findings
* [konmon checks](konmon_checks.md)	 - List, test and write checks
* [konmon findings](konmon_findings.md)	 - Show the findings which are open now, or were open at a given time
* [konmon history](konmon_history.md)	 - Show the condition changes and findings of one object over time
* [konmon report](konmon_report.md)	 - Tell the maintainers about a false positive, an idea or a bug
* [konmon run](konmon_run.md)	 - Watch the cluster, record history and send alerts
* [konmon triage](konmon_triage.md)	 - Walk through the findings and write ignore rules and condition polarity to a config file
* [konmon version](konmon_version.md)	 - Print the version

