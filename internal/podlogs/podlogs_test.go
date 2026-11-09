package podlogs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/guettli/konmon/internal/checks"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func crashingPod(restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "postgres-0", UID: "uid-1"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 "postgres",
			RestartCount:         restarts,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}}},
	}
}

func finding(pod *corev1.Pod) *checks.Finding {
	return &checks.Finding{
		Check: "pod-container-restarts", Category: checks.CategoryHealth,
		GVR:       schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID), Container: "postgres", Key: "postgres",
	}
}

// logRecorder answers log requests with body and records their options.
type logRecorder struct {
	mu   sync.Mutex
	opts []corev1.PodLogOptions
	body string
	err  error
}

func (r *logRecorder) react(action k8stesting.Action) (bool, runtime.Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	opts, _ := action.(k8stesting.GenericActionImpl).Value.(*corev1.PodLogOptions)
	r.opts = append(r.opts, *opts)
	if r.err != nil {
		return true, nil, r.err
	}
	return true, &runtime.Unknown{Raw: []byte(r.body)}, nil
}

func newFetcher(t *testing.T, rec *logRecorder, objs ...runtime.Object) (*Fetcher, *bytes.Buffer) {
	t.Helper()
	client := fake.NewClientset(objs...)
	client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		return rec.react(action)
	})
	var logBuf bytes.Buffer
	return &Fetcher{
		Client: client.CoreV1(),
		Log:    slog.New(slog.NewTextHandler(&logBuf, nil)),
	}, &logBuf
}

func TestPreviousInstanceAndCache(t *testing.T) {
	pod := crashingPod(1389)
	rec := &logRecorder{body: "LOG: starting\nFATAL: could not open file \"base/1/2\"\n"}
	p, _ := newFetcher(t, rec, pod)

	got, ok := p.Logs(context.Background(), finding(pod), now)
	if !ok {
		t.Fatal("want logs")
	}
	want := "konmon: last 40 lines of container postgres, previous instance (restartCount 1389)\n" + rec.body
	if got != want {
		t.Errorf("logs =\n%q\nwant\n%q", got, want)
	}
	if len(rec.opts) != 1 || !rec.opts[0].Previous || rec.opts[0].Container != "postgres" || *rec.opts[0].TailLines != 40 {
		t.Fatalf("log requests = %+v", rec.opts)
	}

	// Re-sent alert, same restart count: from the cache.
	if again, _ := p.Logs(context.Background(), finding(pod), now.Add(30*time.Second)); again != got {
		t.Errorf("cached logs = %q", again)
	}
	if len(rec.opts) != 1 {
		t.Fatalf("fetched again for the same restart count: %d requests", len(rec.opts))
	}

	// It restarted: fetch again.
	pod.Status.ContainerStatuses[0].RestartCount++
	if _, err := p.Client.Pods("db").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ = p.Logs(context.Background(), finding(pod), now.Add(time.Minute))
	if len(rec.opts) != 2 || !strings.Contains(got, "restartCount 1390") {
		t.Fatalf("after a restart: %d requests, logs %q", len(rec.opts), got)
	}
}

func TestCurrentInstanceWithoutPrevious(t *testing.T) {
	pod := crashingPod(0)
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	rec := &logRecorder{body: "not ready yet\n"}
	p, _ := newFetcher(t, rec, pod)
	got, ok := p.Logs(context.Background(), finding(pod), now)
	if !ok || !strings.Contains(got, "current instance") || !strings.HasSuffix(got, "not ready yet\n") {
		t.Fatalf("logs = %q, %v", got, ok)
	}
	if rec.opts[0].Previous {
		t.Error("asked for the previous instance")
	}
}

func TestNeverStartedHasNoLogs(t *testing.T) {
	pod := crashingPod(0)
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	rec := &logRecorder{}
	p, _ := newFetcher(t, rec, pod)
	if got, ok := p.Logs(context.Background(), finding(pod), now); ok {
		t.Fatalf("want no annotation, got %q", got)
	}
	if len(rec.opts) != 0 {
		t.Errorf("fetched logs of a container which never ran")
	}
}

func TestNotAPodFinding(t *testing.T) {
	pod := crashingPod(9)
	rec := &logRecorder{}
	p, _ := newFetcher(t, rec, pod)
	f := finding(pod)
	f.GVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	if _, ok := p.Logs(context.Background(), f, now); ok {
		t.Error("logs for a deployment")
	}
}

// failedJobPod: a Job Pod with an init container and two containers; "work" exited 22, "helper" exited 0.
func failedJobPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "korphan-guard-1-abc", UID: "uid-job"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init"}},
			Containers:     []corev1.Container{{Name: "helper"}, {Name: "work"}},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "helper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "work", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 22}}},
			},
		},
	}
}

func podFinding(pod *corev1.Pod) *checks.Finding {
	return &checks.Finding{
		Check: "conditions", Category: checks.CategoryHealth,
		GVR:       schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID), Key: "ContainersReady",
	}
}

func TestPodFindingGetsLogsOfFailedContainer(t *testing.T) {
	pod := failedJobPod()
	rec := &logRecorder{body: "curl: (22) The requested URL returned error: 504\n"}
	p, _ := newFetcher(t, rec, pod)
	got, ok := p.Logs(context.Background(), podFinding(pod), now)
	if !ok {
		t.Fatal("want logs")
	}
	want := "konmon: last 40 lines of container work, current instance (restartCount 0)\n" + rec.body
	if got != want {
		t.Errorf("logs =\n%q\nwant\n%q", got, want)
	}
	if len(rec.opts) != 1 || rec.opts[0].Container != "work" || rec.opts[0].Previous {
		t.Fatalf("log requests = %+v", rec.opts)
	}
}

func TestPodFindingPrefersFailedInitContainer(t *testing.T) {
	pod := failedJobPod()
	pod.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
	pod.Status.ContainerStatuses = nil
	rec := &logRecorder{body: "init broke\n"}
	p, _ := newFetcher(t, rec, pod)
	if got, ok := p.Logs(context.Background(), podFinding(pod), now); !ok || !strings.Contains(got, "container init,") {
		t.Fatalf("logs = %q, %v", got, ok)
	}
}

func TestPodFindingSingleContainer(t *testing.T) {
	pod := crashingPod(0)
	pod.Spec.Containers = []corev1.Container{{Name: "postgres"}}
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	rec := &logRecorder{body: "not ready yet\n"}
	p, _ := newFetcher(t, rec, pod)
	if got, ok := p.Logs(context.Background(), podFinding(pod), now); !ok || !strings.Contains(got, "container postgres,") {
		t.Fatalf("logs = %q, %v", got, ok)
	}
}

func TestPodFindingNoClearContainer(t *testing.T) {
	pod := failedJobPod()
	pod.Status.ContainerStatuses[1].State.Terminated.ExitCode = 0 // nothing failed, two containers
	rec := &logRecorder{}
	p, _ := newFetcher(t, rec, pod)
	if got, ok := p.Logs(context.Background(), podFinding(pod), now); ok {
		t.Fatalf("want no annotation, got %q", got)
	}
	if len(rec.opts) != 0 {
		t.Error("fetched logs without a clear container")
	}
}

func TestPodGoneOrRecreated(t *testing.T) {
	pod := crashingPod(9)
	rec := &logRecorder{}
	p, _ := newFetcher(t, rec) // not in the API
	if _, ok := p.Logs(context.Background(), finding(pod), now); ok {
		t.Error("logs of a Pod which does not exist")
	}
	p, _ = newFetcher(t, rec, pod)
	f := finding(pod)
	f.UID = "old-uid"
	if _, ok := p.Logs(context.Background(), f, now); ok {
		t.Error("logs of a recreated Pod for the finding of the old one")
	}
}

func TestFetchErrorIsVisibleAndRetried(t *testing.T) {
	pod := crashingPod(7)
	rec := &logRecorder{err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "postgres-0", nil)}
	p, logBuf := newFetcher(t, rec, pod)
	got, ok := p.Logs(context.Background(), finding(pod), now)
	if !ok || !strings.HasPrefix(got, "konmon: could not fetch logs: ") || !strings.Contains(got, "forbidden") {
		t.Fatalf("logs = %q, %v", got, ok)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "forbidden") {
		t.Errorf("error not logged: %s", logBuf.String())
	}
	// Not on every evaluation...
	_, _ = p.Logs(context.Background(), finding(pod), now.Add(time.Minute))
	if len(rec.opts) != 1 {
		t.Fatalf("%d requests within ErrorRetry", len(rec.opts))
	}
	// ...but after ErrorRetry.
	rec.err = nil
	rec.body = "works now\n"
	got, _ = p.Logs(context.Background(), finding(pod), now.Add(DefaultErrorRetry+time.Second))
	if len(rec.opts) != 2 || !strings.HasSuffix(got, "works now\n") {
		t.Fatalf("%d requests, logs %q", len(rec.opts), got)
	}
}

func TestCachedPodIsUsed(t *testing.T) {
	pod := crashingPod(5)
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		t.Fatal(err)
	}
	rec := &logRecorder{body: "x\n"}
	p, _ := newFetcher(t, rec) // the API does not know the Pod: only the cache
	p.Cached = func(namespace, name string) (*unstructured.Unstructured, bool) {
		if namespace != "db" || name != "postgres-0" {
			return nil, true
		}
		return &unstructured.Unstructured{Object: obj}, true
	}
	if _, ok := p.Logs(context.Background(), finding(pod), now); !ok {
		t.Fatal("cached Pod not used")
	}
	// The cache says the Pod is gone: no API call needed.
	other := finding(pod)
	other.Name = "postgres-1"
	if _, ok := p.Logs(context.Background(), other, now); ok {
		t.Error("logs of a Pod the cache does not have")
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short\n", 100, false); got != "short\n" {
		t.Errorf("short = %q", got)
	}
	long := strings.Repeat("0123456789\n", 50) // 550 bytes
	got := Truncate(long, 100, false)
	if !strings.HasPrefix(got, TruncatedMarker+"\n0123456789\n") || len(got) > 100+len(TruncatedMarker)+1 {
		t.Errorf("truncated = %q", got)
	}
	if !strings.HasSuffix(got, "0123456789\n") {
		t.Errorf("end lost: %q", got)
	}
	// Cut while reading.
	if got := Truncate("end\n", 100, true); got != TruncatedMarker+"\nend\n" {
		t.Errorf("cut = %q", got)
	}
	// No newline in the kept part: cut at a rune start.
	got = Truncate(strings.Repeat("ä", 100), 51, false)
	if !strings.HasPrefix(got, TruncatedMarker+"\nä") || strings.ContainsRune(got, '�') {
		t.Errorf("runes = %q", got)
	}
}

func TestLongLogIsCutWhileReading(t *testing.T) {
	pod := crashingPod(5)
	rec := &logRecorder{body: strings.Repeat("x", 100000) + "\nthe end\n"}
	p, _ := newFetcher(t, rec, pod)
	p.MaxBytes = 1000
	got, _ := p.Logs(context.Background(), finding(pod), now)
	if !strings.Contains(got, TruncatedMarker) || !strings.HasSuffix(got, "\nthe end\n") || len(got) > 1200 {
		t.Errorf("len %d, logs %q...", len(got), got[:min(len(got), 200)])
	}
}
