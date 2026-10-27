package checks

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Categories of checks. Only health findings are sent as alerts.
const (
	CategoryHealth  = "health"
	CategoryHygiene = "hygiene"
)

// ErrorCheck is the check name of findings which report that a check failed.
const ErrorCheck = "check-error"

// Finding is one problem of one object, reported by one check.
type Finding struct {
	Check    string `json:"check"`
	Category string `json:"category"`
	Severity string `json:"severity"`

	GVR       schema.GroupVersionResource `json:"gvr"`
	Kind      string                      `json:"kind,omitempty"`
	Namespace string                      `json:"namespace,omitempty"`
	Name      string                      `json:"name"`
	UID       string                      `json:"uid"`

	// Key distinguishes several findings of one check on one object.
	Key string `json:"key,omitempty"`
	// Message is one line without an age; Line adds the age from Since.
	Message string `json:"message"`
	// Since is when the problem started, if the check knows it.
	Since     time.Time         `json:"since,omitzero"`
	Condition string            `json:"condition,omitempty"`
	Container string            `json:"container,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
	// For overrides konmon's --for (from the check's forSeconds).
	For time.Duration `json:"for,omitempty"`
}

// ID identifies a finding across evaluations. It does not contain values
// which change while the problem persists.
func (f *Finding) ID() string {
	return f.Check + "|" + f.UID + "|" + f.Key
}

// Line formats the finding for the CLI, similar to check-conditions:
// "  namespace resource name message (age) [check]".
func (f *Finding) Line(now time.Time) string {
	var b strings.Builder
	if f.UID == "" {
		b.WriteString("  " + f.Message) // not about one object, e.g. check-error
	} else {
		fmt.Fprintf(&b, "  %s %s %s %s", f.Namespace, f.GVR.Resource, f.Name, f.Message)
	}
	if !f.Since.IsZero() {
		fmt.Fprintf(&b, " (%s)", now.Sub(f.Since).Round(time.Second))
	}
	fmt.Fprintf(&b, " [%s]", f.Check)
	return b.String()
}

// Subject is the text --ignore-finding-regex is matched against.
func (f *Finding) Subject() string {
	return fmt.Sprintf("%s %s %s/%s %s", f.Check, f.GVR.Resource, f.Namespace, f.Name, f.Message)
}
