package checks

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Item is one object of the cluster.
type Item struct {
	GVR schema.GroupVersionResource
	Obj *unstructured.Unstructured
}

// Snapshot is a consistent view of the cluster for one evaluation, with
// indexes for ctx.get and ctx.list. Objects must not be modified.
type Snapshot struct {
	items     []Item
	available map[schema.GroupResource]schema.GroupVersionResource
	byType    map[schema.GroupResource][]*unstructured.Unstructured
	byName    map[nameKey]*unstructured.Unstructured
	byOwner   map[string][]ownedItem
	// partial types were only listed in these namespaces.
	partial map[schema.GroupResource][]string
}

type nameKey struct {
	gr              schema.GroupResource
	namespace, name string
}

type ownedItem struct {
	gr  schema.GroupResource
	obj *unstructured.Unstructured
}

// NewSnapshot indexes items. available lists the resource types which are
// fully known (listed or synced): reading other types is an error, so a
// type which is not watched never looks like a type without objects.
func NewSnapshot(items []Item, available []schema.GroupVersionResource) *Snapshot {
	s := &Snapshot{
		items:     items,
		available: make(map[schema.GroupResource]schema.GroupVersionResource, len(available)),
		byType:    map[schema.GroupResource][]*unstructured.Unstructured{},
		byName:    make(map[nameKey]*unstructured.Unstructured, len(items)),
		byOwner:   map[string][]ownedItem{},
	}
	for _, gvr := range available {
		s.available[gvr.GroupResource()] = gvr
	}
	// Sort for deterministic results of ctx.list and of the evaluation order.
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.GVR.Group != b.GVR.Group {
			return a.GVR.Group < b.GVR.Group
		}
		if a.GVR.Resource != b.GVR.Resource {
			return a.GVR.Resource < b.GVR.Resource
		}
		if a.Obj.GetNamespace() != b.Obj.GetNamespace() {
			return a.Obj.GetNamespace() < b.Obj.GetNamespace()
		}
		return a.Obj.GetName() < b.Obj.GetName()
	})
	for _, it := range items {
		gr := it.GVR.GroupResource()
		s.byType[gr] = append(s.byType[gr], it.Obj)
		s.byName[nameKey{gr, it.Obj.GetNamespace(), it.Obj.GetName()}] = it.Obj
		for _, ref := range it.Obj.GetOwnerReferences() {
			s.byOwner[string(ref.UID)] = append(s.byOwner[string(ref.UID)], ownedItem{gr, it.Obj})
		}
	}
	return s
}

// Items returns all objects, sorted by type, namespace and name.
func (s *Snapshot) Items() []Item { return s.items }

// Available reports whether the objects of a type are known.
func (s *Snapshot) Available(gr schema.GroupResource) bool {
	_, ok := s.available[gr]
	return ok
}

// SetPartial marks a type whose objects were only listed in some
// namespaces: reading it elsewhere is an error, so an object in another
// namespace never looks missing.
func (s *Snapshot) SetPartial(gr schema.GroupResource, namespaces []string) {
	if s.partial == nil {
		s.partial = map[schema.GroupResource][]string{}
	}
	s.partial[gr] = namespaces
}

// covers returns an error if the objects of a type in a namespace are not
// known. all: every namespace.
func (s *Snapshot) covers(gr schema.GroupResource, namespace string, all bool) error {
	namespaces, ok := s.partial[gr]
	if !ok || (!all && slices.Contains(namespaces, namespace)) {
		return nil
	}
	where := "in all namespaces"
	if !all {
		where = fmt.Sprintf("in namespace %q", namespace)
	}
	return fmt.Errorf("resource %s is only listed in namespace %s (not allowed to list it %s)",
		FormatResource(gr), strings.Join(namespaces, ", "), where)
}

func (s *Snapshot) get(gr schema.GroupResource, namespace, name string) *unstructured.Unstructured {
	return s.byName[nameKey{gr, namespace, name}]
}

type listOptions struct {
	namespace    string
	hasNamespace bool
	labels       map[string]string
	owner        string
	// ownerNamespace: of an owner given as object; "" for a cluster-scoped
	// owner or a UID.
	ownerNamespace string
}

func (s *Snapshot) list(gr schema.GroupResource, o listOptions) []*unstructured.Unstructured {
	candidates := s.byType[gr]
	if o.owner != "" {
		candidates = nil
		for _, it := range s.byOwner[o.owner] {
			if it.gr == gr {
				candidates = append(candidates, it.obj)
			}
		}
	}
	var result []*unstructured.Unstructured
	for _, obj := range candidates {
		if o.hasNamespace && obj.GetNamespace() != o.namespace {
			continue
		}
		if !labelsMatch(obj.GetLabels(), o.labels) {
			continue
		}
		result = append(result, obj)
	}
	return result
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}
