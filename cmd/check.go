package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"path"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/monitor"
	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/watch"
)

// checkOptions are the flags of commands which list the cluster once and
// run the checks: check and triage.
type checkOptions struct {
	kube          kubeFlags
	checks        checkFlags
	filter        findingFilter
	useDB         bool
	historyWindow time.Duration
}

func (o *checkOptions) register(f *pflag.FlagSet) {
	o.kube.register(f)
	o.checks.register(f)
	o.filter.register(f)
	f.BoolVar(&o.useDB, "use-db", false, "Read condition history for checks from --db.")
	f.DurationVar(&o.historyWindow, "history-window", 24*time.Hour, "How much condition history checks can see.")
}

var checkOpts checkOptions

// exitError ends konmon with a code, without printing an error.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

const (
	// errFindings: there are health findings.
	errFindings = exitError(1)
	// errIncomplete: no findings, but not everything could be checked.
	errIncomplete = exitError(2)
)

var checkCmd = &cobra.Command{
	Use:   "check [CHECK...]",
	Short: "Run all checks once and print the findings",
	Long: `List all objects of the cluster once, run the checks and print the
findings, like check-conditions does. Nothing is stored. Health findings
come first, hygiene findings (which never alert) below them.

With CHECK arguments (names, glob patterns allowed) only these checks run,
and only the resource types they need are listed.

Exit codes: 0 no health findings; 1 health findings; 2 no health findings,
but some resource types or API groups could not be listed (other than
forbidden ones, which are printed). Other errors also exit with 1.

Checks which use condition history need --use-db: the history is read from
the database of a running 'konmon run' (--db).

Examples:
  konmon check --context prod
  konmon check -n my-app --category health
  konmon check conditions 'pod-*'
  konmon check -o json | jq '.findings[] | select(.severity == "critical")'`,
	Args:          cobra.ArbitraryArgs,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		err := check(cmd.Context(), cmd, args)
		var exit exitError
		if err != nil && !errors.As(err, &exit) {
			cmd.PrintErrln("Error:", err)
		}
		return err
	},
}

func init() {
	checkOpts.register(checkCmd.Flags())
	rootCmd.AddCommand(checkCmd)
}

// checkRun is the result of listing the cluster once and running the checks.
type checkRun struct {
	engine    *checks.Engine
	ignore    *ignorer
	listed    *watch.ListResult
	result    *checks.Result
	available int
	now       time.Time
	// findings pass the filter and are not ignored.
	findings []*checks.Finding
	ignored  int
}

// runChecks lists the cluster and runs the checks, like 'konmon check'.
func runChecks(ctx context.Context, o *checkOptions, only []string) (*checkRun, error) {
	if err := o.filter.validate(); err != nil {
		return nil, err
	}
	log, err := newLogger()
	if err != nil {
		return nil, err
	}
	klog.SetSlogLogger(log)
	engine, _, err := o.checks.engine(log, only, o.filter.categories)
	if err != nil {
		return nil, err
	}
	ignore, err := o.checks.ignorer()
	if err != nil {
		return nil, err
	}
	_, dyn, disc, err := o.kube.clients()
	if err != nil {
		return nil, err
	}
	w := &watch.Watcher{Dynamic: dyn, Discovery: disc, Include: engine.Watch, Log: log}
	warnUnserved(engine, disc, log)
	listed, err := w.ListOnce(ctx)
	if err != nil {
		return nil, err
	}
	var snapItems []checks.Item
	for _, it := range listed.Items {
		if ns := it.Obj.GetNamespace(); ns != "" && matchGlob(ns, o.checks.excludeNamespaces) {
			continue
		}
		snapItems = append(snapItems, checks.Item{GVR: it.GVR, Obj: it.Obj})
	}

	var hist checks.History
	now := time.Now()
	if engine.UsesHistory() {
		if !o.useDB {
			log.Warn("some checks use condition history; without --use-db they fail")
		} else {
			st, err := store.OpenExisting(ctx, dbPath)
			if err != nil {
				return nil, err
			}
			defer func() { _ = st.Close() }()
			rows, err := st.ConditionHistorySince(ctx, now.Add(-o.historyWindow))
			if err != nil {
				return nil, err
			}
			hist = monitor.StoreHistory(rows)
		}
	}

	res, err := engine.Evaluate(ctx, checks.NewSnapshot(snapItems, listed.Available), hist, now)
	if err != nil {
		return nil, err
	}
	ignore.warnExpired(log, now)
	run := &checkRun{engine: engine, ignore: ignore, listed: listed, result: res, available: len(listed.Available), now: now}
	for i := range res.Findings {
		f := &res.Findings[i]
		if !o.filter.match(f) {
			continue
		}
		if ignore.ignored(f, now) != "" {
			run.ignored++
			continue
		}
		run.findings = append(run.findings, f)
	}
	return run, nil
}

func check(ctx context.Context, cmd *cobra.Command, only []string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	run, err := runChecks(ctx, &checkOpts, only)
	if err != nil {
		return err
	}
	engine, res, listed, now, ignored := run.engine, run.result, run.listed, run.now, run.ignored
	forbidden := listed.Forbidden
	available := listed.Available
	shown := make([]shownFinding, 0, len(run.findings))
	for _, f := range run.findings {
		shown = append(shown, shownFinding{f, f.Line(now)})
	}
	notAllowed := make([]string, 0, len(forbidden))
	for _, gvr := range forbidden {
		notAllowed = append(notAllowed, checks.FormatResource(gvr.GroupResource()))
	}
	notChecked := make([]string, 0, len(listed.Failed))
	for _, err := range listed.Failed {
		notChecked = append(notChecked, err.Error())
	}
	took := time.Since(now)

	health := 0
	out := cmd.OutOrStdout()
	if checkOpts.filter.output == "json" {
		result := checkResult{
			Findings: []jsonFinding{}, NotAllowed: notAllowed, NotChecked: notChecked,
			Objects: res.Objects, Types: len(available), Checks: len(engine.Checks()), DurationMS: took.Milliseconds(),
			Ignored: ignored,
		}
		for _, s := range shown {
			result.Findings = append(result.Findings, toJSONFinding(s.finding))
			if s.finding.Category == checks.CategoryHealth {
				health++
			}
		}
		sortJSONFindings(result.Findings)
		slices.Sort(result.NotAllowed)
		if err := writeJSON(out, result); err != nil {
			return err
		}
	} else {
		var hygiene int
		health, hygiene = printFindingLines(out, shown)
		if len(notAllowed) > 0 {
			_, _ = fmt.Fprintf(out, "Not allowed to list: %s\n", joinSorted(notAllowed))
		}
		for _, s := range notChecked {
			_, _ = fmt.Fprintf(out, "Not checked: %s\n", s)
		}
		_, _ = fmt.Fprintf(out, "Checked %s of %s with %s in %s: %s.\n",
			plural(res.Objects, "object"), plural(len(available), "type"), plural(len(engine.Checks()), "check"), took.Round(time.Millisecond),
			checkOpts.filter.countText(health, hygiene)+ignoredText(ignored))
	}
	if health > 0 {
		return errFindings
	}
	if len(listed.Failed) > 0 {
		// Do not report "healthy" for a cluster which was only partly checked.
		return errIncomplete
	}
	return nil
}

// checkResult is the output of `check -o json`.
type checkResult struct {
	Findings []jsonFinding `json:"findings"`
	// NotAllowed are the resource types RBAC does not allow to list.
	NotAllowed []string `json:"notAllowed"`
	// NotChecked are errors of resource types or API groups which could
	// not be listed.
	NotChecked []string `json:"notChecked"`
	// Ignored is the number of findings dropped by ignore rules.
	Ignored    int   `json:"ignored"`
	Objects    int   `json:"objects"`
	Types      int   `json:"types"`
	Checks     int   `json:"checks"`
	DurationMS int64 `json:"durationMs"`
}

func matchGlob(s string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, s); err == nil && ok {
			return true
		}
	}
	return false
}
