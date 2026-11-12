package usage

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func pod(name string, labels map[string]any, owner map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": "ns"}
	if labels != nil {
		meta["labels"] = labels
	}
	if owner != nil {
		meta["ownerReferences"] = []any{owner}
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta}}
}

func ref(kind, name string, controller bool) map[string]any {
	return map[string]any{"apiVersion": "apps/v1", "kind": kind, "name": name, "uid": "u", "controller": controller}
}

func TestOwner(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pod        *unstructured.Unstructured
		kind, want string
	}{
		{"deployment via pod-template-hash", pod("web-6d8f-x", map[string]any{"pod-template-hash": "6d8f"}, ref("ReplicaSet", "web-6d8f", true)), "Deployment", "web"},
		{"replicaset without the hash label", pod("rs-x", nil, ref("ReplicaSet", "rs", true)), "ReplicaSet", "rs"},
		{"statefulset", pod("db-0", nil, ref("StatefulSet", "db", true)), "StatefulSet", "db"},
		{"job of a cronjob", pod("backup-29340000-x", nil, ref("Job", "backup-29340000", true)), "CronJob", "backup"},
		{"date-stamped job is no cronjob", pod("migrate-20261112-x", nil, ref("Job", "migrate-20261112", true)), "Job", "migrate-20261112"},
		{"argo rollout", pod("api-7c9-x", map[string]any{"rollouts-pod-template-hash": "7c9"}, ref("ReplicaSet", "api-7c9", true)), "Rollout", "api"},
		{"static pod", pod("etcd-node1", nil, map[string]any{"apiVersion": "v1", "kind": "Node", "name": "node1", "uid": "u", "controller": true}), "Pod", "etcd-node1"},
		{"standalone job", pod("migrate-x", nil, ref("Job", "migrate", true)), "Job", "migrate"},
		{"custom owner", pod("pg-1", nil, ref("Cluster", "pg", true)), "Cluster", "pg"},
		{"non-controller owner is ignored", pod("p", nil, ref("ConfigMap", "c", false)), "Pod", "p"},
		{"unknown pod", nil, "Pod", "gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, name := Owner(tc.pod, "gone")
			if kind != tc.kind || name != tc.want {
				t.Errorf("got %s/%s, want %s/%s", kind, name, tc.kind, tc.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	k := Key{Namespace: "ns", Kind: "Deployment", Workload: "web", Container: "app"}
	var buckets []Bucket
	// 20 hourly buckets with average CPU 0.01 .. 0.20 cores; one sample of
	// 1.5 cores and the memory peak in the last.
	for i := 1; i <= 20; i++ {
		b := Bucket{Key: k, Start: time.Unix(int64(i)*3600, 0)}
		b.Add(Sample{Key: k, CPU: float64(i) / 100, Memory: 100 << 20})
		buckets = append(buckets, b)
	}
	buckets[19].Add(Sample{Key: k, CPU: 1.5, Memory: 200 << 20})
	buckets[19].Add(Sample{Key: k, CPU: 0.1, Memory: 50 << 20})
	other := Key{Namespace: "a", Kind: "Pod", Workload: "p", Container: "c"}
	buckets = append(buckets, Bucket{Key: other, Start: time.Unix(0, 0), Samples: 1, CPUSum: 0.0001, CPUMax: 0.0001, MemMax: 1})
	buckets = append(buckets, Bucket{Key: other, Start: time.Unix(3600, 0)}) // empty, ignored

	got := Summarize(buckets, Rule{CPUPercentile: 95, MemoryHeadroom: 0.15})
	if len(got) != 2 || got[0].Namespace != "a" {
		t.Fatalf("got %+v", got)
	}
	tiny := got[0]
	if tiny.Buckets != 1 || tiny.SuggestCPU.String() != "5m" || tiny.SuggestMemory.String() != "4Mi" {
		t.Errorf("minimums: %+v cpu=%s mem=%s", tiny, tiny.SuggestCPU.String(), tiny.SuggestMemory.String())
	}
	web := got[1]
	// Bucket 20 averages (0.2+1.5+0.1)/3 = 0.6 cores: the 95th percentile of
	// 20 values is the 19th, 0.19.
	if web.Buckets != 20 || web.Samples != 22 {
		t.Errorf("counts: %+v", web)
	}
	if web.CPUPct < 0.1899 || web.CPUPct > 0.1901 || web.CPUMax != 1.5 {
		t.Errorf("cpu: pct=%v max=%v", web.CPUPct, web.CPUMax)
	}
	if web.SuggestCPU.String() != "190m" {
		t.Errorf("cpu request %s, want 190m", web.SuggestCPU.String())
	}
	// 200Mi + 15% = 230Mi.
	if web.MemPeak != 200<<20 || web.SuggestMemory.String() != "230Mi" {
		t.Errorf("memory: peak=%d request=%s", web.MemPeak, web.SuggestMemory.String())
	}
	if !web.First.Equal(time.Unix(3600, 0)) || !web.Last.Equal(time.Unix(20*3600, 0)) {
		t.Errorf("first/last: %v %v", web.First, web.Last)
	}
}

func TestRounding(t *testing.T) {
	for cores, want := range map[float64]string{0: "5m", 0.0051: "10m", 0.1: "100m", 0.1001: "105m", 2.3: "2300m"} {
		if got := CPURequest(cores); got.String() != want {
			t.Errorf("CPURequest(%v) = %s, want %s", cores, got.String(), want)
		}
	}
	for bytes, want := range map[float64]string{0: "4Mi", 5 << 20: "5Mi", 5<<20 + 1: "6Mi", 3 << 30: "3Gi"} {
		if got := MemoryRequest(bytes); got.String() != want {
			t.Errorf("MemoryRequest(%v) = %s, want %s", bytes, got.String(), want)
		}
	}
}

func TestFetch(t *testing.T) {
	pm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata": map[string]any{"name": "web-6d8f-x", "namespace": "ns"},
		"containers": []any{
			map[string]any{"name": "app", "usage": map[string]any{"cpu": "123456789n", "memory": "10Mi"}},
			map[string]any{"name": "broken", "usage": map[string]any{"cpu": "x", "memory": "1Mi"}},
		},
	}}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{PodMetrics: "PodMetricsList"})
	// Created via the resource: the fake would file a constructor object under
	// a resource guessed from its kind, not "pods".
	if _, err := dyn.Resource(PodMetrics).Namespace("ns").Create(context.Background(), pm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	unknown := pm.DeepCopy()
	unknown.SetName("not-cached-yet")
	if _, err := dyn.Resource(PodMetrics).Namespace("ns").Create(context.Background(), unknown, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	lookup := func(_, name string) *unstructured.Unstructured {
		if name == "not-cached-yet" {
			return nil
		}
		return pod(name, map[string]any{"pod-template-hash": "6d8f"}, ref("ReplicaSet", "web-6d8f", true))
	}
	samples, err := Fetch(context.Background(), dyn, "", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("got %+v", samples)
	}
	s := samples[0]
	if s.Kind != "Deployment" || s.Workload != "web" || s.Container != "app" || s.Memory != 10<<20 || s.CPU < 0.1234 || s.CPU > 0.1235 {
		t.Errorf("sample %+v", s)
	}
}
