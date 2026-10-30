package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/guettli/konmon/internal/alertmanager"
	"github.com/guettli/konmon/internal/monitor"
	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/watch"
)

var runOpts struct {
	kube                 kubeFlags
	clusterName          string
	checks               checkFlags
	historyWindow        time.Duration
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
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Watch the cluster, record history and send alerts",
	Long: `Watch all resource types of the cluster and store every change in the
SQLite database. Every --eval-interval all checks run on all objects. A
health finding which is open for at least --for fires and is sent to
Alertmanager (if --alertmanager-url is given).

The kubeconfig is found like kubectl does (KUBECONFIG, ~/.kube/config), or
in-cluster config is used when running in a Pod.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return run(cmd.Context())
	},
}

func init() {
	f := runCmd.Flags()
	runOpts.kube.register(f)
	f.StringVar(&runOpts.clusterName, "cluster-name", "", "Value of the 'cluster' label of alerts. Empty: no label.")
	runOpts.checks.register(f)
	f.DurationVar(&runOpts.historyWindow, "history-window", 24*time.Hour, "How much condition history checks which use ctx.history can see.")
	f.BoolVar(&runOpts.revisions, "revisions", true, "Store a compressed copy of every changed object, not only the latest version.")
	f.StringSliceVar(&runOpts.noRevisionsFor, "no-revisions-for", nil, "Resource types (resource.group, globs allowed) to store without revisions.")
	f.DurationVar(&runOpts.forDuration, "for", 2*time.Minute, "How long a finding must be open before it fires.")
	f.DurationVar(&runOpts.evalInterval, "eval-interval", 30*time.Second, "Time between two evaluations of all objects.")
	f.DurationVar(&runOpts.discoveryInterval, "discovery-interval", 5*time.Minute, "Time between two discoveries of resource types (new CRDs).")
	f.DurationVar(&runOpts.retention, "retention", 30*24*time.Hour, "How long to keep history (revisions, condition changes, resolved findings, deleted objects). 0 keeps everything.")
	f.StringVar(&runOpts.listen, "listen", ":8080", "Address for /healthz (liveness) and /readyz (readiness, startup). Empty disables it.")
	f.StringSliceVar(&runOpts.alertmanagerURLs, "alertmanager-url", nil, "Alertmanager base URL, e.g. http://alertmanager:9093. Can be given multiple times.")
	f.BoolVar(&runOpts.alertmanagerWatchdog, "alertmanager-watchdog", false, "Send an always-firing KonmonWatchdog alert, for a dead man's switch.")
	f.DurationVar(&runOpts.resendResolved, "resend-resolved", 15*time.Minute, "How long resolved alerts are re-sent to Alertmanager.")

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

// kubeFlags select the cluster, like kubectl.
type kubeFlags struct {
	kubeconfig string
	context    string
}

func (k *kubeFlags) register(f *pflag.FlagSet) {
	f.StringVar(&k.kubeconfig, "kubeconfig", "", "Path of the kubeconfig file. Default: $KUBECONFIG or ~/.kube/config, or the in-cluster config.")
	f.StringVar(&k.context, "context", "", "Kubeconfig context to use. Default: current context.")
}

// clients builds the Kubernetes clients.
func (k *kubeFlags) clients() (*rest.Config, dynamic.Interface, discovery.DiscoveryInterface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = k.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: k.context}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("kubeconfig: %w", err)
	}
	config.QPS = 50
	config.Burst = 100
	config.UserAgent = "konmon/" + buildVersion()
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, nil, nil, err
	}
	return config, dyn, disc, nil
}

func run(ctx context.Context) error {
	log, err := newLogger()
	if err != nil {
		return err
	}
	// client-go logs reflector errors with klog; send them to our logger.
	klog.SetSlogLogger(log)

	engine, _, err := runOpts.checks.engine(log, nil, nil)
	if err != nil {
		return err
	}
	ignore, err := runOpts.checks.ignorer()
	if err != nil {
		return err
	}
	ignore.warnExpired(log, time.Now())
	noRevisions, err := globMatcher(runOpts.noRevisionsFor)
	if err != nil {
		return fmt.Errorf("--no-revisions-for: %w", err)
	}

	config, dyn, disc, err := runOpts.kube.clients()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		// A rule may expire while konmon runs.
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				ignore.warnExpired(log, now)
			}
		}
	}()

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	events := make(chan any, 10000)
	watcher := &watch.Watcher{
		Dynamic:   dyn,
		Discovery: disc,
		Include:   engine.Watch,
		Events:    events,
		Log:       log,
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
		Checks:            engine,
		Ignore:            ignore.ignored,
		ExcludeNamespaces: runOpts.checks.excludeNamespaces,
		StoreType:         storeType(engine),
		HistoryWindow:     runOpts.historyWindow,
		StoreRevisions: func(gr schema.GroupResource) bool {
			return runOpts.revisions && !noRevisions(gr)
		},
		For:            runOpts.forDuration,
		EvalInterval:   runOpts.evalInterval,
		ResendResolved: runOpts.resendResolved,
		Retention:      runOpts.retention,
		Log:            log,
	}, st, watcher, notifier)

	warnUnserved(engine, disc, log)
	names := make([]string, 0, len(engine.Checks()))
	for _, d := range engine.Checks() {
		names = append(names, d.Name)
	}
	log.Info("starting konmon", "version", buildVersion(), "server", config.Host, "db", dbPath,
		"alertmanagers", strings.Join(runOpts.alertmanagerURLs, ","), "checks", strings.Join(names, ","))

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return watcher.Run(ctx, runOpts.discoveryInterval) })
	g.Go(func() error { return mon.Run(ctx, events) })
	if runOpts.listen != "" {
		srv := healthServer(runOpts.listen, mon.LastEvaluation, time.Now(), runOpts.evalInterval)
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

// startupGrace is how long /healthz is OK before the first evaluation.
const startupGrace = 10 * time.Minute

// healthServer serves the probes:
//
//   - /readyz: OK while the last evaluation is recent; not OK before the
//     first one, so the Pod is Ready only once konmon really monitors. For
//     the readiness and the startup probe: the startup probe's
//     failureThreshold × periodSeconds is how long the first evaluation (the
//     initial sync) may take.
//   - /healthz: like /readyz, but OK for startupGrace before the first
//     evaluation. For the liveness probe; the grace keeps a Deployment
//     without a startup probe from killing a slow initial sync.
func healthServer(addr string, lastEvaluation func() time.Time, started time.Time, evalInterval time.Duration) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		last := lastEvaluation()
		probeResponse(w, evaluatedRecently(last, time.Now(), evalInterval), last)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		last := lastEvaluation()
		now := time.Now()
		ok := evaluatedRecently(last, now, evalInterval) ||
			(last.IsZero() && now.Sub(started) < startupGrace)
		probeResponse(w, ok, last)
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// evaluatedRecently: an evaluation finished, and the last one is less than
// three intervals (plus a minute for a slow one) ago.
func evaluatedRecently(last, now time.Time, evalInterval time.Duration) bool {
	return !last.IsZero() && now.Sub(last) < 3*evalInterval+time.Minute
}

func probeResponse(w http.ResponseWriter, ok bool, last time.Time) {
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "last evaluation: %s\n", formatTime(last))
		return
	}
	_, _ = fmt.Fprintf(w, "ok, last evaluation: %s\n", formatTime(last))
}
