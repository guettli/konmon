package cmd

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/config"
	"github.com/guettli/konmon/internal/store"
)

var (
	triageOpts          checkOptions
	triageFromDB        bool
	triageBaseline      bool
	triageBaselineUntil string
)

var triageCmd = &cobra.Command{
	Use:   "triage [CHECK...]",
	Short: "Walk through the findings and write ignore rules and condition polarity to a config file",
	Long: `Walk through the open findings, grouped by pattern, and decide for each
group: teach konmon the polarity of a condition, ignore findings like it (with
a scope you choose, a comment and an optional expiry), snooze them, or keep
them as a real problem. The answers are written to the config file (the last
--config, created if missing) after each decision, keeping its comments.
The cluster is never changed: review the file and deploy it like any config.

Findings come from listing the cluster once, like 'konmon check' (same flags),
or with --from-db from the database of a running 'konmon run' (--db).

With --baseline nothing is asked: every open finding gets a narrow ignore
rule (this object) which expires after --baseline-until, so the cluster is
quiet now and only new problems are reported. Running it again adds only new
findings. Failing checks and acknowledged findings are never baselined.

Examples:
  konmon triage --context prod --config konmon.yaml
  konmon triage --config shared.yaml --config prod.yaml -n 'team-*'
  konmon triage --from-db --db /data/konmon.db --config konmon.yaml
  konmon triage --context prod --config konmon.yaml --baseline --category hygiene`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return triage(cmd.Context(), cmd, args)
	},
}

func init() {
	triageOpts.register(triageCmd.Flags())
	triageCmd.Flags().BoolVar(&triageFromDB, "from-db", false, "Take the open findings from the database of 'konmon run' (--db) instead of listing the cluster.")
	triageCmd.Flags().BoolVar(&triageBaseline, "baseline", false,
		"Do not ask: write one narrow ignore rule per open finding, so only new problems are reported. Like the baseline of a linter.")
	triageCmd.Flags().StringVar(&triageBaselineUntil, "baseline-until", "90d",
		"Expiry of the --baseline rules: YYYY-MM-DD or a number of days (90d), so the findings come back for a real triage. Empty: never.")
	rootCmd.AddCommand(triageCmd)
}

func triage(ctx context.Context, cmd *cobra.Command, only []string) error {
	configs := triageOpts.checks.configs
	if len(configs) == 0 {
		return errors.New("--config FILE is required: the answers are written to it (it is created if missing)")
	}
	if !triageBaseline && cmd.Flags().Changed("baseline-until") {
		return errors.New("--baseline-until needs --baseline")
	}
	if triageBaseline {
		date, err := parseUntil(triageBaselineUntil, time.Now())
		if err != nil {
			return fmt.Errorf("--baseline-until: %w", err)
		}
		triageBaselineUntil = date
	}
	target := configs[len(configs)-1]
	editor, err := config.OpenEditor(target)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		// Created on the first answer; until then it has no rules.
		triageOpts.checks.configs = configs[:len(configs)-1]
	}
	base, err := triageOpts.checks.config()
	if err != nil {
		return err
	}

	// Ctrl-C while listing aborts; at the prompt it ends the program, the
	// answers are already saved.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)

	// findings are triaged; universe (not narrowed by -n) is what the
	// counts of the ignore ladder are based on.
	var findings, universe []*checks.Finding
	now := time.Now()
	var st *store.Store
	if triageFromDB || triageOpts.useDB {
		st, err = store.OpenExisting(ctx, dbPath)
		if err != nil {
			stop()
			return err
		}
		defer func() { _ = st.Close() }()
	}
	if triageFromDB {
		if err := triageOpts.filter.validate(); err != nil {
			stop()
			return err
		}
		ignore, err := triageOpts.checks.ignorer()
		if err != nil {
			stop()
			return err
		}
		rows, err := st.FindingsAt(sigCtx, now)
		if err != nil {
			stop()
			return err
		}
		for i := range rows {
			f := &rows[i].Finding
			if len(only) > 0 && !matchGlob(f.Check, only) {
				continue
			}
			if f.Namespace != "" && matchGlob(f.Namespace, triageOpts.checks.excludeNamespaces) {
				continue
			}
			if ignore.ignored(f, now) != "" {
				continue
			}
			universe = append(universe, f)
			if triageOpts.filter.match(f) {
				findings = append(findings, f)
			}
		}
	} else {
		run, err := runChecks(sigCtx, &triageOpts, only)
		if err != nil {
			stop()
			return err
		}
		findings, now = run.findings, run.now
		for i := range run.result.Findings {
			if f := &run.result.Findings[i]; run.ignore.ignored(f, now) == "" {
				universe = append(universe, f)
			}
		}
	}
	stop()

	t := &triageSession{
		in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout(),
		editor: editor, base: base, all: findings, universe: universe, now: now, store: st, ctx: ctx,
		narrowed: len(only) > 0 || len(triageOpts.filter.categories) > 0,
	}
	if triageBaseline {
		return t.baseline(triageBaselineUntil)
	}
	return t.run()
}

// triageGroup are findings of one pattern: check, resource type, condition,
// status and reason.
type triageGroup struct {
	check, resource, condition, status, reason string
	findings                                   []*checks.Finding
}

func (g *triageGroup) category() string { return g.findings[0].Category }

func (g *triageGroup) title() string {
	s := g.check + "  " + g.resource
	if g.condition != "" {
		s += "  " + g.condition
		if g.status != "" {
			s += "=" + g.status
		}
		if g.reason != "" {
			s += " " + g.reason
		}
	}
	return s
}

func groupFindings(findings []*checks.Finding) []*triageGroup {
	byKey := map[string]*triageGroup{}
	var groups []*triageGroup
	for _, f := range findings {
		g := &triageGroup{
			check: f.Check, resource: findingResource(f), condition: f.Condition,
			status: f.Details["status"], reason: f.Details["reason"],
		}
		key := strings.Join([]string{g.check, g.resource, g.condition, g.status, g.reason}, "|")
		if old, ok := byKey[key]; ok {
			g = old
		} else {
			byKey[key] = g
			groups = append(groups, g)
		}
		g.findings = append(g.findings, f)
	}
	slices.SortFunc(groups, func(a, b *triageGroup) int {
		return cmp.Or(
			cmp.Compare(categoryRank(a.category()), categoryRank(b.category())),
			cmp.Compare(len(b.findings), len(a.findings)),
			cmp.Compare(a.title(), b.title()),
		)
	})
	return groups
}

func findingResource(f *checks.Finding) string {
	if f.GVR.Resource == "" {
		return ""
	}
	return checks.FormatResource(f.GVR.GroupResource())
}

func findingObject(f *checks.Finding) string {
	if f.Namespace == "" {
		return f.Name
	}
	return f.Namespace + "/" + f.Name
}

type triageSession struct {
	ctx    context.Context
	in     *bufio.Reader
	out    io.Writer
	editor *config.Editor
	// base is the config of all --config files when the session started,
	// for acknowledged rules of earlier files.
	base *config.Config
	all  []*checks.Finding
	// universe are all findings of the run which are not ignored, also
	// outside -n: the rules are not limited to it.
	universe []*checks.Finding
	// narrowed: only some checks ran, the counts may be too low.
	narrowed bool
	now      time.Time
	store    *store.Store // nil: no history
	changes  int
}

var errQuit = errors.New("quit")

func (t *triageSession) printf(format string, a ...any) { _, _ = fmt.Fprintf(t.out, format, a...) }

// ask prints the prompt and returns the trimmed answer; io.EOF ends the
// session.
func (t *triageSession) ask(prompt string) (string, error) {
	t.printf("%s", prompt)
	line, err := t.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		t.printf("\n")
		return "", errQuit
	}
	return strings.TrimSpace(line), nil
}

func (t *triageSession) run() error {
	groups := groupFindings(t.all)
	if len(groups) == 0 {
		t.printf("No findings to triage.\n")
		return nil
	}
	t.printf("%s in %d groups. Answers are saved to %s after each decision.\n", plural(len(t.all), "finding"), len(groups), t.editor.Path())
	for i, g := range groups {
		cfg, err := t.editor.Config()
		if err != nil {
			return err
		}
		if t.covered(g, cfg) {
			continue
		}
		if err := t.group(i+1, len(groups), g); err != nil {
			if errors.Is(err, errQuit) {
				break
			}
			return err
		}
	}
	if t.changes == 0 {
		t.printf("No changes.\n")
	} else {
		t.printf("Saved %s to %s. Review it (git diff) and deploy it with --config.\n", plural(t.changes, "change"), t.editor.Path())
	}
	return nil
}

// covered reports whether the answers so far already handle every finding of
// the group.
func (t *triageSession) covered(g *triageGroup, cfg *config.Config) bool {
	for _, f := range g.findings {
		if cfg.Ignored(f, t.now) != nil || cfg.IsAcknowledged(f, t.now) != nil || t.base.IsAcknowledged(f, t.now) != nil {
			continue
		}
		if g.check == "conditions" && g.condition != "" && healthyByPolarity(cfg, g) {
			continue
		}
		return false
	}
	return true
}

// healthyByPolarity reports whether the taught polarity makes every
// condition type of the group healthy with its status, like taught() in
// checks/conditions.ts: the rules of the resource first, then "*".
func healthyByPolarity(cfg *config.Config, g *triageGroup) bool {
	for _, typ := range strings.Split(g.condition, "/") {
		verdict := ""
		for _, key := range []string{g.resource, "*"} {
			r := cfg.Conditions[key]
			if r == nil || verdict != "" {
				continue
			}
			switch {
			case slices.Contains(r.IgnoreTypes, typ):
				verdict = "skip"
			case slices.Contains(r.HealthyWhenTrue, typ):
				verdict = "True"
			case slices.Contains(r.HealthyWhenFalse, typ):
				verdict = "False"
			}
		}
		if verdict != "skip" && verdict != g.status {
			return false
		}
	}
	return true
}

func (t *triageSession) group(n, total int, g *triageGroup) error {
	namespaces := map[string]bool{}
	var oldest time.Time
	for _, f := range g.findings {
		namespaces[f.Namespace] = true
		if !f.Since.IsZero() && (oldest.IsZero() || f.Since.Before(oldest)) {
			oldest = f.Since
		}
	}
	t.printf("\n[%d/%d] %s   (%s, %s)\n", n, total, g.title(), g.category(), g.findings[0].Severity)
	where := plural(len(g.findings), "finding")
	if len(namespaces) > 1 || !namespaces[""] {
		where += " in " + plural(len(namespaces), "namespace")
	}
	if !oldest.IsZero() {
		where += ", oldest since " + t.now.Sub(oldest).Round(time.Minute).String()
	}
	t.printf("      %s\n      e.g. %s: %s\n", where, findingObject(g.findings[0]), g.findings[0].Message)

	canTeach := g.check == "conditions" && g.condition != ""
	options := "  i) ignore   s) snooze   k) keep (real problem)   ?) details   n) next   q) quit\n> "
	if canTeach {
		options = "  t) teach polarity  " + options[2:]
	}
	for {
		answer, err := t.ask(options)
		if err != nil {
			return err
		}
		switch answer {
		case "t":
			if !canTeach {
				t.printf("  Only conditions findings can be taught.\n")
				continue
			}
			done, err := t.teach(g)
			if err != nil || done {
				return err
			}
		case "i":
			done, err := t.ignore(g)
			if err != nil || done {
				return err
			}
		case "s":
			done, err := t.snooze(g)
			if err != nil || done {
				return err
			}
		case "k":
			done, err := t.keep(g)
			if err != nil || done {
				return err
			}
		case "?":
			t.details(g)
		case "n", "":
			return nil
		case "q":
			return errQuit
		default:
			t.printf("  Unknown answer %q.\n", answer)
		}
	}
}

func (t *triageSession) teach(g *triageGroup) (bool, error) {
	types := strings.Split(g.condition, "/")
	answer, err := t.ask(fmt.Sprintf("  Which status of %s is healthy? t) True  f) False  b) both (informational)  c) cancel\n> ", strings.Join(types, ", ")))
	if err != nil {
		return false, err
	}
	list := map[string]string{"t": config.HealthyWhenTrue, "f": config.HealthyWhenFalse, "b": config.IgnoreTypes}[answer]
	if list == "" {
		if answer != "c" {
			t.printf("  Unknown answer %q, nothing changed.\n", answer)
		}
		return false, nil
	}
	answer, err = t.ask(fmt.Sprintf("  For which resources? r) %s (default)  a) all resources\n> ", g.resource))
	if err != nil {
		return false, err
	}
	resource := g.resource
	if answer == "a" {
		resource = "*"
	}
	if err := t.editor.Teach(resource, list, types); err != nil {
		return false, err
	}
	if err := t.save(); err != nil {
		return false, err
	}
	t.printf("  Added to conditions.%s.%s: %s\n", resource, list, strings.Join(types, ", "))
	if (list == config.HealthyWhenTrue && g.status == "False") || (list == config.HealthyWhenFalse && g.status == "True") {
		t.printf("  Note: the current status %s is unhealthy by this rule, these findings stay.\n", g.status)
	}
	return true, nil
}

// rung is one scope of the ignore ladder.
type rung struct {
	label string
	rule  config.IgnoreRule
	count int // current findings it hides
}

// globQuote escapes the glob characters of a value, so a rule matches it
// exactly (a key of an own check may contain "*" or "[").
func globQuote(s string) string {
	return globSpecial.ReplaceAllString(s, `\$0`)
}

var globSpecial = regexp.MustCompile(`[*?\[\\]`)

// groupRule matches the findings of the group everywhere.
func groupRule(g *triageGroup) config.IgnoreRule {
	r := config.IgnoreRule{Check: globQuote(g.check), Resource: globQuote(g.resource), Condition: globQuote(g.condition)}
	if g.check == "conditions" && g.condition != "" {
		// The message of the conditions check starts with this.
		r.Message = "^" + regexp.QuoteMeta(fmt.Sprintf("Condition %s=%s %s ", g.condition, g.status, g.reason))
	}
	return r
}

func (t *triageSession) ladder(g *triageGroup) []rung {
	f := g.findings[0]
	group := groupRule(g)
	var rungs []rung
	add := func(label string, r config.IgnoreRule) {
		r.Comment = "-" // counted before the comment is asked
		if r.Compile() != nil {
			return
		}
		for _, old := range rungs {
			if sameRule(&old.rule, &r) {
				return
			}
		}
		count := 0
		for _, other := range t.universe {
			if r.Match(other, t.now) {
				count++
			}
		}
		rungs = append(rungs, rung{label: label, rule: r, count: count})
	}
	if f.Check == checks.ErrorCheck {
		// Never offer to hide every failing check with one key.
		add("only failures of check "+f.Key, config.IgnoreRule{Check: f.Check, Key: globQuote(f.Key)})
		return rungs
	}
	add("only "+findingObject(f), config.IgnoreRule{
		Check: globQuote(f.Check), Resource: globQuote(g.resource), Namespace: globQuote(f.Namespace),
		Name: globQuote(f.Name), Key: globQuote(f.Key), Condition: globQuote(f.Condition), Container: globQuote(f.Container),
	})
	if f.Namespace != "" {
		r := group
		r.Namespace = globQuote(f.Namespace)
		add("these findings in namespace "+f.Namespace, r)
	}
	add("these findings everywhere", group)
	if g.condition != "" {
		add(fmt.Sprintf("condition %s of %s, any status and reason", g.condition, g.resource), config.IgnoreRule{Check: g.check, Resource: g.resource, Condition: g.condition})
	}
	if g.resource != "" {
		add(fmt.Sprintf("every %s finding of %s", g.check, g.resource), config.IgnoreRule{Check: g.check, Resource: g.resource})
	}
	add("every finding of check "+g.check, config.IgnoreRule{Check: g.check})
	return rungs
}

func sameRule(a, b *config.IgnoreRule) bool {
	return a.Check == b.Check && a.Resource == b.Resource && a.Namespace == b.Namespace && a.Name == b.Name &&
		a.Condition == b.Condition && a.Container == b.Container && a.Key == b.Key && a.Message == b.Message
}

func (t *triageSession) ignore(g *triageGroup) (bool, error) {
	rungs := t.ladder(g)
	t.printf("  Ignore which findings?\n")
	if t.narrowed {
		t.printf("  (counts include only the checks of this run)\n")
	}
	for i, r := range rungs {
		extra := ""
		if r.count > len(g.findings) {
			extra = fmt.Sprintf("  (+%d more)", r.count-len(g.findings))
		}
		t.printf("  %d) %-60s hides %d%s\n", i+1, r.label, r.count, extra)
	}
	var i int
	for {
		answer, err := t.ask(fmt.Sprintf("  1-%d, or c) cancel\n> ", len(rungs)))
		if err != nil {
			return false, err
		}
		if answer == "c" {
			return false, nil
		}
		if i, err = strconv.Atoi(answer); err == nil && i >= 1 && i <= len(rungs) {
			break
		}
	}
	var err error
	rule := rungs[i-1].rule
	if rule.Comment, err = t.comment("Why are these findings fine", ""); err != nil {
		return false, err
	}
	if rule.Until, err = t.until("Expires (YYYY-MM-DD or 30d, empty: never)", ""); err != nil {
		return false, err
	}
	return t.addIgnore(&rule)
}

func (t *triageSession) snooze(g *triageGroup) (bool, error) {
	rule := groupRule(g)
	var err error
	if rule.Until, err = t.until("Snooze until (YYYY-MM-DD or 30d)", "7d"); err != nil {
		return false, err
	}
	if rule.Comment, err = t.comment("Comment", "snoozed"); err != nil {
		return false, err
	}
	return t.addIgnore(&rule)
}

func (t *triageSession) addIgnore(rule *config.IgnoreRule) (bool, error) {
	if err := t.editor.AddIgnore(rule); err != nil {
		return false, err
	}
	if err := t.save(); err != nil {
		return false, err
	}
	t.printf("  Added an ignore rule.\n")
	return true, nil
}

func (t *triageSession) keep(g *triageGroup) (bool, error) {
	rule := groupRule(g)
	var err error
	if rule.Comment, err = t.comment("Comment (e.g. a ticket)", ""); err != nil {
		return false, err
	}
	if err := t.editor.AddAcknowledged(&rule); err != nil {
		return false, err
	}
	if err := t.save(); err != nil {
		return false, err
	}
	t.printf("  Acknowledged: still reported and alerted, triage does not ask again.\n")
	return true, nil
}

func (t *triageSession) comment(prompt, def string) (string, error) {
	for {
		p := "  " + prompt
		if def != "" {
			p += " [" + def + "]"
		}
		answer, err := t.ask(p + ": ")
		if err != nil {
			return "", err
		}
		if answer == "" {
			answer = def
		}
		if answer != "" {
			return answer, nil
		}
		t.printf("  A comment is required.\n")
	}
}

func (t *triageSession) until(prompt, def string) (string, error) {
	for {
		p := "  " + prompt
		if def != "" {
			p += " [" + def + "]"
		}
		answer, err := t.ask(p + ": ")
		if err != nil {
			return "", err
		}
		if answer == "" {
			answer = def
		}
		if answer == "" {
			return "", nil
		}
		date, err := parseUntil(answer, t.now)
		if err == nil {
			return date, nil
		}
		t.printf("  %s.\n", upperFirst(err.Error()))
	}
}

func (t *triageSession) details(g *triageGroup) {
	for i, f := range g.findings {
		if i == 10 {
			t.printf("  … and %d more\n", len(g.findings)-10)
			break
		}
		t.printf("%s\n", f.Line(t.now))
	}
	if t.store == nil {
		t.printf("  (condition history: run with --from-db or --use-db)\n")
		return
	}
	f := g.findings[0]
	objs, err := t.store.FindObjects(t.ctx, f.GVR.Resource, f.GVR.Group, f.Namespace, f.Name)
	if err != nil || len(objs) == 0 {
		return
	}
	t.printf("  History of %s, last 7 days:\n", findingObject(f))
	_ = printHistory(t.ctx, t.out, t.store, &objs[len(objs)-1], t.now.Add(-7*24*time.Hour))
}

func (t *triageSession) save() error {
	if err := t.editor.Save(); err != nil {
		return err
	}
	t.changes++
	return nil
}

// baseline writes one narrow ignore rule per finding, without asking.
func (t *triageSession) baseline(until string) error {
	until, err := parseUntil(until, t.now)
	if err != nil {
		return fmt.Errorf("--baseline-until: %w", err)
	}
	comment := "baseline " + t.now.UTC().Format(time.DateOnly)
	added, skipped := 0, 0
	for _, f := range t.all {
		if f.Check == checks.ErrorCheck || t.base.IsAcknowledged(f, t.now) != nil {
			skipped++
			continue
		}
		rule := &config.IgnoreRule{
			Check: globQuote(f.Check), Resource: globQuote(findingResource(f)), Namespace: globQuote(f.Namespace),
			Name: globQuote(f.Name), Key: globQuote(f.Key), Condition: globQuote(f.Condition), Container: globQuote(f.Container),
			Until: until, Comment: comment,
		}
		if err := t.editor.AddIgnore(rule); err != nil {
			return fmt.Errorf("%s: %w", f.Line(t.now), err)
		}
		added++
	}
	if added > 0 {
		if err := t.save(); err != nil {
			return err
		}
	}
	switch {
	case added == 0:
		t.printf("Baseline: no new findings.\n")
	case until != "":
		t.printf("Baseline: added %s to %s, expiring after %s.\n", plural(added, "ignore rule"), t.editor.Path(), until)
	default:
		t.printf("Baseline: added %s to %s.\n", plural(added, "ignore rule"), t.editor.Path())
	}
	if skipped > 0 {
		t.printf("Not baselined: %s (failing checks or acknowledged).\n", plural(skipped, "finding"))
	}
	return nil
}

// parseUntil accepts "", YYYY-MM-DD (today or later) or a number of days
// ("30d") and returns the date.
func parseUntil(s string, now time.Time) (string, error) {
	if s == "" {
		return "", nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return now.UTC().AddDate(0, 0, n).Format(time.DateOnly), nil
		}
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return "", errors.New("want YYYY-MM-DD or a number of days like 30d")
	}
	if d.Before(now.UTC().Truncate(24 * time.Hour)) {
		return "", errors.New("that day is in the past")
	}
	return s, nil
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
