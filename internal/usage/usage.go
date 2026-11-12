// Package usage samples the CPU and memory use of containers from the
// metrics API (metrics.k8s.io, served by metrics-server), aggregates it per
// workload, container and time bucket, and turns the aggregate into
// suggested resource requests.
package usage

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// PodMetrics is the resource of the metrics API.
var PodMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// Key is one container of one workload.
type Key struct {
	Namespace string
	Kind      string // Deployment, StatefulSet, DaemonSet, CronJob, Job, ReplicaSet, Pod, or an owner's kind
	Workload  string
	Container string
}

// Sample is the use of one container of one Pod at one time.
type Sample struct {
	Key
	CPU    float64 // cores
	Memory int64   // bytes (working set, what metrics-server reports)
}

// Bucket aggregates the samples of one Key in one time bucket. Every Pod of
// a workload adds its own sample: a request is per Pod.
type Bucket struct {
	Key
	Start   time.Time
	Samples int64
	CPUSum  float64
	CPUMax  float64
	MemMax  int64
}

// Add adds a sample to the bucket.
func (b *Bucket) Add(s Sample) {
	b.Samples++
	b.CPUSum += s.CPU
	b.CPUMax = math.Max(b.CPUMax, s.CPU)
	if s.Memory > b.MemMax {
		b.MemMax = s.Memory
	}
}

// PodLookup returns a Pod by namespace and name, nil if unknown.
type PodLookup func(namespace, name string) *unstructured.Unstructured

// Batch reports whether kind runs now and then (Jobs) rather than all the
// time: its data is judged by the time span it covers, not by hours.
func Batch(kind string) bool { return kind == "CronJob" || kind == "Job" }

// Fetch lists the current PodMetrics and returns one sample per container,
// attributed to the Pod's workload (pods resolves owners).
func Fetch(ctx context.Context, dyn dynamic.Interface, namespace string, pods PodLookup) ([]Sample, error) {
	list, err := dyn.Resource(PodMetrics).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var samples []Sample
	for i := range list.Items {
		pm := &list.Items[i]
		ns, name := pm.GetNamespace(), pm.GetName()
		pod := pods(ns, name)
		if pod == nil {
			// Not known (yet): a per-Pod row would split the workload.
			continue
		}
		kind, workload := Owner(pod, name)
		containers, _, _ := unstructured.NestedSlice(pm.Object, "containers")
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cname, _ := cm["name"].(string)
			u, _ := cm["usage"].(map[string]any)
			cpu, errCPU := quantity(u["cpu"])
			mem, errMem := quantity(u["memory"])
			if cname == "" || errCPU != nil || errMem != nil {
				continue
			}
			samples = append(samples, Sample{
				Key:    Key{Namespace: ns, Kind: kind, Workload: workload, Container: cname},
				CPU:    cpu.AsApproximateFloat64(),
				Memory: mem.Value(),
			})
		}
	}
	return samples, nil
}

func quantity(v any) (resource.Quantity, error) {
	s, ok := v.(string)
	if !ok {
		return resource.Quantity{}, fmt.Errorf("not a quantity: %v", v)
	}
	return resource.ParseQuantity(s)
}

// A Job created by a CronJob is named <cronjob>-<scheduled time in minutes
// since the epoch>.
var cronJobSuffix = regexp.MustCompile(`^(.+)-(\d{8,})$`)

// scheduledMinutes reports whether s is a plausible CronJob schedule time in
// minutes since the epoch (2020 to 2099). A date like 20261112 (YYYYMMDD,
// at most 20991231) stays below that range.
func scheduledMinutes(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n >= 26_297_280 && n < 68_374_080
}

// Owner returns the workload of a Pod: its controller, with a ReplicaSet of a
// Deployment resolved to the Deployment (via the pod-template-hash label), of
// an Argo Rollout to the Rollout (rollouts-pod-template-hash), and a Job of a
// CronJob to the CronJob (via the Job name). A static Pod (owned by its
// Node) is its own workload. Without the Pod object the Pod itself is the
// workload; callers should skip Pods they do not know yet.
func Owner(pod *unstructured.Unstructured, podName string) (kind, name string) {
	if pod == nil {
		return "Pod", podName
	}
	for _, ref := range pod.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "ReplicaSet":
			if hash := pod.GetLabels()["rollouts-pod-template-hash"]; hash != "" {
				if r, ok := strings.CutSuffix(ref.Name, "-"+hash); ok && r != "" {
					return "Rollout", r
				}
			}
			if hash := pod.GetLabels()["pod-template-hash"]; hash != "" {
				if d, ok := strings.CutSuffix(ref.Name, "-"+hash); ok && d != "" {
					return "Deployment", d
				}
			}
		case "Job":
			if m := cronJobSuffix.FindStringSubmatch(ref.Name); m != nil && scheduledMinutes(m[2]) {
				return "CronJob", m[1]
			}
		case "Node":
			return "Pod", pod.GetName()
		}
		return ref.Kind, ref.Name
	}
	return "Pod", pod.GetName()
}

// Rule turns observed use into a suggestion.
type Rule struct {
	// CPUPercentile of the bucket averages, 0-100 (default 95).
	CPUPercentile float64
	// MemoryHeadroom is added to the peak memory as a fraction, e.g. 0.15.
	MemoryHeadroom float64
}

// Stats is what was observed for one Key.
type Stats struct {
	Key
	Buckets       int
	Samples       int64
	First         time.Time // start of the first bucket
	Last          time.Time // start of the last bucket
	CPUPct        float64   // cores, percentile of bucket averages
	CPUMax        float64   // cores, highest single sample
	MemPeak       int64     // bytes, highest single sample
	SuggestCPU    resource.Quantity
	SuggestMemory resource.Quantity
}

// Summarize groups buckets by Key and computes the suggestion of each.
// Results are sorted by namespace, kind, workload, container.
func Summarize(buckets []Bucket, rule Rule) []Stats {
	if rule.CPUPercentile <= 0 || rule.CPUPercentile > 100 {
		rule.CPUPercentile = 95
	}
	rule.MemoryHeadroom = max(rule.MemoryHeadroom, 0)
	byKey := map[Key][]Bucket{}
	for _, b := range buckets {
		if b.Samples > 0 {
			byKey[b.Key] = append(byKey[b.Key], b)
		}
	}
	result := make([]Stats, 0, len(byKey))
	for k, bs := range byKey {
		st := Stats{Key: k, Buckets: len(bs), First: bs[0].Start, Last: bs[0].Start}
		avgs := make([]float64, 0, len(bs))
		for _, b := range bs {
			st.Samples += b.Samples
			st.CPUMax = math.Max(st.CPUMax, b.CPUMax)
			if b.MemMax > st.MemPeak {
				st.MemPeak = b.MemMax
			}
			if b.Start.Before(st.First) {
				st.First = b.Start
			}
			if b.Start.After(st.Last) {
				st.Last = b.Start
			}
			avgs = append(avgs, b.CPUSum/float64(b.Samples))
		}
		st.CPUPct = percentile(avgs, rule.CPUPercentile)
		st.SuggestCPU = CPURequest(st.CPUPct)
		st.SuggestMemory = MemoryRequest(float64(st.MemPeak) * (1 + rule.MemoryHeadroom))
		result = append(result, st)
	}
	slices.SortFunc(result, func(a, b Stats) int {
		return strings.Compare(a.Namespace+"\x00"+a.Kind+"\x00"+a.Workload+"\x00"+a.Container,
			b.Namespace+"\x00"+b.Kind+"\x00"+b.Workload+"\x00"+b.Container)
	})
	return result
}

// percentile is the nearest-rank percentile p (0-100] of values.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := slices.Clone(values)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[max(rank, 1)-1]
}

// CPURequest rounds cores up to a request: multiples of 5m, at least 5m.
func CPURequest(cores float64) resource.Quantity {
	m := int64(math.Ceil(cores*1000/5)) * 5
	return *resource.NewMilliQuantity(max(m, 5), resource.DecimalSI)
}

// MemoryRequest rounds bytes up to a request: whole Mi, at least 4Mi.
func MemoryRequest(bytes float64) resource.Quantity {
	const mi = 1 << 20
	n := int64(math.Ceil(bytes / mi))
	return *resource.NewQuantity(max(n, 4)*mi, resource.BinarySI)
}
