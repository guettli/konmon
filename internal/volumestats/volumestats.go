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

// Collector keeps the latest stats of all Nodes.
type Collector struct {
	// Client is a REST client of the core API group.
	Client rest.Interface
	// MaxAge: the stats of a Node whose summary could not be read for this
	// long are dropped.
	MaxAge time.Duration

	mu      sync.Mutex
	byNode  map[string]nodeStats
	failed  map[string]error
	fetched bool
}

type nodeStats struct {
	at    time.Time
	stats map[checks.VolumeKey]checks.VolumeStats
}

// Update reads the summaries of the given Nodes. It returns the errors per
// Node; the stats of a failed Node are kept until they are older than
// MaxAge.
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
	failed := map[string]error{}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byNode == nil {
		c.byNode = map[string]nodeStats{}
	}
	current := map[string]bool{}
	for range nodes {
		r := <-results
		current[r.node] = true
		if r.err != nil {
			failed[r.node] = r.err
			continue
		}
		c.byNode[r.node] = nodeStats{at: now, stats: r.stats}
	}
	for n, s := range c.byNode {
		if !current[n] || now.Sub(s.at) > c.MaxAge {
			delete(c.byNode, n)
		}
	}
	c.failed = failed
	c.fetched = true
	return failed
}

// Stats returns the stats of all PVCs; nil before the first Update.
func (c *Collector) Stats() map[checks.VolumeKey]checks.VolumeStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched {
		return nil
	}
	all := map[checks.VolumeKey]checks.VolumeStats{}
	for _, ns := range c.byNode {
		for k, v := range ns.stats {
			if old, ok := all[k]; ok && !v.Time.After(old.Time) {
				continue
			}
			all[k] = v
		}
	}
	return all
}

// Failed returns the Nodes whose summary could not be read in the last
// Update.
func (c *Collector) Failed() map[string]error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}
