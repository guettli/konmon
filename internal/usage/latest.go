package usage

import (
	"sync"
	"time"

	"github.com/guettli/konmon/internal/checks"
)

// Latest keeps the last use of the containers of every Pod, for checks
// which use "container-usage". It is safe for concurrent use.
type Latest struct {
	// MaxAge: usage which was not read for this long (the metrics API
	// fails) is stale.
	MaxAge time.Duration

	mu      sync.Mutex
	at      time.Time
	pods    map[checks.PodKey]checks.PodUsage
	err     error
	fetched bool
}

// Set replaces the usage with a successful read at now.
func (l *Latest) Set(list []checks.PodUsage, now time.Time) {
	pods := make(map[checks.PodKey]checks.PodUsage, len(list))
	for _, pu := range list {
		pods[checks.PodKey{Namespace: pu.Namespace, Pod: pu.Pod}] = pu
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at, l.pods, l.err, l.fetched = now, pods, nil, true
}

// Failed records that the metrics API could not be read: the last usage is
// kept, and it becomes stale after MaxAge.
func (l *Latest) Failed(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

// Err is the error of the last read; nil if it worked or nothing was read
// yet.
func (l *Latest) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Usage returns the usage of all Pods; nil before the first read (an
// empty map if it failed). Usage older than MaxAge is marked Stale.
// complete is false if the last read failed: a Pod without usage may then
// just not be known.
func (l *Latest) Usage(now time.Time) (map[checks.PodKey]checks.PodUsage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.fetched {
		if l.err != nil {
			return map[checks.PodKey]checks.PodUsage{}, false
		}
		return nil, false
	}
	stale := now.Sub(l.at) > l.MaxAge
	pods := make(map[checks.PodKey]checks.PodUsage, len(l.pods))
	for k, v := range l.pods {
		v.Stale = stale
		pods[k] = v
	}
	return pods, l.err == nil
}
