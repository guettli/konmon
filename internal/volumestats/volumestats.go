// Package volumestats reads the usage of the volumes of PersistentVolumeClaims
// from the kubelets: the summary API (/stats/summary) of every Node, via the
// API server proxy (needs get on nodes/proxy).
package volumestats

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/guettli/konmon/internal/checks"
)

// The part of the kubelet summary API (k8s.io/kubelet/pkg/apis/stats/v1alpha1)
// which is needed here.
type summary struct {
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Volume []struct {
			Name           string    `json:"name"`
			Time           time.Time `json:"time"`
			AvailableBytes *uint64   `json:"availableBytes"`
			CapacityBytes  *uint64   `json:"capacityBytes"`
			UsedBytes      *uint64   `json:"usedBytes"`
			InodesFree     *uint64   `json:"inodesFree"`
			Inodes         *uint64   `json:"inodes"`
			InodesUsed     *uint64   `json:"inodesUsed"`
			PVCRef         *struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
		} `json:"volume"`
	} `json:"pods"`
}

// Fetch reads the summary of one Node and returns the stats of the volumes
// which belong to a PersistentVolumeClaim. client is a REST client of the
// core API group (e.g. CoreV1().RESTClient()).
func Fetch(ctx context.Context, client rest.Interface, node string) (map[checks.VolumeKey]checks.VolumeStats, error) {
	data, err := client.Get().Resource("nodes").Name(node).SubResource("proxy").Suffix("stats", "summary").DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("node %s: stats summary: %w", node, err)
	}
	return parse(data, node)
}

func parse(data []byte, node string) (map[checks.VolumeKey]checks.VolumeStats, error) {
	var s summary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("node %s: stats summary: %w", node, err)
	}
	result := map[checks.VolumeKey]checks.VolumeStats{}
	for _, p := range s.Pods {
		for _, v := range p.Volume {
			if v.PVCRef == nil || v.PVCRef.Name == "" {
				continue
			}
			key := checks.VolumeKey{Namespace: v.PVCRef.Namespace, PVC: v.PVCRef.Name}
			if old, ok := result[key]; ok && !v.Time.After(old.Time) {
				// Mounted by several Pods: the newest measurement wins.
				continue
			}
			result[key] = checks.VolumeStats{
				Node: node, Pod: p.PodRef.Name, Volume: v.Name, Time: v.Time,
				UsedBytes: v.UsedBytes, CapacityBytes: v.CapacityBytes, AvailableBytes: v.AvailableBytes,
				Inodes: v.Inodes, InodesUsed: v.InodesUsed, InodesFree: v.InodesFree,
			}
		}
	}
	return result, nil
}

// Collector keeps the latest stats of all Nodes. It is safe for concurrent
// use.
type Collector struct {
	// Client is a REST client of the core API group.
	Client rest.Interface
	// MaxAge: the stats of a Node which were not read for this long (its
	// kubelet fails, or the Nodes cannot be listed) are stale. Stale stats
	// are kept, marked as such, until the Node is gone, so a check can tell
	// "not known" from "not mounted".
	MaxAge time.Duration

	mu      sync.Mutex
	byNode  map[string]nodeStats
	failed  map[string]error
	listErr error
	fetched bool
}

type nodeStats struct {
	at    time.Time
	stats map[checks.VolumeKey]checks.VolumeStats
}

// Update reads the summaries of the given Nodes. It returns the errors per
// Node. The lock is not held while the kubelets are asked.
func (c *Collector) Update(ctx context.Context, nodes []string, now time.Time) map[string]error {
	type result struct {
		node  string
		stats map[checks.VolumeKey]checks.VolumeStats
		err   error
	}
	results := make(chan result, len(nodes))
	sem := make(chan struct{}, 8)
	for _, n := range nodes {
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			s, err := Fetch(reqCtx, c.Client, n)
			results <- result{n, s, err}
		}()
	}
	fetched := map[string]map[checks.VolumeKey]checks.VolumeStats{}
	failed := map[string]error{}
	for range nodes {
		r := <-results
		if r.err != nil {
			failed[r.node] = r.err
			continue
		}
		fetched[r.node] = r.stats
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byNode == nil {
		c.byNode = map[string]nodeStats{}
	}
	current := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		current[n] = true
	}
	for n := range c.byNode {
		if !current[n] {
			// The Node is gone, and so are its Pods.
			delete(c.byNode, n)
		}
	}
	for n, s := range fetched {
		c.byNode[n] = nodeStats{at: now, stats: s}
	}
	c.failed = failed
	c.listErr = nil
	c.fetched = true
	return failed
}

// ListFailed records that the Nodes could not be listed: the stats are not
// updated, and they become stale after MaxAge.
func (c *Collector) ListFailed(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listErr = err
}

// Stats returns the stats of all PVCs; stats nil before the first Update.
// Stats of Nodes which were not read for MaxAge are marked Stale. complete
// is false if a Node failed or the Nodes could not be listed in the last
// try: then a PVC without stats may be mounted on a Node which failed.
func (c *Collector) Stats(now time.Time) (stats map[checks.VolumeKey]checks.VolumeStats, complete bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched {
		return nil, false
	}
	all := map[checks.VolumeKey]checks.VolumeStats{}
	for _, ns := range c.byNode {
		stale := now.Sub(ns.at) > c.MaxAge
		for k, v := range ns.stats {
			if old, ok := all[k]; ok && (!old.Stale && stale || old.Stale == stale && !v.Time.After(old.Time)) {
				continue
			}
			v.Stale = stale
			all[k] = v
		}
	}
	return all, len(c.failed) == 0 && c.listErr == nil
}

// Failed returns the Nodes whose summary could not be read in the last
// try; the key "" if the Nodes could not be listed.
func (c *Collector) Failed() map[string]error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listErr == nil {
		return c.failed
	}
	failed := make(map[string]error, len(c.failed)+1)
	for n, err := range c.failed {
		failed[n] = err
	}
	failed[""] = c.listErr
	return failed
}
