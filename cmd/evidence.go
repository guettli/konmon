package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/guettli/konmon/internal/store"
)

// Verdicts of the condition history about a group of findings, in the order
// triage shows them.
const (
	verdictFalsePositive = "likely false positive"
	verdictInformational = "likely informational"
	verdictNone          = ""
	verdictReal          = "likely real"
)

func verdictRank(v string) int {
	switch v {
	case verdictFalsePositive:
		return 0
	case verdictInformational:
		return 1
	case verdictReal:
		return 3
	}
	return 2
}

// evidence is what the condition history says about a group.
type evidence struct {
	verdict string
	reason  string
}

// Thresholds of assess.
const (
	// constantFor: a condition which has had its "bad" status this long on
	// every object of the type, while they are Ready, is probably healthy.
	constantFor = 24 * time.Hour
	// flappingChanges: this many status changes per object in the window
	// while Ready stays True: informational.
	flappingChanges = 6
	// correlatedWithin: Ready went bad this soon after the condition.
	correlatedWithin = 10 * time.Minute
)

// objectConditions are the current conditions of one object.
type objectConditions struct {
	uid        string
	conditions map[string]store.Condition // by type
}

// historyData is the history konmon recorded, for assess.
type historyData struct {
	// objects by resource.group, only those with status.conditions.
	objects map[string][]objectConditions
	history map[string][]store.HistoryRow // by uid
	since   time.Time
}

// loadHistory reads the current conditions of the objects of the resource
// types and the condition history since since.
func loadHistory(ctx context.Context, st *store.Store, resources map[string]bool, since time.Time) (*historyData, error) {
	h := &historyData{objects: map[string][]objectConditions{}, since: since}
	err := st.Objects(ctx, func(ref store.ObjectRef, object []byte) error {
		resource := ref.GVR.Resource
		if ref.GVR.Group != "" {
			resource += "." + ref.GVR.Group
		}
		if !resources[resource] {
			return nil
		}
		var obj struct {
			Status struct {
				Conditions []store.Condition `json:"conditions"`
			} `json:"status"`
		}
		if err := json.Unmarshal(object, &obj); err != nil {
			return nil //nolint:nilerr // status.conditions of another shape: no evidence, not an error
		}
		if len(obj.Status.Conditions) == 0 {
			return nil
		}
		oc := objectConditions{uid: ref.UID, conditions: map[string]store.Condition{}}
		for _, c := range obj.Status.Conditions {
			oc.conditions[c.Type] = c
		}
		h.objects[resource] = append(h.objects[resource], oc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.history, err = st.ConditionHistorySince(ctx, since)
	return h, err
}

// assess judges a group of conditions findings with the history: is the
// "bad" status normal for this type (constant on all objects while they are
// Ready, or flapping while Ready stays True), or did Ready go bad after it?
func (h *historyData) assess(g *triageGroup, now time.Time) evidence {
	if g.check != "conditions" || g.condition == "" || h == nil {
		return evidence{}
	}
	typ := strings.Split(g.condition, "/")[0]
	objects := h.objects[g.resource]
	var withType, bad, badReady, changes, correlated int
	var oldest time.Time
	for _, o := range objects {
		c, ok := o.conditions[typ]
		if !ok {
			continue
		}
		withType++
		if c.Status != g.status {
			continue
		}
		bad++
		if ready, ok := o.conditions["Ready"]; typ == "Ready" || !ok || ready.Status == "True" {
			badReady++
		}
		if t, err := time.Parse(time.RFC3339, c.LastTransitionTime); err == nil && (oldest.IsZero() || t.Before(oldest)) {
			oldest = t
		}
		n, corr := h.changes(o.uid, typ, g.status)
		changes += n
		if corr {
			correlated++
		}
	}
	if bad == 0 {
		return evidence{}
	}
	what := fmt.Sprintf("%s=%s", typ, g.status)
	switch {
	case correlated > 0:
		return evidence{verdictReal, fmt.Sprintf("Ready became unhealthy within %s after %s on %d of %s",
			roundDuration(correlatedWithin), what, correlated, plural(bad, "object"))}
	case typ != "Ready" && bad == withType && badReady == bad && !oldest.IsZero() && now.Sub(oldest) >= constantFor:
		return evidence{verdictFalsePositive, fmt.Sprintf("%s on %d/%d %s for %s, all of them Ready",
			what, bad, withType, g.resource, roundDuration(now.Sub(oldest)))}
	case typ != "Ready" && badReady == bad && changes >= flappingChanges*bad:
		return evidence{verdictInformational, fmt.Sprintf("%s changed %d times on %s in %s while Ready stayed True",
			typ, changes, plural(bad, "object"), roundDuration(now.Sub(h.since)))}
	}
	return evidence{}
}

// changes counts the status changes of a condition type of one object, and
// reports whether Ready became unhealthy shortly after the type got status.
func (h *historyData) changes(uid, typ, status string) (int, bool) {
	var n int
	var last string
	var badAt time.Time
	correlated := false
	for _, r := range h.history[uid] {
		switch r.Type {
		case typ:
			if last != "" && r.Status != last {
				n++
			}
			last = r.Status
			if r.Status == status {
				badAt = r.ObservedAt
			}
		case "Ready":
			if typ != "Ready" && r.Status != "True" && !badAt.IsZero() && r.ObservedAt.Sub(badAt) <= correlatedWithin && !r.ObservedAt.Before(badAt) {
				correlated = true
			}
		}
	}
	return n, correlated
}

// roundDuration is "3d", "5h" or "12m".
func roundDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
