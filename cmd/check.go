package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"path"
	"regexp"
	"sort"
	"strings"
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
	kubeContext   string
	checks        checkFlags
	useDB         bool
	historyWindow time.Duration
}

// errFindings makes `konmon check` exit with 1 without printing an error.
var errFindings = errors.New("findings")

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Run all checks once and print the findings",
	Long: `List all objects of the cluster once, run the checks and print the
findings, like check-conditions does. Nothing is stored. The exit code is 1
if there are health findings.

Checks which use condition history need --use-db: the history is read from
the database of a running 'konmon run' (--db).`,
	Args:          cobra.NoArgs,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		err := check(cmd.Context(), cmd)
		if err != nil && !errors.Is(err, errFindings) {
			cmd.PrintErrln("Error:", err)
		}
		return err
	},
}

func init() {
	f := checkCmd.Flags()
	f.StringVar(&checkOpts.kubeContext, "context", "", "Kubeconfig context to use. Default: current context.")
	checkOpts.checks.register(f)
	f.BoolVar(&checkOpts.useDB, "use-db", false, "Read condition history for checks from --db.")
	f.DurationVar(&checkOpts.historyWindow, "history-window", 24*time.Hour, "How much condition history checks can see.")
	rootCmd.AddCommand(checkCmd)
}

func check(ctx context.Context, cmd *cobra.Command) error {
	log, err := newLogger()
	if err != nil {
		return err
	}
	klog.SetSlogLogger(log)
	engine, _, err := checkOpts.checks.engine(log)
	if err != nil {
		return err
	}
	ignore, err := checkOpts.checks.ignoreRegexps()
	if err != nil {
		return err
	}
	_, dyn, disc, err := clients(checkOpts.kubeContext)
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
			st, err := store.Open(ctx, dbPath)
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
	var lines []string
	health := 0
	for i := range res.Findings {
		f := &res.Findings[i]
		if matchAny(ignore, f.Subject()) {
			continue
		}
		if f.Category == checks.CategoryHealth {
			health++
		}
		lines = append(lines, f.Line(now))
	}
	sort.Strings(lines)
	out := cmd.OutOrStdout()
	for _, l := range lines {
		_, _ = fmt.Fprintln(out, l)
	}
	if len(forbidden) > 0 {
		names := make([]string, 0, len(forbidden))
		for _, gvr := range forbidden {
			names = append(names, checks.FormatResource(gvr.GroupResource()))
		}
		sort.Strings(names)
		_, _ = fmt.Fprintf(out, "Not allowed to list: %s\n", strings.Join(names, ", "))
	}
	for _, err := range listed.Failed {
		_, _ = fmt.Fprintf(out, "Not checked: %v\n", err)
	}
	_, _ = fmt.Fprintf(out, "Checked %d objects of %d types with %d checks in %s: %d findings.\n",
		res.Objects, len(available), len(engine.Checks()), time.Since(now).Round(time.Millisecond), len(lines))
	if health > 0 {
		return errFindings
	}
	if len(listed.Failed) > 0 {
		// Do not report "healthy" for a cluster which was only partly checked.
		return fmt.Errorf("%d resource types or API groups could not be listed", len(listed.Failed))
	}
	return nil
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
