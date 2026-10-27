## konmon report

Tell the maintainers about a false positive, an idea or a bug

### Synopsis

Print a link to a prefilled GitHub issue. Nothing is sent: you see and edit
everything in the browser before you submit it. Object names and condition
messages are not included.

Every false positive which is fixed in konmon itself is fixed for everybody.

### Options

```
  -h, --help   help for report
      --open   Open the link in the browser.
```

### Options inherited from parent commands

```
      --db string          Path of the SQLite database. (default "konmon.db")
      --log-level string   Log level: debug, info, warn, error. (default "info")
```

### SEE ALSO

* [konmon](konmon.md)	 - Kubernetes Condition Monitor
* [konmon report bug](konmon_report_bug.md)	 - Report a bug
* [konmon report check-idea](konmon_report_check-idea.md)	 - Suggest something konmon should detect
* [konmon report false-positive](konmon_report_false-positive.md)	 - Report a finding of a healthy state

