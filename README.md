# konmon: Kubernetes Condition Monitor

konmon watches **all** resources of a Kubernetes cluster (including every CRD), keeps a local copy with history in SQLite, runs **checks written in TypeScript or JavaScript** on every object, and sends alerts to Alertmanager.

The 29 core checks (unhealthy `status.conditions` of any resource, crash-looping or unstartable containers, Services without endpoints, dangling references, and more; ported from [check-conditions](https://github.com/guettli/check-conditions), [k8sgpt](https://github.com/k8sgpt-ai/k8sgpt) and [Popeye](https://github.com/derailed/popeye)) are ordinary check files embedded in the binary. Your own checks work exactly the same way: no recompiling, and a check of yours with the name of a core check replaces it.

Conditions have one big advantage over logs and events: they show the current state, and a warning disappears as soon as the desired state is reached. But `status.conditions` only shows *now*. konmon records every change, so you can answer "what was wrong at 03:00?" and "when did this machine become not ready?".

## How it works

```
             watch (dynamic informers, the types the checks need)
 API server ───────────────────────────────────────────────────────▶ konmon run
                                                                    │
     ┌──────────────────────────────────────────────────────────────┤
     ▼                                                              ▼
 SQLite (konmon.db)                                         every --eval-interval:
   objects            latest version of every object        checks (TS/JS, goja)
   revisions          gzip copy of every real change          → findings table
   conditions         current conditions per object           → Alertmanager
   condition_history  one row per condition change
   findings           one row per period a problem was open
```

- **Change detection ignores noise:** `resourceVersion`, `managedFields` and condition heartbeats (`lastHeartbeatTime`, `lastProbeTime`, `lastUpdateTime`) are not real changes. A revision or history row is only written if something else changed.
- **Only the types the checks need are watched.** A type a check runs on (`match`) is stored; a type a check only reads (`reads`) stays in memory. Secrets are never read. Leases, EndpointSlices and `events.k8s.io` are excluded by default since they change all the time (`--exclude-resource`); a check can still read them.
- **Deletions while konmon was down** are detected after the initial list of each type.
- **New CRDs** are picked up by re-running discovery (`--discovery-interval`).
- **Findings work like Prometheus alerts:** a finding is *pending* until it has been open for `--for`, then it *fires*. Firing alerts are re-sent on every evaluation; resolved alerts are sent for `--resend-resolved`.

## Install

```console
go install github.com/guettli/konmon@latest
```

Binaries for Linux, macOS and Windows are attached to every [release](https://github.com/guettli/konmon/releases), and every release is pushed as `ghcr.io/guettli/konmon:<version>` (and `:latest`).

## Quick start

```console
# What is wrong in the cluster right now? Health first, hygiene below.
# Exit code 1: health findings.
konmon check --context prod

# False positives (e.g. conditions of CRDs konmon does not know): fix them
# interactively; the answers go into a config file.
konmon triage --context prod --config konmon.yaml

# Monitor: keep the history and send alerts.
konmon run --db konmon.db --config konmon.yaml --alertmanager-url http://alertmanager:9093

# What was wrong two hours ago? What happened to this object?
konmon findings --db konmon.db --at 2h
konmon history --db konmon.db -n kube-system pods/coredns-6799fbcd5-abcde
```

In a cluster: `kubectl apply -k 'github.com/guettli/konmon/deploy?ref=v0.0.10'` (see [deploy](docs/deploy.md)).

## Documentation

- [Using konmon](docs/usage.md): `check`, `findings`, `history`, the database
- [Checks](docs/checks.md): the core checks, and how to write your own in TypeScript
- [Ignoring findings](docs/config.md): the config file, teaching condition polarity, `konmon triage`
- [Running in a cluster](docs/deploy.md): manifests, flags, alerts, memory
- [CLI reference](docs/cli/konmon.md) (generated)
- [Contributing](CONTRIBUTING.md)

## Improve konmon

konmon's knowledge about conditions grows with every report. If konmon reports something healthy, misses something, or does something wrong, please tell us. It takes a minute:

```console
konmon report false-positive --check conditions --resource widgets.example.com \
  --condition Stalled --status True --reason Waiting
konmon report check-idea --title "detect expired certificates"
konmon report bug
```

Each prints a link to a prefilled GitHub issue (add `--open` to open it). Nothing is sent automatically, and object names and messages are left out; you see and edit everything before you submit. `konmon triage` prints the same link after you teach the polarity of a CRD condition. Or open an issue directly: [false positive](https://github.com/guettli/konmon/issues/new?template=false-positive.yml), [check idea](https://github.com/guettli/konmon/issues/new?template=check-idea.yml), [bug](https://github.com/guettli/konmon/issues/new?template=bug.yml).

## License

Apache License 2.0. konmon contains code ported from check-conditions, k8sgpt and Popeye, see [NOTICE](NOTICE).
