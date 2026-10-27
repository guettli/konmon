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
	triageOpts   checkOptions
	triageFromDB bool
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

Examples:
  konmon triage --context prod --config konmon.yaml
  konmon triage --config shared.yaml --config prod.yaml -n 'team-*'
  konmon triage --from-db --db /data/konmon.db --config konmon.yaml`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return triage(cmd.Context(), cmd, args)
	},
}

func init() {
	triageOpts.register(triageCmd.Flags())
	triageCmd.Flags().BoolVar(&triageFromDB, "from-db", false, "Take the open findings from the database of 'konmon run' (--db) instead of listing the cluster.")
	rootCmd.AddCommand(triageCmd)
}

func triage(ctx context.Context, cmd *cobra.Command, only []string) error {
	configs := triageOpts.checks.configs
	if len(configs) == 0 {
		return errors.New("--config FILE is required: the answers are written to it (it is created if missing)")
	}
	target := configs[len(configs)-1]
	editor, err := config.OpenEditor(target)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		if err := editor.Save(); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var findings []*checks.Finding
	now := time.Now()
	var st *store.Store
	if triageFromDB || triageOpts.useDB {
		st, err = store.OpenExisting(ctx, dbPath)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
	}
	if triageFromDB {
		if err := triageOpts.filter.validate(); err != nil {
			return err
		}
		ignore, err := triageOpts.checks.ignorer()
		if err != nil {
			return err
		}
		rows, err := st.FindingsAt(ctx, now)
		if err != nil {
			return err
		}
		for i := range rows {
			f := &rows[i].Finding
			if len(only) > 0 && !matchGlob(f.Check, only) {
				continue
			}
			if triageOpts.filter.match(f) && ignore.ignored(f, now) == "" {
				findings = append(findings, f)
			}
		}
	} else {
		run, err := runChecks(ctx, &triageOpts, only)
		if err != nil {
			return err
		}
		findings, now = run.findings, run.now
	}

	t := &triageSession{
		in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout(),
		editor: editor, all: findings, now: now, store: st, ctx: ctx,
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
	ctx     context.Context
	in      *bufio.Reader
	out     io.Writer
	editor  *config.Editor
	all     []*checks.Finding
	now     time.Time
	store   *store.Store // nil: no history
	changes int
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
		if cfg.Ignored(f, t.now) != nil || cfg.IsAcknowledged(f, t.now) != nil {
			continue
		}
		if g.check == "conditions" && g.condition != "" && taughtAll(cfg, g.resource, strings.Split(g.condition, "/")) {
			continue
		}
		return false
	}
	return true
}

func taughtAll(cfg *config.Config, resource string, types []string) bool {
	for _, typ := range types {
		found := false
		for _, key := range []string{resource, "*"} {
			r := cfg.Conditions[key]
			if r != nil && (slices.Contains(r.HealthyWhenTrue, typ) || slices.Contains(r.HealthyWhenFalse, typ) || slices.Contains(r.IgnoreTypes, typ)) {
				found = true
			}
		}
		if !found {
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
		options = "  t) teach polarity" + options[1:]
	}
	for {
		answer, err := t.ask(options)
		if err != nil {
			return err
		}
		switch answer {
		case "t":
			if !canTeach {
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

// groupRule matches the findings of the group everywhere.
func groupRule(g *triageGroup) config.IgnoreRule {
	r := config.IgnoreRule{Check: g.check, Resource: g.resource, Condition: g.condition}
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
		for _, other := range t.all {
			if r.Match(other, t.now) {
				count++
			}
		}
		rungs = append(rungs, rung{label: label, rule: r, count: count})
	}
	add("only "+findingObject(f), config.IgnoreRule{Check: f.Check, Resource: g.resource, Namespace: f.Namespace, Name: f.Name, Key: f.Key})
	if f.Namespace != "" {
		r := group
		r.Namespace = f.Namespace
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
	for i, r := range rungs {
		extra := ""
		if r.count > len(g.findings) {
			extra = fmt.Sprintf("  (+%d of other groups)", r.count-len(g.findings))
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
		if days, ok := strings.CutSuffix(answer, "d"); ok {
			if n, err := strconv.Atoi(days); err == nil && n > 0 {
				return t.now.UTC().AddDate(0, 0, n).Format(time.DateOnly), nil
			}
		}
		if _, err := time.Parse(time.DateOnly, answer); err == nil {
			return answer, nil
		}
		t.printf("  Want YYYY-MM-DD or a number of days like 30d.\n")
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
