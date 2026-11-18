package checks

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// metadataOnly resource types are only read as metadata (apiVersion, kind
// and metadata, without the last-applied-configuration annotation), and only
// for a check which names them: "*" never includes them.
var metadataOnly = []schema.GroupResource{
	{Resource: "secrets"},
}

// Denied reports whether a resource type must never be read: subresources.
func Denied(gr schema.GroupResource) bool {
	return strings.Contains(gr.Resource, "/")
}

// MetadataOnly reports whether konmon reads only the metadata of a type:
// Secrets. Their data never reaches konmon.
func MetadataOnly(gr schema.GroupResource) bool {
	for _, m := range metadataOnly {
		if m == gr {
			return true
		}
	}
	return false
}

// ParseResource parses "resource" (core group) or "resource.group", like
// kubectl does for the full form: "pods", "deployments.apps",
// "ingresses.networking.k8s.io". A name without a dot is always the core
// group, so it never matches a custom resource of the same name. Case does
// not matter: resource and group names are always lowercase.
func ParseResource(s string) (schema.GroupResource, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "*" {
		return schema.GroupResource{}, fmt.Errorf("invalid resource %q", s)
	}
	resource, group, _ := strings.Cut(s, ".")
	if resource == "" || strings.ContainsAny(s, "/* ") {
		return schema.GroupResource{}, fmt.Errorf("invalid resource %q: use resource or resource.group", s)
	}
	return schema.GroupResource{Group: group, Resource: resource}, nil
}

// FormatResource is the inverse of ParseResource.
func FormatResource(gr schema.GroupResource) string {
	if gr.Group == "" {
		return gr.Resource
	}
	return gr.Resource + "." + gr.Group
}

// resourceSet matches resource types: all ("*") and a fixed set. "*" does
// not include the MetadataOnly types: a check has to name them.
type resourceSet struct {
	all bool
	set map[schema.GroupResource]struct{}
}

func parseResourceSet(patterns []string, allowAll bool) (resourceSet, error) {
	rs := resourceSet{set: map[schema.GroupResource]struct{}{}}
	for _, p := range patterns {
		if p == "*" {
			if !allowAll {
				return rs, errors.New(`"*" is not allowed here`)
			}
			rs.all = true
			continue
		}
		gr, err := ParseResource(p)
		if err != nil {
			return rs, err
		}
		rs.set[gr] = struct{}{}
	}
	return rs, nil
}

func (rs resourceSet) contains(gr schema.GroupResource) bool {
	if _, ok := rs.set[gr]; ok {
		return true
	}
	return rs.all && !MetadataOnly(gr)
}
