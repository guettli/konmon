// Package alertmanager sends findings to the Alertmanager v2 API, the same
// way Prometheus does: all firing alerts are re-sent on every evaluation with
// an endsAt in the near future, resolved alerts are sent with endsAt set to
// the time they were resolved.
package alertmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/guettli/check-conditions/pkg/checkconditions"

	"github.com/guettli/konmon/internal/store"
)

// Client posts alerts to one or more Alertmanagers.
type Client struct {
	// URLs are base URLs, e.g. http://alertmanager:9093.
	URLs []string
	// Labels are added to every alert, e.g. cluster=prod.
	Labels map[string]string
	// ResolveTimeout is how long a firing alert stays firing in Alertmanager
	// if konmon stops sending it. Prometheus uses 4 evaluation intervals.
	ResolveTimeout time.Duration
	// Watchdog sends an always-firing KonmonWatchdog alert, to be routed to a
	// dead man's switch.
	Watchdog bool
	// GeneratorURL is shown as source link in Alertmanager. Optional.
	GeneratorURL string
	HTTP         *http.Client
}

// Alert is the Alertmanager v2 postable alert.
type Alert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

// Notify implements monitor.Notifier.
func (c *Client) Notify(ctx context.Context, now time.Time, firing, resolved []store.FindingRow) error {
	alerts := make([]Alert, 0, len(firing)+len(resolved)+1)
	for i := range firing {
		alerts = append(alerts, c.alert(&firing[i], now, now.Add(c.ResolveTimeout)))
	}
	for i := range resolved {
		alerts = append(alerts, c.alert(&resolved[i], now, resolved[i].ResolvedAt))
	}
	if c.Watchdog {
		labels := c.labels()
		labels["alertname"] = "KonmonWatchdog"
		labels["severity"] = "none"
		alerts = append(alerts, Alert{
			Labels: labels,
			Annotations: map[string]string{
				"summary": "konmon is running. This alert is always firing; route it to a dead man's switch.",
			},
			StartsAt:     now,
			EndsAt:       now.Add(c.ResolveTimeout),
			GeneratorURL: c.GeneratorURL,
		})
	}
	if len(alerts) == 0 {
		return nil
	}
	body, err := json.Marshal(alerts)
	if err != nil {
		return err
	}
	var errs []string
	for _, u := range c.URLs {
		if err := c.post(ctx, u, body); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("alertmanager: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (c *Client) labels() map[string]string {
	labels := make(map[string]string, len(c.Labels)+8)
	for k, v := range c.Labels {
		labels[k] = v
	}
	return labels
}

func (c *Client) alert(r *store.FindingRow, now, endsAt time.Time) Alert {
	f := &r.Finding
	labels := c.labels()
	labels["alertname"] = "Konmon" + string(f.Kind)
	labels["severity"] = "warning"
	setIfNotEmpty(labels, "group", f.GVR.Group)
	setIfNotEmpty(labels, "resource", f.GVR.Resource)
	setIfNotEmpty(labels, "namespace", f.Namespace)
	setIfNotEmpty(labels, "name", f.Name)
	if f.Kind == checkconditions.FindingCondition {
		setIfNotEmpty(labels, "condition", strings.Join(f.ConditionTypes, "/"))
	}
	setIfNotEmpty(labels, "container", f.Container)

	annotations := map[string]string{
		"summary": strings.TrimSpace(f.Line(now)),
	}
	setIfNotEmpty(annotations, "status", f.Status)
	setIfNotEmpty(annotations, "reason", f.Reason)
	setIfNotEmpty(annotations, "message", f.Message)
	setIfNotEmpty(annotations, "detail", f.Detail)
	if !f.LastTransitionTime.IsZero() {
		annotations["lastTransitionTime"] = f.LastTransitionTime.UTC().Format(time.RFC3339)
	}
	return Alert{
		Labels:       labels,
		Annotations:  annotations,
		StartsAt:     r.FirstSeen,
		EndsAt:       endsAt,
		GeneratorURL: c.GeneratorURL,
	}
}

func setIfNotEmpty(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func (c *Client) post(ctx context.Context, baseURL string, body []byte) error {
	u := strings.TrimSuffix(baseURL, "/") + "/api/v2/alerts"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("post %s: %s: %s", u, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
