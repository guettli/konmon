package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/guettli/konmon/internal/store"
)

// Verdicts of the condition history about a group of findings, in the order
// triage shows them.
const (
	verdictFalsePositive = "possibly normal for this type"
	verdictInformational = "possibly informational"
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

// assess judges a group of conditions findings with the history. It is
// deliberately cautious: a wrong "possibly normal" could make somebody
// silence a real outage.
//
//   - possibly normal: every object of the group has the status for every
//     type of the group since more than a day (the newest transition), all
//     of them have Ready=True, and every object of the type in the cluster
//     has this status; at least 2 objects, not for status Unknown.
//   - possibly informational: the type changes often while Ready stays True
//     on all objects of the group during the whole window.
//   - likely real: Ready became unhealthy shortly after the type got the
//     status, on any object of the group.
func (h *historyData) assess(g *triageGroup, now time.Time) evidence {
	if g.check != "conditions" || g.condition == "" || h == nil {
		return evidence{}
	}
	types := strings.Split(g.condition, "/")
	inGroup := map[string]bool{}
	for _, f := range g.findings {
		inGroup[f.UID] = true
	}
	var objects []objectConditions // of the group, known to the store
	for _, o := range h.objects[g.resource] {
		if inGroup[o.uid] {
			objects = append(objects, o)
		}
	}
	if len(objects) == 0 {
		return evidence{}
	}
	what := fmt.Sprintf("%s=%s", g.condition, g.status)

	correlated, flapping := 0, 0
	allReady, readyStable := true, true
	var newest time.Time
	for _, o := range objects {
		ready, hasReady := o.conditions["Ready"]
		if !hasReady || ready.Status != "True" || slices.Contains(types, "Ready") {
			allReady = false
		}
		objFlaps, objCorrelated := true, false
		for _, typ := range types {
			c, ok := o.conditions[typ]
			if !ok || c.Status != g.status {
				allReady = false // the store is older or newer than the findings
				continue
			}
			t, err := time.Parse(time.RFC3339, c.LastTransitionTime)
			if err != nil {
				allReady = false
			} else if t.After(newest) {
				newest = t
			}
			n, corr, readyBad := h.changes(o.uid, typ, g.status)
			if corr {
				objCorrelated = true
			}
			if readyBad {
				readyStable = false
			}
			if n < flappingChanges {
				objFlaps = false
			}
		}
		if objFlaps {
			flapping++
		}
		if objCorrelated {
			correlated++
		}
	}
	if correlated > 0 {
		return evidence{verdictReal, fmt.Sprintf("Ready became unhealthy within %s after %s on %d of %s",
			roundDuration(correlatedWithin), what, correlated, plural(len(objects), "object"))}
	}
	// The other verdicts speak about every object of the group: not when
	// the store does not know some of them (another --exclude-namespace).
	if (g.status != "True" && g.status != "False") || len(objects) < len(inGroup) {
		return evidence{}
	}
	sameEverywhere := true
	for _, o := range h.objects[g.resource] {
		for _, typ := range types {
			if c, ok := o.conditions[typ]; ok {
				if c.Status != g.status {
					sameEverywhere = false
				}
			}
		}
	}
	switch {
	case allReady && sameEverywhere && len(objects) >= 2 && !newest.IsZero() && now.Sub(newest) >= constantFor:
		return evidence{verdictFalsePositive, fmt.Sprintf("%s on all %s of %s for at least %s, all of them Ready=True",
			what, plural(len(objects), "object"), g.resource, roundDuration(now.Sub(newest)))}
	case allReady && readyStable && flapping == len(objects):
		return evidence{verdictInformational, fmt.Sprintf("%s changed at least %d times on each of %s in %s while Ready stayed True",
			g.condition, flappingChanges, plural(len(objects), "object"), roundDuration(now.Sub(h.since)))}
	}
	return evidence{}
}

// changes counts the status changes of a condition type of one object, and
// reports whether Ready became unhealthy shortly after the type got status
// (a change from True, or the first Ready row of the window: a reason
// change while Ready stays False is not), and whether Ready was unhealthy
// at any time of the window.
func (h *historyData) changes(uid, typ, status string) (n int, correlated, readyBad bool) {
	var last, lastReady string
	var badAt time.Time
	for _, r := range h.history[uid] {
		if r.Change == "removed" {
			continue // carries the old status
		}
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
			wasHealthy := lastReady == "" || lastReady == "True"
			lastReady = r.Status
			if r.Status == "True" {
				continue
			}
			readyBad = true
			if typ != "Ready" && wasHealthy && !badAt.IsZero() && !r.ObservedAt.Before(badAt) && r.ObservedAt.Sub(badAt) <= correlatedWithin {
				correlated = true
			}
		}
	}
	return n, correlated, readyBad
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
