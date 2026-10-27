package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"path"
	"regexp"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/monitor"
	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/watch"
)

var checkOpts struct {
	kube          kubeFlags
	checks        checkFlags
	filter        findingFilter
	useDB         bool
	historyWindow time.Duration
}

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
	f := checkCmd.Flags()
	checkOpts.kube.register(f)
	checkOpts.checks.register(f)
	checkOpts.filter.register(f)
	f.BoolVar(&checkOpts.useDB, "use-db", false, "Read condition history for checks from --db.")
	f.DurationVar(&checkOpts.historyWindow, "history-window", 24*time.Hour, "How much condition history checks can see.")
	rootCmd.AddCommand(checkCmd)
}

func check(ctx context.Context, cmd *cobra.Command, only []string) error {
	if err := checkOpts.filter.validate(); err != nil {
		return err
	}
	log, err := newLogger()
	if err != nil {
		return err
	}
	klog.SetSlogLogger(log)
	engine, _, err := checkOpts.checks.engine(log, only, checkOpts.filter.categories)
	if err != nil {
		return err
	}
	ignore, err := checkOpts.checks.ignoreRegexps()
	if err != nil {
		return err
	}
	_, dyn, disc, err := checkOpts.kube.clients()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w := &watch.Watcher{Dynamic: dyn, Discovery: disc, Include: engine.Watch, Log: log}
	warnUnserved(engine, disc, log)
	listed, err := w.ListOnce(ctx)
	if err != nil {
		return err
	}
	available, forbidden := listed.Available, listed.Forbidden
	var snapItems []checks.Item
	for _, it := range listed.Items {
		if ns := it.Obj.GetNamespace(); ns != "" && matchGlob(ns, checkOpts.checks.excludeNamespaces) {
			continue
		}
		snapItems = append(snapItems, checks.Item{GVR: it.GVR, Obj: it.Obj})
	}

	var hist checks.History
	now := time.Now()
	if engine.UsesHistory() {
		if !checkOpts.useDB {
			log.Warn("some checks use condition history; without --use-db they fail")
		} else {
			st, err := store.OpenExisting(ctx, dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			rows, err := st.ConditionHistorySince(ctx, now.Add(-checkOpts.historyWindow))
			if err != nil {
				return err
			}
			hist = monitor.StoreHistory(rows)
		}
	}

	res, err := engine.Evaluate(ctx, checks.NewSnapshot(snapItems, available), hist, now)
	if err != nil {
		return err
	}
	var shown []shownFinding
	for i := range res.Findings {
		f := &res.Findings[i]
		if matchAny(ignore, f.Subject()) || !checkOpts.filter.match(f) {
			continue
		}
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
			checkOpts.filter.countText(health, hygiene))
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
	NotAllowed []string `json:"notAllowed,omitempty"`
	// NotChecked are errors of resource types or API groups which could
	// not be listed.
	NotChecked []string `json:"notChecked,omitempty"`
	Objects    int      `json:"objects"`
	Types      int      `json:"types"`
	Checks     int      `json:"checks"`
	DurationMS int64    `json:"durationMs"`
}

func matchGlob(s string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, s); err == nil && ok {
			return true
		}
	}
	return false
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
