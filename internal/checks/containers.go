package checks

import "time"

// PodKey names a Pod.
type PodKey struct {
	Namespace string
	Pod       string
}

// PodUsage is the use of the containers of one Pod as the metrics API
// (metrics-server) reports it.
type PodUsage struct {
	Namespace, Pod string
	// Time of the measurement (the end of its window).
	Time time.Time
	// Containers by name.
	Containers map[string]ContainerUsage
	// Stale: the metrics API could not be read for a while; these are the
	// last known numbers.
	Stale bool
}

// ContainerUsage is the use of one container.
type ContainerUsage struct {
	CPU    float64 // cores
	Memory int64   // bytes, working set
}

// SetContainerUsage gives checks which use "container-usage" the use of
// the containers. complete: the metrics API was read successfully, so a
// Pod without usage is not measured (yet). Without SetContainerUsage, for a
// Pod without usage if not complete, or for stale usage, ctx.containerUsage
// throws and the previous findings stay.
func (s *Snapshot) SetContainerUsage(usage map[PodKey]PodUsage, complete bool) {
	if usage == nil {
		usage = map[PodKey]PodUsage{}
	}
	s.containers = usage
	s.containersComplete = complete
}
