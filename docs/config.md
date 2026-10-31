# Ignoring findings: the config file

A finding which is fine (a false positive, or something you accept) is dropped by an ignore rule in a config file, `--config konmon.yaml` (for `run`, `check` and `findings`; repeatable, the rules are merged):

```yaml
ignore:
  - check: conditions
    resource: widgets.example.com   # resource.group
    namespace: team-*
    condition: Stalled
    message: "Waiting for upstream" # regular expression
    comment: Stalled=True while waiting for upstream is normal.   # required
    until: 2027-01-31               # optional, last day (UTC); then konmon warns and reports again
```

All fields which are set must match; `check`, `resource`, `namespace`, `name`, `condition`, `container` and `key` are globs, where `*` also matches `/` (quote a glob which starts with `*`: `name: "*-test"`). A `namespace` pattern never matches cluster-scoped objects. Unknown fields, a second YAML document (`---`) and a rule which matches everything are errors, so a typo never ignores everything or nothing. A failing check (`check-error`) is only hidden by a rule with `check: check-error` and `key: <name of the failing check>`. `konmon check` says how many findings were ignored; `konmon run` logs `finding ignored` with the rule.

`konmon findings --config new.yaml` applies the rules to the findings a running konmon stored, to try a stricter config before deploying it (findings which the running config already ignores are not stored). See [examples/konmon.yaml](../examples/konmon.yaml).

## Teaching condition polarity

Most false positives of the `conditions` check are conditions of CRDs konmon does not know: is `True` or `False` healthy? Instead of ignoring them, teach konmon in the same file:

```yaml
conditions:
  widgets.example.com:              # resource.group; a bare name is the core group; "*" is any resource
    healthyWhenTrue:  [Synced]
    healthyWhenFalse: [Stalled, Degraded]
    ignoreTypes:      [LastSyncAttempted]   # informational: both values are fine
```

Unlike an ignore rule, a taught condition is still reported when it has its bad status, or a status other than `True`/`False` (e.g. `Unknown`; use `ignoreTypes` for types whose status is something else, like `Accepted`). The rules of the resource are looked at before `"*"`. Taught polarity wins over the built-in polarity tables of `checks/conditions.ts`; its list of known healthy states (specific reasons) still applies. If you replace `conditions.ts` with your own check, it gets the section as param `rules`. `konmon findings --config` applies only `ignore` rules, not `conditions` (stored findings are not evaluated again). If the CRD is public, please tell us (see [Improve konmon](../README.md#improve-konmon)) so everybody gets it.

## Interactive: `konmon triage`

You don't have to write the file by hand. `konmon triage` walks through the open findings, grouped by pattern ("Stalled=True Waiting on 12 widgets"), and writes your answers to the config file (the last `--config`, created if missing, comments kept), after each decision:

```console
$ konmon triage --context prod --config konmon.yaml
[1/7] conditions  widgets.example.com  Stalled=True Waiting   (health, warning)
      12 findings in 4 namespaces, the oldest for 9d
      history: possibly normal for this type: Stalled=True on all 12 objects of widgets.example.com for at least 9d, all of them Ready=True. Suggestion: if that is right for this type, teach the polarity (t).
      e.g. team-a/widget-1: Condition Stalled=True Waiting "Waiting for upstream"
  t) teach polarity  i) ignore   s) snooze   k) keep (real problem)   ?) details   n) next   q) quit
> i
  Ignore which findings?
  1) only team-a/widget-1                                         hides 1
  2) these findings in namespace team-a                           hides 3
  3) these findings everywhere                                    hides 12
  4) condition Stalled of widgets.example.com, any status and reason  hides 14  (+2 more)
  5) every conditions finding of widgets.example.com              hides 15  (+3 more)
  6) every finding of check conditions                            hides 40  (+28 more)
```

- **t** teaches the polarity (for this resource type or all), **i** ignores with the scope you pick (each with the number of findings it hides), a required comment and an optional expiry. **s** snoozes (an ignore rule with `until`, default 7 days), **k** keeps a real problem (`acknowledged:`, still alerts, not asked again), **?** shows the objects and, with `--from-db`/`--use-db`, the condition history.
- Same flags as `konmon check` (`-n`, `--category`, check names), or `--from-db` to triage the findings of a running konmon.
- With history (`--from-db`, or `--use-db` for a live run), triage ranks condition findings and says why. It is deliberately cautious, it only hints:
  - **possibly normal for this type**: at least 2 objects, every object of the type has this status (not `Unknown`) since more than a day, and all of them are `Ready=True`;
  - **possibly informational**: it flips often while `Ready` stays `True` the whole time;
  - **likely real**: `Ready` became unhealthy within 10 minutes after it. These come last.

  It cannot know whether `Ready` of the type summarises the condition: check before you teach.
- It never changes the cluster: review the file (`git diff`) and deploy it.

**Baseline.** On a cluster with many findings on day one, `konmon triage --baseline` writes one narrow ignore rule (this object) per open finding without asking, expiring after `--baseline-until` (default 90 days). The cluster is quiet now, only new problems are reported, and the expiry brings the old ones back for a real triage. Running it again adds only new findings (also the ones whose baseline rule expired: triage them interactively instead); failing checks and acknowledged findings are never baselined. A baseline rule is by object name, so a recreated object with the same name stays hidden until the rule expires. Narrow it like `check`, e.g. `--category hygiene`.

In a cluster, put the file in a ConfigMap and restart konmon on changes (e.g. with [Reloader](https://github.com/stakater/Reloader)).

