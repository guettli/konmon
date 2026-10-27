package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	core "github.com/guettli/konmon/checks"
	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/config"
)

// checkFlags are shared by `run` and `check`.
type checkFlags struct {
	configs           []string
	dirs              []string
	disabled          []string
	params            []string
	ignoreFindings    []string
	excludeNamespaces []string
	excludeResources  []string
	timeout           time.Duration
}

func (c *checkFlags) register(f *pflag.FlagSet) {
	f.StringArrayVar(&c.configs, "config", nil, "Config file with ignore rules (YAML, see README). Can be given multiple times: the rules are merged.")
	f.StringArrayVar(&c.dirs, "checks", nil, "Directory with own checks (*.ts, *.js). A check with the name of a core check replaces it. Can be given multiple times.")
	f.StringSliceVar(&c.disabled, "disable-check", nil, "Checks to disable, by name. Comma-separated, glob patterns allowed.")
	f.StringArrayVar(&c.params, "check-param", nil, "Override a check parameter: check.param=value, e.g. pod-container-restarts.restarts=10. Can be given multiple times.")
	f.StringArrayVar(&c.ignoreFindings, "ignore-finding-regex", nil, "Drop findings matching this regex (Go syntax). Matched against: 'check resource namespace/name message'. Can be given multiple times.")
	f.StringSliceVar(&c.excludeNamespaces, "exclude-namespace", nil, "Namespaces to ignore (not stored, not checked). Comma-separated, glob patterns allowed.")
	f.StringSliceVar(&c.excludeResources, "exclude-resource", []string{"leases.coordination.k8s.io", "endpointslices.discovery.k8s.io", "events.events.k8s.io"},
		"Resource types not to check or store, as resource.group (core: resource). Comma-separated, glob patterns allowed. A check can still read them if it lists them in reads. Secrets are never read.")
	f.DurationVar(&c.timeout, "check-timeout", time.Second, "Time limit for one call of a check function.")
}

// engine loads the core checks and the checks of --checks. only (glob
// patterns) and categories keep just these checks; empty keeps all.
func (c *checkFlags) engine(log *slog.Logger, only, categories []string) (*checks.Engine, *checks.Files, error) {
	if _, err := globMatcher(c.excludeNamespaces); err != nil {
		return nil, nil, fmt.Errorf("--exclude-namespace: %w", err)
	}
	exclude, err := globMatcher(c.excludeResources)
	if err != nil {
		return nil, nil, fmt.Errorf("--exclude-resource: %w", err)
	}
	user := &checks.Files{Tests: map[string]*checks.TestFile{}}
	for _, dir := range c.dirs {
		files, err := checks.ReadFiles(os.DirFS(dir), dir)
		if err != nil {
			return nil, nil, fmt.Errorf("--checks %s: %w", dir, err)
		}
		user.Sources = append(user.Sources, files.Sources...)
		for p, tf := range files.Tests {
			user.Tests[p] = tf
		}
	}
	params := map[string]map[string]string{}
	for _, p := range c.params {
		key, value, ok := strings.Cut(p, "=")
		check, param, ok2 := strings.Cut(key, ".")
		if !ok || !ok2 || check == "" || param == "" {
			return nil, nil, fmt.Errorf("--check-param %q: want check.param=value", p)
		}
		if params[check] == nil {
			params[check] = map[string]string{}
		}
		params[check][param] = value
	}
	e, err := checks.Load(checks.CoreFiles().Sources, user.Sources, checks.Options{
		Disabled: c.disabled, Only: only, Categories: categories, Params: params, Timeout: c.timeout, Exclude: exclude, Log: log,
	})
	if err != nil {
		return nil, nil, err
	}
	return e, user, nil
}

// ignorer combines the rules of --config and --ignore-finding-regex. It
// returns nil if there are none.
func (c *checkFlags) ignorer() (*ignorer, error) {
	cfg, err := config.Load(c.configs)
	if err != nil {
		return nil, err
	}
	ig := &ignorer{config: cfg}
	for _, s := range c.ignoreFindings {
		r, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("invalid --ignore-finding-regex %q: %w", s, err)
		}
		ig.regexps = append(ig.regexps, r)
	}
	return ig, nil
}

// ignorer drops findings: rules of config files and --ignore-finding-regex.
type ignorer struct {
	config  *config.Config
	regexps []*regexp.Regexp
}

func (ig *ignorer) ignored(f *checks.Finding, now time.Time) bool {
	if ig.config.Ignored(f, now) != nil {
		return true
	}
	if f.Check == checks.ErrorCheck {
		return false // a failing check is never hidden by a regex
	}
	return matchAny(ig.regexps, f.Subject())
}

// warnExpired logs the rules which do not match any more.
func (ig *ignorer) warnExpired(log *slog.Logger, now time.Time) {
	for _, r := range ig.config.Expired(now) {
		log.Warn("ignore rule expired, findings it matched are reported again", "rule", r.Source, "until", r.Until, "comment", r.Comment)
	}
}

var checksFlags checkFlags

var checksCmd = &cobra.Command{
	Use:   "checks",
	Short: "List, test and write checks",
}

var checksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the enabled checks",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, _, err := checksFlags.engine(slog.New(slog.DiscardHandler), nil, nil)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tCATEGORY\tSEVERITY\tMATCH\tREADS\tSOURCE")
		for _, d := range e.Checks() {
			match := strings.Join(d.MatchResources, ",")
			if len(d.ExcludeTypes) > 0 {
				match += fmt.Sprintf(" (except %d)", len(d.ExcludeTypes))
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", d.Name, d.Category, d.Severity, match,
				strings.Join(d.ReadsResources, ","), d.Path)
		}
		return w.Flush()
	},
}

var checksTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Run the *.test.yaml files of the core checks and of --checks directories",
	Long: `Run the test cases next to the checks, without a cluster. Each
NAME.test.yaml holds cases for one check: an object, related objects, the
time and the expected findings. See the files in the checks directory of the
konmon repository.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, user, err := checksFlags.engine(slog.New(slog.DiscardHandler), nil, nil)
		if err != nil {
			return err
		}
		failed, total := 0, 0
		run := func(files *checks.Files, core bool) {
			for _, p := range files.TestPaths() {
				if !e.Tested(files.Tests[p], core) {
					// The core check is disabled or replaced by a user check.
					continue
				}
				for _, r := range e.RunTests(files.Tests[p]) {
					total++
					if r.Err != nil {
						failed++
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "FAIL %s: %s\n    %v\n", p, r.Case, r.Err)
					}
				}
			}
		}
		run(checks.CoreFiles(), true)
		run(user, false)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d of %d test cases passed\n", total-failed, total)
		if failed > 0 {
			return fmt.Errorf("%d test cases failed", failed)
		}
		return nil
	},
}

var checksTypesCmd = &cobra.Command{
	Use:   "types",
	Short: "Print konmon.d.ts, the TypeScript types for writing checks",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := io.WriteString(cmd.OutOrStdout(), core.Types())
		return err
	},
}

func init() {
	checksFlags.register(checksCmd.PersistentFlags())
	checksCmd.AddCommand(checksListCmd, checksTestCmd, checksTypesCmd)
	rootCmd.AddCommand(checksCmd)
}

// storeType decides which types are written to the database: the checked
// ones. Types which checks only read stay in memory.
func storeType(e *checks.Engine) func(schema.GroupResource) bool {
	return e.Store
}

// warnUnserved logs core-group resources (names without a dot) which checks
// name in match or reads but the cluster does not serve: usually a missing
// group ("deployments" is the core group; use "deployments.apps"). Names
// with a group are not reported: checks for optional CRDs are normal.
func warnUnserved(e *checks.Engine, disc discovery.DiscoveryInterface, log *slog.Logger) {
	lists, err := discovery.ServerPreferredResources(disc)
	if err != nil {
		// Partial discovery would make served types look missing; the
		// watcher reports discovery errors.
		return
	}
	served := map[schema.GroupResource]struct{}{}
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			served[schema.GroupResource{Group: gv.Group, Resource: r.Name}] = struct{}{}
		}
	}
	for _, d := range e.Checks() {
		for _, r := range append(append([]string{}, d.MatchResources...), d.ReadsResources...) {
			gr, err := checks.ParseResource(r)
			if err != nil || gr.Group != "" {
				continue // "*" or a resource with a group
			}
			if _, ok := served[gr]; !ok {
				log.Warn("check names a resource the cluster does not serve", "check", d.Name, "resource", r,
					"hint", "a name without a dot is the core group, e.g. use deployments.apps")
			}
		}
	}
}
