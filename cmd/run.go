package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/guettli/check-conditions/pkg/checkconditions"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/guettli/konmon/internal/alertmanager"
	"github.com/guettli/konmon/internal/monitor"
	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/watch"
)

var runOpts struct {
	kubeContext          string
	clusterName          string
	excludeNamespaces    []string
	excludeResources     []string
	revisions            bool
	noRevisionsFor       []string
	forDuration          time.Duration
	evalInterval         time.Duration
	discoveryInterval    time.Duration
	retention            time.Duration
	listen               string
	alertmanagerURLs     []string
	alertmanagerWatchdog bool
	resendResolved       time.Duration
	ignoreRegexs         []string
	args                 checkconditions.Arguments
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Watch the cluster, record history and send alerts",
	Long: `Watch all resource types of the cluster and store every change in the
SQLite database. Every --eval-interval all objects are checked with the
check-conditions rules. A finding which is open for at least --for fires and
is sent to Alertmanager (if --alertmanager-url is given).

The kubeconfig is found like kubectl does (KUBECONFIG, ~/.kube/config), or
in-cluster config is used when running in a Pod.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return run(cmd.Context())
	},
}

func init() {
	f := runCmd.Flags()
	f.StringVar(&runOpts.kubeContext, "context", "", "Kubeconfig context to use. Default: current context.")
	f.StringVar(&runOpts.clusterName, "cluster-name", "", "Value of the 'cluster' label of alerts. Empty: no label.")
	f.StringSliceVar(&runOpts.excludeNamespaces, "exclude-namespace", nil, "Namespaces to ignore (not stored, not checked). Comma-separated, glob patterns allowed.")
	f.StringSliceVar(&runOpts.excludeResources, "exclude-resource", []string{"leases.coordination.k8s.io", "endpointslices.discovery.k8s.io", "events.events.k8s.io"},
		"Resource types to not watch, as resource.group (core: resource). Comma-separated, glob patterns allowed. Types without conditions like secrets, configmaps and events are never watched.")
	f.BoolVar(&runOpts.revisions, "revisions", true, "Store a compressed copy of every changed object, not only the latest version.")
	f.StringSliceVar(&runOpts.noRevisionsFor, "no-revisions-for", nil, "Resource types (resource.group, globs allowed) to store without revisions.")
	f.DurationVar(&runOpts.forDuration, "for", 2*time.Minute, "How long a finding must be open before it fires.")
	f.DurationVar(&runOpts.evalInterval, "eval-interval", 30*time.Second, "Time between two evaluations of all objects.")
	f.DurationVar(&runOpts.discoveryInterval, "discovery-interval", 5*time.Minute, "Time between two discoveries of resource types (new CRDs).")
	f.DurationVar(&runOpts.retention, "retention", 30*24*time.Hour, "How long to keep history (revisions, condition changes, resolved findings, deleted objects). 0 keeps everything.")
	f.StringVar(&runOpts.listen, "listen", ":8080", "Address for /healthz. Empty disables it.")
	f.StringSliceVar(&runOpts.alertmanagerURLs, "alertmanager-url", nil, "Alertmanager base URL, e.g. http://alertmanager:9093. Can be given multiple times.")
	f.BoolVar(&runOpts.alertmanagerWatchdog, "alertmanager-watchdog", false, "Send an always-firing KonmonWatchdog alert, for a dead man's switch.")
	f.DurationVar(&runOpts.resendResolved, "resend-resolved", 15*time.Minute, "How long resolved alerts are re-sent to Alertmanager.")

	// The check-conditions rules, same flags and defaults as check-conditions.
	f.DurationVar(&runOpts.args.WarnDeletionTimestampOlderThan, "warn-deletion-older-than", 10*time.Minute, "Warn about resources whose deletionTimestamp is older than this duration. Set to 0 to disable.")
	f.DurationVar(&runOpts.args.PodStartGracePeriod, "pod-start-grace", 30*time.Second, "Treat a Pod whose ContainersReady/Initialized condition is False as healthy while it is still starting for the first time and younger than this duration. Set to 0 to disable.")
	f.Int64Var(&runOpts.args.PodRestartWarnCount, "pod-restart-warn-count", 5, "Warn about a Pod container that has restarted at least this many times and is still unhealthy. Set to 0 to disable.")
	f.StringArrayVar(&runOpts.ignoreRegexs, "ignore-condition-regex", nil, "Additional regex to ignore a condition line, on top of the built-in list of check-conditions. Can be given multiple times. Matched against: 'resource type=status reason \"message\"'.")
	rootCmd.AddCommand(runCmd)
}

func globMatcher(patterns []string) (func(schema.GroupResource) bool, error) {
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
	}
	return func(gr schema.GroupResource) bool {
		s := gr.String()
		for _, p := range patterns {
			if ok, _ := path.Match(p, s); ok {
				return true
			}
		}
		return false
	}, nil
}

func run(ctx context.Context) error {
	log, err := newLogger()
	if err != nil {
		return err
	}
	// client-go logs reflector errors with klog; send them to our logger.
	klog.SetSlogLogger(log)

	args := runOpts.args
	for _, s := range runOpts.ignoreRegexs {
		r, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("invalid --ignore-condition-regex %q: %w", s, err)
		}
		args.ExtraConditionLinesToIgnoreRegexs = append(args.ExtraConditionLinesToIgnoreRegexs, r)
	}
	if _, err := globMatcher(runOpts.excludeNamespaces); err != nil {
		return fmt.Errorf("--exclude-namespace: %w", err)
	}
	excludeResource, err := globMatcher(runOpts.excludeResources)
	if err != nil {
		return fmt.Errorf("--exclude-resource: %w", err)
	}
	noRevisions, err := globMatcher(runOpts.noRevisionsFor)
	if err != nil {
		return fmt.Errorf("--no-revisions-for: %w", err)
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: runOpts.kubeContext}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	config.QPS = 50
	config.Burst = 100
	config.UserAgent = "konmon/" + buildVersion()
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	events := make(chan any, 10000)
	watcher := &watch.Watcher{
		Dynamic:   dyn,
		Discovery: disc,
		Include: func(gr schema.GroupResource) bool {
			return !checkconditions.SkipResource(gr) && !excludeResource(gr)
		},
		Events: events,
		Log:    log,
	}

	var notifier monitor.Notifier
	if len(runOpts.alertmanagerURLs) > 0 {
		labels := map[string]string{}
		if runOpts.clusterName != "" {
			labels["cluster"] = runOpts.clusterName
		}
		notifier = &alertmanager.Client{
			URLs:           runOpts.alertmanagerURLs,
			Labels:         labels,
			ResolveTimeout: 4 * runOpts.evalInterval,
			Watchdog:       runOpts.alertmanagerWatchdog,
			HTTP:           &http.Client{Timeout: 15 * time.Second},
		}
	}
	mon := monitor.New(monitor.Config{
		Args:              &args,
		ExcludeNamespaces: runOpts.excludeNamespaces,
		StoreRevisions: func(gr schema.GroupResource) bool {
			return runOpts.revisions && !noRevisions(gr)
		},
		For:            runOpts.forDuration,
		EvalInterval:   runOpts.evalInterval,
		ResendResolved: runOpts.resendResolved,
		Retention:      runOpts.retention,
		Log:            log,
	}, st, watcher, notifier)

	log.Info("starting konmon", "version", buildVersion(), "server", config.Host, "db", dbPath,
		"alertmanagers", strings.Join(runOpts.alertmanagerURLs, ","))

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return watcher.Run(ctx, runOpts.discoveryInterval) })
	g.Go(func() error { return mon.Run(ctx, events) })
	if runOpts.listen != "" {
		srv := healthServer(runOpts.listen, mon, time.Now())
		g.Go(func() error {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("listen %s: %w", runOpts.listen, err)
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		})
	}
	err = g.Wait()
	log.Info("stopped")
	return err
}

// healthServer serves /healthz: OK while evaluations run. Before the first
// evaluation (initial sync of a big cluster) it is OK for 10 minutes.
func healthServer(addr string, mon *monitor.Monitor, started time.Time) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		last := mon.LastEvaluation()
		now := time.Now()
		ok := (last.IsZero() && now.Sub(started) < 10*time.Minute) ||
			(!last.IsZero() && now.Sub(last) < 3*runOpts.evalInterval+time.Minute)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "last evaluation: %s\n", formatTime(last))
			return
		}
		_, _ = fmt.Fprintf(w, "ok, last evaluation: %s\n", formatTime(last))
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}
