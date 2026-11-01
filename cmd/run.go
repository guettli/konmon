package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path"
	"slices"
	"strconv"
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
	f.StringVar(&runOpts.listen, "listen", ":8080", "Address for /healthz (liveness), /readyz (readiness, startup) and /metrics. Empty disables it.")
	f.StringSliceVar(&runOpts.alertmanagerURLs, "alertmanager-url", nil, "Alertmanager base URL, e.g. http://alertmanager:9093. Can be given multiple times.")
	f.BoolVar(&runOpts.alertmanagerWatchdog, "alertmanager-watchdog", false, "Send an always-firing KonmonWatchdog alert, for a dead man's switch.")
	f.DurationVar(&runOpts.resendResolved, "resend-resolved", 15*time.Minute, "How long resolved alerts are re-sent to Alertmanager.")

	rootCmd.AddCommand(runCmd)
}

func globMatcher(patterns []string) (func(schema.GroupResource) bool, error) {
	patterns = slices.Clone(patterns)
	for i, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		patterns[i] = strings.ToLower(p) // resource names are lowercase
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

func (k *kubeFlags) loader() (clientcmd.ClientConfig, *clientcmd.ClientConfigLoadingRules) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = k.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: k.context}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides), loadingRules
}

// describe says which cluster the flags select, and where the kubeconfig
// comes from: "https://10.0.0.1:6443 (context \"prod\")" and
// "/home/me/.kube/config".
func (k *kubeFlags) describe() (cluster, kubeconfig string) {
	cc, loadingRules := k.loader()
	config, err := cc.ClientConfig()
	if err != nil {
		return "unknown", "unknown"
	}
	cluster = config.Host
	raw, err := cc.RawConfig()
	if err != nil || len(raw.Contexts) == 0 {
		return cluster, "in-cluster config"
	}
	name := k.context
	if name == "" {
		name = raw.CurrentContext
	}
	cluster += fmt.Sprintf(" (context %q)", name)
	if loadingRules.ExplicitPath != "" {
		return cluster, loadingRules.ExplicitPath
	}
	kubeconfig = strings.Join(loadingRules.GetLoadingPrecedence(), string(os.PathListSeparator))
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "" {
		kubeconfig += " (from $" + clientcmd.RecommendedConfigPathEnvVar + ")"
	}
	return cluster, kubeconfig
}

// clients builds the Kubernetes clients.
func (k *kubeFlags) clients() (*rest.Config, dynamic.Interface, discovery.DiscoveryInterface, error) {
	cc, _ := k.loader()
	config, err := cc.ClientConfig()
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
	if runOpts.evalInterval <= 0 {
		return errors.New("--eval-interval must be positive")
	}
	if runOpts.discoveryInterval <= 0 {
		return errors.New("--discovery-interval must be positive")
	}
	if err := notNegative(
		flagValue{"--for", runOpts.forDuration},
		flagValue{"--history-window", runOpts.historyWindow},
		flagValue{"--retention", runOpts.retention},
		flagValue{"--resend-resolved", runOpts.resendResolved},
	); err != nil {
		return err
	}
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
		srv := healthServer(runOpts.listen, mon, watcher, time.Now(), runOpts.evalInterval)
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
//   - /metrics: Prometheus metrics, to alert from outside when konmon does
//     not evaluate (konmon cannot report that about itself).
func healthServer(addr string, ev evaluations, stale staleTypes, started time.Time, evalInterval time.Duration) *http.Server {
	lastEvaluation := ev.LastEvaluation
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, ev, stale, started, evalInterval)
	})
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

// evaluations is what the health server needs of the monitor.
type evaluations interface {
	LastEvaluation() time.Time
	Evaluations() uint64
}

// staleTypes is what the health server needs of the watcher.
type staleTypes interface {
	Stale() []watch.StaleType
}

// writeMetrics writes the metrics in the Prometheus text format. A handful
// of gauges and one counter: no need for a client library.
func writeMetrics(w io.Writer, ev evaluations, stale staleTypes, started time.Time, evalInterval time.Duration) {
	var lastSeconds float64
	if last := ev.LastEvaluation(); !last.IsZero() {
		lastSeconds = float64(last.UnixMilli()) / 1000
	}
	metrics := []struct {
		name, typ, help, labels string
		value                   float64
	}{
		{"konmon_build_info", "gauge", "Always 1; the version as label.", `{version="` + labelValue(buildVersion()) + `"}`, 1},
		{"konmon_start_time_seconds", "gauge", "Start time of the process, Unix seconds.", "", float64(started.Unix())},
		{"konmon_evaluation_interval_seconds", "gauge", "Time between two evaluations (--eval-interval).", "", evalInterval.Seconds()},
		{"konmon_evaluations_total", "counter", "Successful evaluations since the start.", "", float64(ev.Evaluations())},
		{"konmon_last_evaluation_timestamp_seconds", "gauge", "Time of the last successful evaluation, Unix seconds; 0 before the first.", "", lastSeconds},
	}
	for _, m := range metrics {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s%s %s\n",
			m.name, m.help, m.name, m.typ, m.name, m.labels, strconv.FormatFloat(m.value, 'f', -1, 64))
	}
	// One series per stale type, so an alert can name it.
	_, _ = fmt.Fprint(w, "# HELP konmon_stale_resource_type_since_timestamp_seconds Watched resource types whose list and watch calls fail: "+
		"their objects are out of date. The value is the time of the first failed call, Unix seconds.\n"+
		"# TYPE konmon_stale_resource_type_since_timestamp_seconds gauge\n")
	if stale == nil {
		return
	}
	types := stale.Stale()
	slices.SortFunc(types, func(a, b watch.StaleType) int { return strings.Compare(a.String(), b.String()) })
	for _, t := range types {
		_, _ = fmt.Fprintf(w, "konmon_stale_resource_type_since_timestamp_seconds{resource=\"%s\"} %d\n", labelValue(t.String()), t.Since.Unix())
	}
}

// labelValue escapes a Prometheus label value: only \\, " and newline.
var labelValue = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace

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
