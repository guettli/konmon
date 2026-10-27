package checks

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// denied resource types are never watched, stored or given to checks, no
// matter what a check asks for.
var denied = []schema.GroupResource{
	{Resource: "secrets"},
}

// Denied reports whether a resource type must never be read.
func Denied(gr schema.GroupResource) bool {
	if strings.Contains(gr.Resource, "/") {
		return true // subresources
	}
	for _, d := range denied {
		if d == gr {
			return true
		}
	}
	return false
}

// ParseResource parses "resource" (core group) or "resource.group", like
// kubectl does for the full form: "pods", "deployments.apps",
// "ingresses.networking.k8s.io". A name without a dot is always the core
// group, so it never matches a custom resource of the same name.
func ParseResource(s string) (schema.GroupResource, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "*" {
		return schema.GroupResource{}, fmt.Errorf("invalid resource %q", s)
	}
	resource, group, _ := strings.Cut(s, ".")
	if resource == "" || strings.ContainsAny(s, "/* ") {
		return schema.GroupResource{}, fmt.Errorf("invalid resource %q: use resource or resource.group", s)
	}
	return schema.GroupResource{Group: group, Resource: strings.ToLower(resource)}, nil
}

// FormatResource is the inverse of ParseResource.
func FormatResource(gr schema.GroupResource) string {
	if gr.Group == "" {
		return gr.Resource
	}
	return gr.Resource + "." + gr.Group
}

// resourceSet matches resource types: either all ("*") or a fixed set.
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
	if rs.all {
		return true
	}
	_, ok := rs.set[gr]
	return ok
}
