# Contributing

Thanks! Reports of false positives and ideas for checks are as welcome as code: see [Improve konmon](README.md#improve-konmon).

## Development

```console
make check   # the same as CI: tests, golangci-lint, actionlint, govulncheck, shellcheck,
             # generated docs, tsc and prettier for the TypeScript checks
make fmt     # format Go and TypeScript
make docs    # regenerate docs/cli and the table of core checks in docs/checks.md
make e2e     # deploy/ on a kind cluster (needs kind and docker)
```

Layout:

| Path | |
|---|---|
| `cmd/` | the CLI (cobra) |
| `checks/` | the core checks (TypeScript, embedded in the binary) with their `*.test.yaml` |
| `internal/checks/` | loading, compiling (esbuild) and running checks (goja) |
| `internal/config/` | the config file: ignore rules, condition polarity, the editor of `konmon triage` |
| `internal/monitor/` | `konmon run`: evaluations, findings, alerts |
| `internal/store/` | the SQLite database |
| `internal/watch/` | discovery and informers |
| `internal/alertmanager/` | the Alertmanager client |
| `deploy/` | manifests, tested by `hack/e2e.sh` |
| `docs/` | documentation; `docs/cli` and the checks table are generated |

## A new core check

1. Write `checks/NAME.ts`. The header comment starts with `// Reports …`: its first sentence becomes the description in [docs/checks.md](docs/checks.md). See [Writing checks](docs/checks.md#writing-checks).
2. Add `checks/NAME.test.yaml` with healthy and unhealthy cases. Think of states which look broken but are normal (scale to zero, rollouts, completed Jobs): core checks alert, they must not cry wolf.
3. Code ported from another project: name it in the header comment and in [NOTICE](NOTICE), and check that its license allows it.
4. `make docs check`, then try it on a real cluster: `go run . check --context <ctx> NAME`.

## Tests against real data

The core `conditions` check is a port of check-conditions. `internal/checks/corpus_test.go` compares both on the same objects; with `KONMON_CORPUS_DB=konmon.db` it uses the objects of a real database. On three clusters (about 2,000 objects) the results are identical. A full evaluation of 900 objects takes about 8 ms (`BenchmarkEvaluate`).

## CI and releases

CI runs on pull requests and on main: tests with the race detector and coverage, the check tests, a build of every release target, lint (golangci-lint, actionlint, govulncheck, `go mod tidy`, shellcheck, generated docs up to date), a type check and formatting check of the TypeScript checks, an image build, and the end-to-end test on kind. Actions are pinned by commit; Dependabot updates them and the Go modules weekly.

After CI succeeds on `main`, the release workflow bumps the patch version, creates a GitHub release with binaries and pushes the image `ghcr.io/guettli/konmon:<version>` (and `:latest`).
