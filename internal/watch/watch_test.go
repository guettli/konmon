package watch

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	watchapi "k8s.io/apimachinery/pkg/watch"
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

func TestStaleWhenListAndWatchFailAfterTheStart(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{jobsGVR: "JobList"}, newJob("a", "uid-a"))
	var (
		mu        sync.Mutex
		forbidden bool
		watcher   *watchapi.FakeWatcher
	)
	denied := apierrors.NewForbidden(jobsGVR.GroupResource(), "", errors.New("RBAC changed"))
	dyn.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		return forbidden, nil, denied
	})
	dyn.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watchapi.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		if forbidden {
			return true, nil, denied
		}
		watcher = watchapi.NewFake()
		return true, watcher, nil
	})
	disc := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{{
		GroupVersion: "batch/v1",
		APIResources: []metav1.APIResource{{Name: "jobs", Namespaced: true, Kind: "Job", Verbs: []string{"list", "watch"}}},
	}}
	events := make(chan any, 100)
	w := &Watcher{Dynamic: dyn, Discovery: disc, Events: events, Log: slog.New(slog.DiscardHandler), SyncTimeout: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx, time.Hour) }()
	for {
		if _, ok := next(t, events).(Synced); ok {
			break
		}
	}
	if s := w.Stale(); len(s) != 0 {
		t.Fatalf("Stale() = %v before any failed call", s)
	}

	// Access is lost: the watch ends, and the informer cannot list or
	// watch again.
	// The reflector opens the watch after the initial list.
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return watcher != nil })
	mu.Lock()
	forbidden = true
	watcher.Stop()
	mu.Unlock()
	waitFor(t, func() bool { return len(w.Stale()) == 1 })
	s := w.Stale()[0]
	if s.GroupResource != jobsGVR.GroupResource() || !apierrors.IsForbidden(s.Err) || s.Since.IsZero() {
		t.Errorf("Stale() = %+v", s)
	}
	if len(w.Snapshot()) != 1 {
		t.Errorf("the objects of a stale type should stay, got %d", len(w.Snapshot()))
	}

	mu.Lock()
	forbidden = false
	mu.Unlock()
	waitFor(t, func() bool { return len(w.Stale()) == 0 })
}

func TestStaleWhenTheInitialEventsOfAWatchListFail(t *testing.T) {
	w := &Watcher{Log: slog.New(slog.DiscardHandler), SyncTimeout: time.Nanosecond}
	ctx := t.Context()
	failing := func(c *calls) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.since.IsZero()
	}
	endBookmark := &unstructured.Unstructured{}
	endBookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})

	// An Error event in the stream: failing, although the call succeeded.
	c := &calls{}
	fake := watchapi.NewFake()
	wi := w.recordInitialEvents(ctx, jobsGVR, c, fake)
	go fake.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 500, Reason: metav1.StatusReasonInternalError, Message: "conversion webhook"})
	<-wi.ResultChan()
	waitFor(t, func() bool { return failing(c) })

	// The stream ends before the bookmark at the end of the initial objects.
	c = &calls{}
	fake = watchapi.NewFake()
	wi = w.recordInitialEvents(ctx, jobsGVR, c, fake)
	go func() { fake.Add(newJob("a", "uid-a")); fake.Stop() }()
	for range wi.ResultChan() {
	}
	waitFor(t, func() bool { return failing(c) })

	// All objects sent: the failure is cleared.
	fake = watchapi.NewFake()
	wi = w.recordInitialEvents(ctx, jobsGVR, c, fake)
	go func() { fake.Add(newJob("a", "uid-a")); fake.Action(watchapi.Bookmark, endBookmark) }()
	<-wi.ResultChan()
	<-wi.ResultChan()
	waitFor(t, func() bool { return !failing(c) })

	// Stopped by the reflector: not a failure.
	c = &calls{}
	fake = watchapi.NewFake()
	wi = w.recordInitialEvents(ctx, jobsGVR, c, fake)
	wi.Stop()
	time.Sleep(50 * time.Millisecond)
	if failing(c) {
		t.Error("a watch stopped by the reflector counts as failure")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(20 * time.Millisecond)
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
	if len(res.PerNamespace) != 1 || !slices.Equal(res.PerNamespace[jobsGVR], []string{"ns"}) || len(res.Available) != 1 {
		t.Errorf("per namespace %v, available %v", res.PerNamespace, res.Available)
	}
	if len(res.Forbidden) != 1 || res.Forbidden[0] != cronJobsGVR {
		t.Errorf("forbidden %v", res.Forbidden)
	}

	// A namespace which may not be read (or a typo) keeps the others.
	w.Namespaces = []string{"typo", "ns"}
	res, err = w.ListOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || !slices.Equal(res.PerNamespace[jobsGVR], []string{"ns"}) {
		t.Errorf("items %v, per namespace %v", res.Items, res.PerNamespace)
	}
	if got := res.ForbiddenIn["typo"]; len(got) != 1 || got[0] != jobsGVR {
		t.Errorf("forbidden in %v", res.ForbiddenIn)
	}
}
