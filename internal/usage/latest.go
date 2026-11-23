package usage

import (
	"sync"
	"time"

	"github.com/guettli/konmon/internal/checks"
)

// forget: a Pod which the metrics API has not listed for this long is
// dropped; until then its last usage is kept (stale after MaxAge), so a Pod
// which metrics-server skips for a while (its Node could not be scraped)
// keeps its findings instead of resolving them.
const forget = time.Hour

// Latest keeps the last use of the containers of every Pod, for checks
// which use "container-usage". It is safe for concurrent use.
type Latest struct {
	// MaxAge: usage which was not read for this long (the metrics API
	// fails, or it does not list the Pod) is stale.
	MaxAge time.Duration

	mu          sync.Mutex
	pods        map[checks.PodKey]entry
	err         error
	unavailable bool
	fetched     bool
}

type entry struct {
	usage checks.PodUsage
	seen  time.Time
}

// Set records a successful read at now. Pods which are not in list keep
// their last usage for a while (see forget).
func (l *Latest) Set(list []checks.PodUsage, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pods == nil {
		l.pods = map[checks.PodKey]entry{}
	}
	for k, e := range l.pods {
		if now.Sub(e.seen) > forget {
			delete(l.pods, k)
		}
	}
	for _, pu := range list {
		l.pods[checks.PodKey{Namespace: pu.Namespace, Pod: pu.Pod}] = entry{usage: pu, seen: now}
	}
	l.err, l.unavailable, l.fetched = nil, false, true
}

// Failed records that the metrics API could not be read: the last usage is
// kept, and it becomes stale after MaxAge.
func (l *Latest) Failed(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

// Unavailable records that the cluster does not serve the metrics API (no
// metrics-server): there is no usage to check, which is not a failure.
func (l *Latest) Unavailable() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pods, l.err, l.unavailable = nil, nil, true
}

// Err is the error of the last read; nil if it worked, if the metrics API
// is not served, or if nothing was read yet.
func (l *Latest) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Usage returns the usage of all Pods; nil before the first read (an
// empty map if it failed or the metrics API is not served). Usage which was
// not read for MaxAge is marked Stale. complete is false if the last read
// failed: a Pod without usage may then just not be known.
func (l *Latest) Usage(now time.Time) (map[checks.PodKey]checks.PodUsage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.unavailable {
		return map[checks.PodKey]checks.PodUsage{}, true
	}
	if !l.fetched {
		if l.err != nil {
			return map[checks.PodKey]checks.PodUsage{}, false
		}
		return nil, false
	}
	pods := make(map[checks.PodKey]checks.PodUsage, len(l.pods))
	for k, e := range l.pods {
		pu := e.usage
		pu.Stale = now.Sub(e.seen) > l.MaxAge
		pods[k] = pu
	}
	return pods, l.err == nil
}
