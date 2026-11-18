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
	fakemetadata "k8s.io/client-go/metadata/fake"
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
	if obj, synced := w.Get(jobsGVR.GroupResource(), "ns", "a"); !synced || obj == nil || obj.GetUID() != "uid-a" {
		t.Errorf("Get(a) = %v, synced %t", obj, synced)
	}
	if obj, synced := w.Get(jobsGVR.GroupResource(), "ns", "nope"); !synced || obj != nil {
		t.Errorf("Get(nope) = %v, synced %t: want nil, true", obj, synced)
	}
	if obj, synced := w.Get(cronJobsGVR.GroupResource(), "ns", "a"); synced || obj != nil {
		t.Errorf("Get of a type which is not watched = %v, synced %t", obj, synced)
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
	wi.Stop()

	// The stream ends before the bookmark at the end of the initial objects.
	c = &calls{}
	fake = watchapi.NewFake()
	wi = w.recordInitialEvents(ctx, jobsGVR, c, fake)
	go func() { fake.Add(newJob("a", "uid-a")); fake.Stop() }()
	<-wi.ResultChan()
	if _, open := <-wi.ResultChan(); open {
		t.Fatal("the stream should end")
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

func TestSkippedTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{jobsGVR: "JobList", cronJobsGVR: "CronJobList"})
	var (
		mu      sync.Mutex
		allowed bool
	)
	dyn.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		return !allowed, nil, apierrors.NewForbidden(jobsGVR.GroupResource(), "", errors.New("RBAC"))
	})
	dyn.PrependReactor("list", "cronjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(cronJobsGVR.GroupResource(), "", errors.New("RBAC"))
	})
	disc := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	setResources := func(names ...string) {
		rs := make([]metav1.APIResource, 0, len(names))
		for _, n := range names {
			rs = append(rs, metav1.APIResource{Name: n, Namespaced: true, Verbs: []string{"list", "watch"}})
		}
		disc.Resources = []*metav1.APIResourceList{{GroupVersion: "batch/v1", APIResources: rs}}
	}
	setResources("jobs", "cronjobs")
	w := &Watcher{
		Dynamic: dyn, Discovery: disc, Events: make(chan any, 100), Log: slog.New(slog.DiscardHandler),
		informers: map[schema.GroupResource]*running{}, skipped: map[schema.GroupResource]error{},
	}
	ctx := t.Context()
	skipped := func() []string {
		types := w.Skipped()
		names := make([]string, 0, len(types))
		for _, s := range types {
			names = append(names, s.String()+" "+s.Reason)
		}
		slices.Sort(names)
		return names
	}
	if err := w.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := skipped(); !slices.Equal(got, []string{"cronjobs.batch Forbidden", "jobs.batch Forbidden"}) {
		t.Fatalf("Skipped() = %v", got)
	}

	// Allowed now: watched at the next discovery.
	mu.Lock()
	allowed = true
	mu.Unlock()
	if err := w.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := skipped(); !slices.Equal(got, []string{"cronjobs.batch Forbidden"}) {
		t.Errorf("Skipped() = %v", got)
	}

	// The type is gone (CRD deleted): not skipped any more.
	setResources("jobs")
	if err := w.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := skipped(); len(got) != 0 {
		t.Errorf("Skipped() = %v", got)
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
	stuck := &running{gvr: widgets.WithVersion("v1"), synced: make(chan struct{}), started: time.Now(), calls: &calls{}}
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
	// Also stale, although no call failed: its findings are frozen.
	if st := w.Stale(); len(st) != 1 || st[0].GroupResource != widgets || !st[0].Since.Equal(stuck.started) {
		t.Errorf("Stale() = %v", st)
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

var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

func newSecretMetadata(name, uid string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: name, UID: types.UID(uid),
			Labels:        map[string]string{"app": "web"},
			Annotations:   map[string]string{lastApplied: `{"data":{"key":"dmFsdWU="}}`, "keep": "me"},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
	}
}

func newFakeMetadata(objs ...runtime.Object) *fakemetadata.FakeMetadataClient {
	scheme := fakemetadata.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		panic(err)
	}
	return fakemetadata.NewSimpleMetadataClient(scheme, objs...)
}

func secretsWatcher(meta *fakemetadata.FakeMetadataClient, events chan any) *Watcher {
	scheme := runtime.NewScheme()
	// The dynamic client has the full Secret: it must never be used.
	full := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"namespace": "ns", "name": "full", "uid": "uid-full"},
		"data":     map[string]any{"key": "dmFsdWU="},
	}}
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{secretsGVR: "SecretList"}, full)
	disc := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "secrets", Namespaced: true, Kind: "Secret", Verbs: []string{"get", "list", "watch"}},
		},
	}}
	w := &Watcher{
		Dynamic:      dyn,
		Discovery:    disc,
		MetadataOnly: func(gr schema.GroupResource) bool { return gr == secretsGVR.GroupResource() },
		Events:       events,
		Log:          slog.New(slog.DiscardHandler),
	}
	if meta != nil {
		w.Metadata = meta
	}
	return w
}

func checkMetadataOnly(t *testing.T, u *unstructured.Unstructured, name string) {
	t.Helper()
	if u.GetName() != name || u.GetKind() != "Secret" || u.GetAPIVersion() != "v1" {
		t.Errorf("got %s %s/%s", u.GetAPIVersion(), u.GetKind(), u.GetName())
	}
	for k := range u.Object {
		if k != "apiVersion" && k != "kind" && k != "metadata" {
			t.Errorf("field %q of a metadata-only object", k)
		}
	}
	if _, ok := u.GetAnnotations()[lastApplied]; ok || u.GetAnnotations()["keep"] != "me" {
		t.Errorf("annotations %v", u.GetAnnotations())
	}
	if len(u.GetManagedFields()) != 0 || u.GetLabels()["app"] != "web" {
		t.Errorf("managedFields %v, labels %v", u.GetManagedFields(), u.GetLabels())
	}
}

func TestMetadataOnlyTypesAreWatchedWithTheMetadataAPI(t *testing.T) {
	meta := newFakeMetadata(newSecretMetadata("a", "uid-a"))
	events := make(chan any, 100)
	w := secretsWatcher(meta, events)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx, time.Hour) }()

	up, ok := next(t, events).(Upsert)
	if !ok || up.GVR != secretsGVR {
		t.Fatalf("want Upsert of a, got %#v", up)
	}
	checkMetadataOnly(t, up.Obj, "a")
	if _, ok := next(t, events).(Synced); !ok {
		t.Fatal("want Synced")
	}

	if _, err := meta.Resource(secretsGVR).Namespace("ns").(fakemetadata.MetadataClient).CreateFake(newSecretMetadata("b", "uid-b"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	up, ok = next(t, events).(Upsert)
	if !ok {
		t.Fatalf("want Upsert of b, got %#v", up)
	}
	checkMetadataOnly(t, up.Obj, "b")
	if obj, synced := w.Get(secretsGVR.GroupResource(), "ns", "b"); !synced || obj == nil {
		t.Errorf("Get(b) = %v, synced %t", obj, synced)
	} else {
		checkMetadataOnly(t, obj, "b")
	}

	if err := meta.Resource(secretsGVR).Namespace("ns").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if del, ok := next(t, events).(Delete); !ok || del.UID != "uid-a" {
		t.Fatalf("want Delete of uid-a, got %#v", del)
	}
}

func TestListOnceListsMetadataOnlyTypesWithTheMetadataAPI(t *testing.T) {
	meta := newFakeMetadata(newSecretMetadata("a", "uid-a"))
	res, err := secretsWatcher(meta, nil).ListOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || len(res.Available) != 1 {
		t.Fatalf("items %v, available %v", res.Items, res.Available)
	}
	checkMetadataOnly(t, res.Items[0].Obj, "a")
}

func TestMetadataOnlyTypesAreSkippedWithoutMetadataClient(t *testing.T) {
	res, err := secretsWatcher(nil, nil).ListOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || len(res.Available) != 0 {
		t.Errorf("a metadata-only type was read with the dynamic client: %v", res.Items)
	}
}

func TestStripToMetadata(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "s", "annotations": map[string]any{lastApplied: "{}"}},
		"data":     map[string]any{"k": "dg=="},
		"type":     "Opaque",
	}}
	got := StripToMetadata(u)
	if len(got.Object) != 3 || got.GetAnnotations() != nil || got.GetName() != "s" {
		t.Errorf("got %v", got.Object)
	}
	if u.Object["data"] == nil || u.GetAnnotations()[lastApplied] != "{}" {
		t.Error("the original must not change")
	}
}
