package watch

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var (
	jobsGVR     = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	cronJobsGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
)

func newJob(name, uid string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("batch/v1")
	u.SetKind("Job")
	u.SetNamespace("ns")
	u.SetName(name)
	u.SetUID(types.UID(uid))
	u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
	return u
}

func next(t *testing.T, events <-chan any) any {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for event")
		return nil
	}
}

func TestWatcherSendsUpsertSyncedAndDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{jobsGVR: "JobList"}, newJob("a", "uid-a"))
	disc := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{{
		GroupVersion: "batch/v1",
		APIResources: []metav1.APIResource{
			{Name: "jobs", Namespaced: true, Kind: "Job", Verbs: []string{"get", "list", "watch"}},
			{Name: "jobs/status", Namespaced: true, Kind: "Job", Verbs: []string{"get", "list", "watch"}},
			{Name: "cronjobs", Namespaced: true, Kind: "CronJob", Verbs: []string{"get", "list", "watch"}},
		},
	}}
	events := make(chan any, 100)
	w := &Watcher{
		Dynamic:   dyn,
		Discovery: disc,
		Include:   func(gr schema.GroupResource) bool { return gr.Resource != "cronjobs" },
		Events:    events,
		Log:       slog.New(slog.DiscardHandler),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx, time.Hour) }()

	up, ok := next(t, events).(Upsert)
	if !ok || up.Obj.GetName() != "a" || up.GVR != jobsGVR {
		t.Fatalf("want Upsert of a, got %#v", up)
	}
	if len(up.Obj.GetManagedFields()) != 0 {
		t.Error("managedFields should be stripped")
	}
	synced, ok := next(t, events).(Synced)
	if !ok {
		t.Fatalf("want Synced, got %#v", synced)
	}
	if _, found := synced.UIDs["uid-a"]; !found || len(synced.UIDs) != 1 {
		t.Errorf("Synced UIDs = %v", synced.UIDs)
	}
	// Synced also waits for the end of the discovery run.
	for deadline := time.Now().Add(5 * time.Second); !w.Synced() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.Synced() || len(w.Snapshot()) != 1 {
		t.Errorf("watcher should be synced with one object, got %d", len(w.Snapshot()))
	}

	if _, err := dyn.Resource(jobsGVR).Namespace("ns").Create(ctx, newJob("b", "uid-b"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if up, ok := next(t, events).(Upsert); !ok || up.Obj.GetName() != "b" {
		t.Fatalf("want Upsert of b, got %#v", up)
	}
	if err := dyn.Resource(jobsGVR).Namespace("ns").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if del, ok := next(t, events).(Delete); !ok || del.UID != "uid-a" {
		t.Fatalf("want Delete of uid-a, got %#v", del)
	}
}

func TestSyncedWaitsForTheEndOfDiscovery(t *testing.T) {
	synced := make(chan struct{})
	close(synced)
	w := &Watcher{informers: map[schema.GroupResource]*running{
		jobsGVR.GroupResource(): {gvr: jobsGVR, synced: synced},
	}}
	if w.Synced() {
		t.Error("Synced while discovery has not gone through all resource types")
	}
	w.discovered = true
	if !w.Synced() {
		t.Error("not Synced after discovery with all informers synced")
	}
}

func TestSyncedGivesUpOnAnInformerWhichDoesNotSync(t *testing.T) {
	done := make(chan struct{})
	close(done)
	widgets := schema.GroupResource{Group: "example.com", Resource: "widgets"}
	stuck := &running{gvr: widgets.WithVersion("v1"), synced: make(chan struct{}), started: time.Now()}
	w := &Watcher{
		Log: slog.New(slog.DiscardHandler), SyncTimeout: time.Minute, discovered: true,
		informers: map[schema.GroupResource]*running{jobsGVR.GroupResource(): {gvr: jobsGVR, synced: done}, widgets: stuck},
	}
	if w.Synced() {
		t.Error("Synced while an informer lists for the first time")
	}
	stuck.started = time.Now().Add(-2 * time.Minute)
	if !w.Synced() {
		t.Error("one broken resource type blocks Synced forever")
	}
	if p := w.Pending(); len(p) != 1 || p[0].GroupResource != widgets || !p[0].Stuck {
		t.Errorf("Pending() = %v", p)
	}
	// A type started later does not block evaluations again.
	w.informers[widgets] = &running{gvr: widgets.WithVersion("v2"), synced: make(chan struct{}), started: time.Now()}
	if !w.Synced() {
		t.Error("a new informer blocks Synced after the first sync")
	}
}

func TestListOnceListsPerNamespaceWhenAllNamespacesIsForbidden(t *testing.T) {
	other := newJob("x", "uid-x")
	other.SetNamespace("other")
	scheme := runtime.NewScheme()
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{jobsGVR: "JobList", cronJobsGVR: "CronJobList"}, newJob("a", "uid-a"), other)
	// RBAC of a user of namespace ns: jobs may be listed there only,
	// cronjobs not at all.
	dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Resource == "jobs" && a.GetNamespace() == "ns" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC"))
	})
	disc := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{{
		GroupVersion: "batch/v1",
		APIResources: []metav1.APIResource{
			{Name: "jobs", Namespaced: true, Kind: "Job", Verbs: []string{"get", "list", "watch"}},
			{Name: "cronjobs", Namespaced: true, Kind: "CronJob", Verbs: []string{"get", "list", "watch"}},
		},
	}}
	w := &Watcher{Dynamic: dyn, Discovery: disc, Log: slog.New(slog.DiscardHandler)}
	res, err := w.ListOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || len(res.Forbidden) != 2 {
		t.Errorf("without Namespaces: %d items, forbidden %v", len(res.Items), res.Forbidden)
	}

	w.Namespaces = []string{"ns"}
	res, err = w.ListOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Obj.GetName() != "a" {
		t.Errorf("items %v", res.Items)
	}
	if len(res.PerNamespace) != 1 || res.PerNamespace[0] != jobsGVR || len(res.Available) != 1 {
		t.Errorf("per namespace %v, available %v", res.PerNamespace, res.Available)
	}
	if len(res.Forbidden) != 1 || res.Forbidden[0] != cronJobsGVR {
		t.Errorf("forbidden %v", res.Forbidden)
	}
}
