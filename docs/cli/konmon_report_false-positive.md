## konmon report false-positive

Report a finding of a healthy state

```
konmon report false-positive [flags]
```

### Examples

```
  konmon report false-positive --check conditions --resource widgets.example.com \
    --condition Stalled --status True --reason Waiting
```

### Options

```
      --check string       Name of the check, e.g. conditions.
      --condition string   Condition type, e.g. Stalled.
  -h, --help               help for false-positive
      --reason string      Reason of the condition.
      --resource string    Resource type: resource.group, e.g. widgets.example.com.
      --rule string        The config which fixes it (YAML).
      --status string      Status of the condition, e.g. True.
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
      --open               Open the link in the browser.
```

### SEE ALSO

* [konmon report](konmon_report.md)	 - Tell the maintainers about a false positive, an idea or a bug

