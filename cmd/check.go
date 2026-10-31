package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/signal"
	"path"
	"regexp"
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
	// ignoreYounger and ignoreOlder: a duration or an RFC3339 timestamp.
	ignoreYounger, ignoreOlder string
}

func (o *checkOptions) register(f *pflag.FlagSet) {
	o.kube.register(f)
	o.checks.register(f)
	o.filter.register(f)
	f.BoolVar(&o.useDB, "use-db", false, "Read condition history for checks from --db.")
	f.DurationVar(&o.historyWindow, "history-window", 24*time.Hour, "How much condition history checks can see.")
	f.StringVar(&o.ignoreYounger, "ignore-younger-than", "",
		"Hide findings whose problem started more recently than this: a duration (5m: started in the last 5 minutes) or an RFC3339 timestamp. Findings without a start time (e.g. a missing resource request) are always shown.")
	f.StringVar(&o.ignoreOlder, "ignore-older-than", "",
		"Hide findings whose problem started before this: a duration (24h: unchanged for a day) or an RFC3339 timestamp. Combine with --ignore-younger-than for a time window.")
}

// ageWindow returns the start times outside of which findings are hidden.
// Zero: no limit.
func (o *checkOptions) ageWindow(now time.Time) (notBefore, notAfter time.Time, err error) {
	if notBefore, err = parseTime(o.ignoreOlder, now); err != nil {
		return notBefore, notAfter, fmt.Errorf("--ignore-older-than: %w", err)
	}
	if notAfter, err = parseTime(o.ignoreYounger, now); err != nil {
		return notBefore, notAfter, fmt.Errorf("--ignore-younger-than: %w", err)
	}
	return notBefore, notAfter, nil
}

// inAgeWindow reports whether a finding is not hidden by
// --ignore-younger-than and --ignore-older-than.
func inAgeWindow(f *checks.Finding, notBefore, notAfter time.Time) bool {
	if f.Check == checks.ErrorCheck || f.Since.IsZero() {
		return true
	}
	if !notBefore.IsZero() && f.Since.Before(notBefore) {
		return false
	}
	if !notAfter.IsZero() && f.Since.After(notAfter) {
		return false
	}
	return true
}

// checkLoop are the flags of 'konmon check' which repeat it.
type checkLoop struct {
	while    string
	repeat   bool
	interval time.Duration
	timeout  time.Duration
	name     string
}

func (l *checkLoop) register(f *pflag.FlagSet) {
	f.StringVar(&l.while, "while", "",
		"Repeat while a health finding matches this regular expression, then exit 0. Only matching findings are shown. '.' waits until there are no health findings.")
	f.BoolVar(&l.repeat, "repeat", false, "Repeat forever (until Ctrl-C).")
	f.DurationVar(&l.interval, "interval", 15*time.Second, "With --while or --repeat: pause between two runs.")
	f.DurationVar(&l.timeout, "timeout", 0, "Give up after this time, exit code 3. 0: no limit.")
	f.StringVar(&l.name, "name", "", "Text for the summary line, to tell several terminals apart.")
}

var checkLoopOpts checkLoop

var checkOpts checkOptions

// exitError ends konmon with a code, without printing an error.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

const (
	// errFindings: there are health findings.
	errFindings = exitError(1)
	// errIncomplete: no findings, but not everything could be checked.
	errIncomplete = exitError(2)
	// errCheckFailed: the check could not run (bad flags, no cluster).
	errCheckFailed = exitError(3)
	// errInterrupted: Ctrl-C or SIGTERM.
	errInterrupted = exitError(130)
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
forbidden ones, which are printed); 3 konmon could not check (invalid
flags, cluster not reachable, --timeout).

--repeat runs the check again and again, --while until no health finding
matches a regular expression: 'konmon check --while StillProvisioning;
music' plays music once the condition is gone. In both, network errors
are printed and retried.

Checks which use condition history need --use-db: the history is read from
the database of a running 'konmon run' (--db).

Examples:
  konmon check --context prod
  konmon check -n my-app --category health
  konmon check conditions 'pod-*'
  konmon check -o json | jq '.findings[] | select(.severity == "critical")'
  konmon check --while . --timeout 10m   # wait until healthy
  konmon check --repeat --ignore-younger-than 5m`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return check(cmd.Context(), cmd, args)
	},
}

func init() {
	checkOpts.register(checkCmd.Flags())
	checkLoopOpts.register(checkCmd.Flags())
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
	if _, _, err := o.ageWindow(time.Now()); err != nil {
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
	notBefore, notAfter, err := o.ageWindow(now)
	if err != nil {
		return nil, err
	}
	run := &checkRun{engine: engine, ignore: ignore, listed: listed, result: res, available: len(listed.Available), now: now}
	for i := range res.Findings {
		f := &res.Findings[i]
		if !o.filter.match(f) || !inAgeWindow(f, notBefore, notAfter) {
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
	l := &checkLoopOpts
	var while *regexp.Regexp
	if l.while != "" {
		var err error
		if while, err = regexp.Compile(l.while); err != nil {
			return fmt.Errorf("--while: %w", err)
		}
	}
	if l.interval <= 0 {
		return errors.New("--interval must be positive")
	}
	if err := checkOpts.filter.validate(); err != nil {
		return err
	}
	if _, _, err := checkOpts.ageWindow(time.Now()); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runCtx := ctx
	if l.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}
	out := cmd.OutOrStdout()
	if checkOpts.filter.output == "text" {
		cluster, kubeconfig := checkOpts.kube.describe()
		_, _ = fmt.Fprintf(out, "Cluster:    %s\nKubeconfig: %s\n\n", cluster, kubeconfig)
	}
	once := func(ctx context.Context) (bool, error) { return checkOnce(ctx, cmd, only, while) }
	var err error
	if while == nil && !l.repeat {
		_, err = once(runCtx)
	} else {
		err = repeatCheck(runCtx, cmd.ErrOrStderr(), l.interval, while != nil, once)
	}
	switch {
	case ctx.Err() != nil:
		return errInterrupted
	case runCtx.Err() != nil:
		return fmt.Errorf("timeout after %s", l.timeout)
	}
	return err
}

// repeatCheck calls once until it reports no match (with while) or ctx
// ends. Network errors are printed and retried, other errors only after a
// successful run: a wrong flag ends it at once.
func repeatCheck(ctx context.Context, msg io.Writer, interval time.Duration, while bool, once func(context.Context) (bool, error)) error {
	start := time.Now()
	succeeded := false
	for {
		matched, err := once(ctx)
		if errors.Is(err, errFindings) || errors.Is(err, errIncomplete) {
			err = nil
		}
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			var netErr net.Error
			if !succeeded && !errors.As(err, &netErr) {
				return err
			}
			_, _ = fmt.Fprintf(msg, "Error: %v. Trying again.\n", err)
		case while && !matched:
			_, _ = fmt.Fprintln(msg, "No health finding matches --while. Done.")
			return nil
		default:
			succeeded = true
		}
		_, _ = fmt.Fprintf(msg, "Next run in %s (running for %s).\n\n", interval, time.Since(start).Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// checkOnce lists the cluster, runs the checks and prints the findings.
// With while, only health findings matching it are shown, and matched
// reports whether there was one.
func checkOnce(ctx context.Context, cmd *cobra.Command, only []string, while *regexp.Regexp) (matched bool, err error) {
	start := time.Now()
	run, err := runChecks(ctx, &checkOpts, only)
	if err != nil {
		return false, err
	}
	engine, res, listed, now, ignored := run.engine, run.result, run.listed, run.now, run.ignored
	forbidden := listed.Forbidden
	available := listed.Available
	shown := make([]shownFinding, 0, len(run.findings))
	for _, f := range run.findings {
		line := f.Line(now)
		if while != nil {
			if f.Category != checks.CategoryHealth || !while.MatchString(line) {
				continue
			}
			matched = true
		}
		shown = append(shown, shownFinding{f, line})
	}
	notAllowed := make([]string, 0, len(forbidden))
	for _, gvr := range forbidden {
		notAllowed = append(notAllowed, checks.FormatResource(gvr.GroupResource()))
	}
	notChecked := make([]string, 0, len(listed.Failed))
	for _, err := range listed.Failed {
		notChecked = append(notChecked, err.Error())
	}
	took := time.Since(start)

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
			return false, err
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
		counts := checkOpts.filter.countText(health, hygiene)
		if while != nil {
			counts = plural(health, "health finding") + " matching --while"
		}
		name := ""
		if checkLoopOpts.name != "" {
			name = " (" + checkLoopOpts.name + ")"
		}
		_, _ = fmt.Fprintf(out, "%s Checked %s of %s with %s in %s: %s.%s\n", now.Format(time.TimeOnly),
			plural(res.Objects, "object"), plural(len(available), "type"), plural(len(engine.Checks()), "check"), took.Round(time.Millisecond),
			counts+ignoredText(ignored), name)
		if slices.ContainsFunc(shown, func(s shownFinding) bool { return s.finding.Check == "conditions" }) {
			// stderr: not part of the output which scripts read.
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "A finding is wrong? 'konmon triage' fixes it in your config; 'konmon report false-positive' tells the maintainers.")
		}
	}
	if health > 0 {
		return matched, errFindings
	}
	if len(listed.Failed) > 0 {
		// Do not report "healthy" for a cluster which was only partly checked.
		return matched, errIncomplete
	}
	return matched, nil
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
