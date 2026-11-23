package cmd

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/volumestats"
)

// defaultVolumeStatsNodeSelector skips virtual kubelets (Liqo,
// virtual-kubelet): they do not serve the summary API, and the volumes of
// their Pods belong to another cluster.
const defaultVolumeStatsNodeSelector = "liqo.io/type!=virtual-node,type!=virtual-kubelet"

// volumeStatsNodes lists the Nodes whose kubelets are asked.
func volumeStatsNodes(ctx context.Context, core corev1client.CoreV1Interface, selector string) ([]string, error) {
	list, err := core.Nodes().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}

// updateVolumeStats reads the summaries of all selected Nodes once and logs
// Nodes which start or stop failing (failing is updated).
func updateVolumeStats(ctx context.Context, c *volumestats.Collector, core corev1client.CoreV1Interface, selector string,
	failing map[string]bool, log *slog.Logger,
) {
	nodes, err := volumeStatsNodes(ctx, core, selector)
	if err != nil {
		if ctx.Err() == nil && !failing[""] {
			log.Warn("cannot list Nodes for volume stats; checks which use volume-stats see no new data", "err", err)
		}
		failing[""] = true
		return
	}
	if failing[""] {
		log.Info("listing Nodes for volume stats again")
		delete(failing, "")
	}
	failed := c.Update(ctx, nodes, time.Now())
	if ctx.Err() != nil {
		return
	}
	for _, n := range slices.Sorted(maps.Keys(failed)) {
		if !failing[n] {
			log.Warn("cannot read volume stats from the kubelet (needs get on nodes/proxy); its volumes are not checked",
				"node", n, "err", failed[n])
			failing[n] = true
		}
	}
	for n := range failing {
		if _, still := failed[n]; n != "" && !still {
			log.Info("reading volume stats from the kubelet again", "node", n)
			delete(failing, n)
		}
	}
}

// collectVolumeStats updates c every interval until ctx is done. It never
// returns an error: a kubelet which cannot be asked is logged.
func collectVolumeStats(ctx context.Context, c *volumestats.Collector, core corev1client.CoreV1Interface, selector string,
	interval time.Duration, log *slog.Logger,
) error {
	failing := map[string]bool{}
	updateVolumeStats(ctx, c, core, selector, failing, log)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			updateVolumeStats(ctx, c, core, selector, failing, log)
		}
	}
}

// volumeStatsFunc returns the stats of c for the monitor; nil if c is nil.
func volumeStatsFunc(c *volumestats.Collector) func() map[checks.VolumeKey]checks.VolumeStats {
	if c == nil {
		return nil
	}
	return c.Stats
}
