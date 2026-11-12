package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/usage"
)

// collectUsage samples the metrics API every interval and adds the samples
// to hourly usage buckets, for `konmon suggest-requests`. Without the metrics
// API (no metrics-server) it logs once and keeps trying.
func collectUsage(ctx context.Context, st *store.Store, dyn dynamic.Interface, pods usage.PodLookup, interval time.Duration, log *slog.Logger) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			samples, err := usage.Fetch(ctx, dyn, "", pods)
			if err == nil {
				err = st.Update(ctx, func(tx *store.Tx) error {
					return tx.AddUsage(now.UTC().Truncate(time.Hour), samples)
				})
			}
			switch {
			case err != nil && ctx.Err() != nil:
				return nil
			case err != nil && !failing:
				failing = true
				msg := "cannot read container usage; suggest-requests gets no data until this works"
				if apierrors.IsNotFound(err) {
					msg = "metrics API (metrics.k8s.io) not available, install metrics-server; suggest-requests gets no data until then"
				}
				log.Warn(msg, "err", err)
			case err == nil && failing:
				failing = false
				log.Info("reading container usage again", "containers", len(samples))
			}
		}
	}
}

var suggestOpts struct {
	kube          kubeFlags
	namespace     string
	since         string
	minHours      int
	live          time.Duration
	liveInterval  time.Duration
	cpuPercentile float64
	memHeadroom   float64
	output        string
}

var suggestCmd = &cobra.Command{
	Use:   "suggest-requests",
	Short: "Suggest CPU and memory requests from the observed usage",
	Long: `Suggest CPU and memory requests per workload and container from the usage
'konmon run' recorded (sampled from the metrics API, metrics.k8s.io, once a
minute, kept as hourly buckets for --retention). With --live konmon samples
the cluster itself for that long instead, without a database.

The rule: CPU request = the --cpu-percentile (95) of the hourly average CPU
use, rounded up to 5m; memory request = the highest memory use seen plus
--memory-headroom (15%), rounded up to 1Mi. No CPU limit is suggested
(see check container-cpu-limit). Every Pod of a workload counts as one
sample: a request is per Pod.

DATA is the number of hours with samples (minutes with --live). Workloads
with less than --min-hours of it get no suggestion: a day
covers the daily load pattern, a week the weekly one. A workload which does
real work only now and then (a nightly job) needs a window that contains it.

Examples:
  konmon suggest-requests --db /data/konmon.db
  konmon suggest-requests --db /data/konmon.db -n my-app --since 720h -o yaml
  konmon suggest-requests --live 1h -n my-app`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return suggest(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() {
	f := suggestCmd.Flags()
	suggestOpts.kube.register(f)
	f.StringVarP(&suggestOpts.namespace, "namespace", "n", "", "Only this namespace.")
	f.StringVar(&suggestOpts.since, "since", "168h", "Use the usage recorded since then: a duration (168h) or an RFC3339 time.")
	f.IntVar(&suggestOpts.minHours, "min-hours", 24, "Hours of data a workload needs for a suggestion (database mode).")
	f.DurationVar(&suggestOpts.live, "live", 0, "Sample the cluster for this long instead of reading the database, e.g. 30m.")
	f.DurationVar(&suggestOpts.liveInterval, "live-interval", 15*time.Second, "With --live: time between two samples.")
	f.Float64Var(&suggestOpts.cpuPercentile, "cpu-percentile", 95, "Percentile of the average CPU use per hour (per minute with --live).")
	f.Float64Var(&suggestOpts.memHeadroom, "memory-headroom", 0.15, "Added to the peak memory use, as a fraction.")
	f.StringVarP(&suggestOpts.output, "output", "o", "table", "Output format: table, yaml (resources snippets) or json.")
	rootCmd.AddCommand(suggestCmd)
}

// suggestion is one row of the output.
type suggestion struct {
	usage.Stats
	Hours      float64
	Enough     bool
	CurrentCPU string // "" if unknown or unset
	CurrentMem string
}

func suggest(ctx context.Context, w, errW io.Writer) error {
	o := &suggestOpts
	switch o.output {
	case "table", "yaml", "json":
	default:
		return fmt.Errorf("--output: want table, yaml or json, got %q", o.output)
	}
	if o.cpuPercentile <= 0 || o.cpuPercentile > 100 {
		return errors.New("--cpu-percentile must be in (0, 100]")
	}
	if o.memHeadroom < 0 {
		return errors.New("--memory-headroom must not be negative")
	}
	rule := usage.Rule{CPUPercentile: o.cpuPercentile, MemoryHeadroom: o.memHeadroom}

	var buckets []usage.Bucket
	var bucketSize time.Duration
	var current func(kind, namespace, name string) map[string]any
	if o.live > 0 {
		if o.liveInterval <= 0 {
			return errors.New("--live-interval must be positive")
		}
		_, dyn, _, err := o.kube.clients()
		if err != nil {
			return err
		}
		bucketSize = time.Minute
		if buckets, err = sampleLive(ctx, errW, dyn); err != nil {
			return err
		}
		current = func(kind, namespace, name string) map[string]any {
			gvr, ok := workloadResources[kind]
			if !ok {
				return nil
			}
			obj, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil
			}
			return obj.Object
		}
	} else {
		since, err := parseTime(o.since, time.Now())
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		st, err := store.OpenExisting(ctx, dbPath)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		if buckets, err = st.Usage(ctx, since, o.namespace); err != nil {
			return err
		}
		bucketSize = time.Hour
		current = func(kind, namespace, name string) map[string]any {
			raw, err := st.ObjectJSON(ctx, kind, namespace, name)
			if err != nil || raw == nil {
				return nil
			}
			var obj map[string]any
			if json.Unmarshal(raw, &obj) != nil {
				return nil
			}
			return obj
		}
	}

	rows := make([]suggestion, 0)
	for _, s := range usage.Summarize(buckets, rule) {
		hours := float64(s.Buckets) * bucketSize.Hours()
		r := suggestion{Stats: s, Hours: hours, Enough: o.live > 0 || hours >= float64(o.minHours)}
		if obj := current(s.Kind, s.Namespace, s.Workload); obj != nil {
			r.CurrentCPU, r.CurrentMem = currentRequests(obj, s.Container)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		if o.live > 0 {
			return errors.New("no usage samples: is metrics-server installed (kubectl top pods)?")
		}
		return fmt.Errorf("no usage recorded in %s since %s: konmon run records it once a minute when the metrics API (metrics-server) is available", dbPath, o.since)
	}
	switch o.output {
	case "json":
		return writeJSON(w, toJSONSuggestions(rows))
	case "yaml":
		return writeSuggestYAML(w, rows, o.minHours)
	}
	return writeSuggestTable(w, rows, o.minHours)
}

// workloadResources maps the kinds usage.Owner returns to their resource.
var workloadResources = map[string]schema.GroupVersionResource{
	"Deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"DaemonSet":   {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"ReplicaSet":  {Group: "apps", Version: "v1", Resource: "replicasets"},
	"CronJob":     {Group: "batch", Version: "v1", Resource: "cronjobs"},
	"Job":         {Group: "batch", Version: "v1", Resource: "jobs"},
	"Pod":         {Version: "v1", Resource: "pods"},
}

// sampleLive samples the metrics API for --live, into one bucket per minute.
func sampleLive(ctx context.Context, errW io.Writer, dyn dynamic.Interface) ([]usage.Bucket, error) {
	o := &suggestOpts
	pods := map[string]*unstructured.Unstructured{}
	lookup := func(namespace, name string) *unstructured.Unstructured { return pods[namespace+"/"+name] }
	refreshPods := func() error {
		list, err := dyn.Resource(workloadResources["Pod"]).Namespace(o.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		clear(pods)
		for i := range list.Items {
			pods[list.Items[i].GetNamespace()+"/"+list.Items[i].GetName()] = &list.Items[i]
		}
		return nil
	}
	buckets := map[usage.Key]map[time.Time]*usage.Bucket{}
	deadline := time.Now().Add(o.live)
	_, _ = fmt.Fprintf(errW, "Sampling container usage every %s for %s...\n", o.liveInterval, o.live)
	t := time.NewTicker(o.liveInterval)
	defer t.Stop()
	n := 0
	for {
		if err := refreshPods(); err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
		samples, err := usage.Fetch(ctx, dyn, o.namespace, lookup)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, errors.New("metrics API (metrics.k8s.io) not available: install metrics-server")
			}
			return nil, fmt.Errorf("read metrics API: %w", err)
		}
		now := time.Now().UTC().Truncate(time.Minute)
		for _, s := range samples {
			if buckets[s.Key] == nil {
				buckets[s.Key] = map[time.Time]*usage.Bucket{}
			}
			b := buckets[s.Key][now]
			if b == nil {
				b = &usage.Bucket{Key: s.Key, Start: now}
				buckets[s.Key][now] = b
			}
			b.Add(s)
		}
		n++
		if !time.Now().Add(o.liveInterval).Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	_, _ = fmt.Fprintf(errW, "%d samples taken.\n", n)
	var result []usage.Bucket
	for _, byTime := range buckets {
		for _, b := range byTime {
			result = append(result, *b)
		}
	}
	return result, nil
}

// currentRequests returns the CPU and memory request of a container of a
// workload or Pod object ("" if unset).
func currentRequests(obj map[string]any, container string) (cpu, mem string) {
	paths := [][]string{
		{"spec", "template", "spec"},                        // Deployment, StatefulSet, DaemonSet, ReplicaSet, Job
		{"spec", "jobTemplate", "spec", "template", "spec"}, // CronJob
		{"spec"}, // Pod
	}
	for _, p := range paths {
		spec, found, _ := unstructured.NestedMap(obj, p...)
		if !found {
			continue
		}
		for _, field := range []string{"containers", "initContainers"} {
			list, _, _ := unstructured.NestedSlice(spec, field)
			for _, c := range list {
				cm, _ := c.(map[string]any)
				if cm["name"] != container {
					continue
				}
				req, _, _ := unstructured.NestedMap(cm, "resources", "requests")
				return quantityString(req["cpu"]), quantityString(req["memory"])
			}
		}
		return "", ""
	}
	return "", ""
}

func quantityString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func milli(cores float64) string {
	return resource.NewMilliQuantity(int64(cores*1000+0.5), resource.DecimalSI).String()
}

func mebi(bytes int64) string {
	return fmt.Sprintf("%dMi", (bytes+(1<<20)-1)>>20)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// dataText is the amount of data: hours with samples, or minutes with --live.
func dataText(hours float64) string {
	if hours < 1 {
		return fmt.Sprintf("%.0fmin", hours*60)
	}
	return fmt.Sprintf("%.0fh", hours)
}

func writeSuggestTable(w io.Writer, rows []suggestion, minHours int) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAMESPACE\tWORKLOAD\tCONTAINER\tDATA\tCPU REQ\tCPU P\tCPU MAX\t-> CPU\tMEM REQ\tMEM PEAK\t-> MEM")
	short := 0
	for _, r := range rows {
		cpu, mem := r.SuggestCPU.String(), r.SuggestMemory.String()
		if !r.Enough {
			cpu, mem = "(too little data)", ""
			short++
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Namespace, r.Kind, r.Workload, r.Container, dataText(r.Hours),
			orDash(r.CurrentCPU), milli(r.CPUPct), milli(r.CPUMax), cpu,
			orDash(r.CurrentMem), mebi(r.MemPeak), mem)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if short > 0 {
		_, _ = fmt.Fprintf(w, "%d containers have less than %dh of data (--min-hours).\n", short, minHours)
	}
	return nil
}

// writeSuggestYAML prints a resources snippet per container, to paste into
// the manifests.
func writeSuggestYAML(w io.Writer, rows []suggestion, minHours int) error {
	last := ""
	for _, r := range rows {
		id := r.Namespace + "/" + r.Kind + "/" + r.Workload
		if id != last {
			_, _ = fmt.Fprintf(w, "# %s %s/%s\n", r.Namespace, r.Kind, r.Workload)
			last = id
		}
		if !r.Enough {
			_, _ = fmt.Fprintf(w, "# container %s: only %.0fh of data, need %dh\n", r.Container, r.Hours, minHours)
			continue
		}
		_, _ = fmt.Fprintf(w, "- name: %s\n  resources:\n    requests:\n      cpu: %s\n      memory: %s\n",
			r.Container, r.SuggestCPU.String(), r.SuggestMemory.String())
	}
	return nil
}

type jsonSuggestion struct {
	Namespace  string    `json:"namespace"`
	Kind       string    `json:"kind"`
	Workload   string    `json:"workload"`
	Container  string    `json:"container"`
	Hours      float64   `json:"hours"`
	Samples    int64     `json:"samples"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	CurrentCPU string    `json:"currentCpuRequest,omitempty"`
	CurrentMem string    `json:"currentMemoryRequest,omitempty"`
	CPUPct     string    `json:"cpuPercentile"`
	CPUMax     string    `json:"cpuMax"`
	MemPeak    string    `json:"memoryPeak"`
	SuggestCPU string    `json:"suggestedCpuRequest,omitempty"`
	SuggestMem string    `json:"suggestedMemoryRequest,omitempty"`
}

func toJSONSuggestions(rows []suggestion) []jsonSuggestion {
	out := make([]jsonSuggestion, 0, len(rows))
	for _, r := range rows {
		j := jsonSuggestion{
			Namespace: r.Namespace, Kind: r.Kind, Workload: r.Workload, Container: r.Container,
			Hours: r.Hours, Samples: r.Samples, First: r.First, Last: r.Last,
			CurrentCPU: r.CurrentCPU, CurrentMem: r.CurrentMem,
			CPUPct: milli(r.CPUPct), CPUMax: milli(r.CPUMax), MemPeak: mebi(r.MemPeak),
		}
		if r.Enough {
			j.SuggestCPU, j.SuggestMem = r.SuggestCPU.String(), r.SuggestMemory.String()
		}
		out = append(out, j)
	}
	return out
}
