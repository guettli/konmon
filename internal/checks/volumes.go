package checks

import "time"

// VolumeKey names a PersistentVolumeClaim.
type VolumeKey struct {
	Namespace string
	PVC       string
}

// VolumeStats is the usage of the volume of a PVC as the kubelet of the
// Node which mounts it reports it. Fields the kubelet does not report are
// nil.
type VolumeStats struct {
	// Node, Pod and Volume (the name in the Pod spec) of the measurement.
	Node, Pod, Volume string
	Time              time.Time
	UsedBytes         *uint64
	CapacityBytes     *uint64
	AvailableBytes    *uint64
	Inodes            *uint64
	InodesUsed        *uint64
	InodesFree        *uint64
	// Stale: the kubelet could not be asked for a while; these are the
	// last known numbers.
	Stale bool
}

// SetVolumeStats gives checks which use "volume-stats" the usage of
// volumes. complete: every kubelet was asked successfully, so a PVC without
// stats is not mounted. Without SetVolumeStats, or for a PVC without stats
// if not complete, or for stale stats, ctx.volumeStats throws and the
// previous findings stay: not known is not the same as not mounted.
func (s *Snapshot) SetVolumeStats(stats map[VolumeKey]VolumeStats, complete bool) {
	if stats == nil {
		stats = map[VolumeKey]VolumeStats{}
	}
	s.volumes = stats
	s.volumesComplete = complete
}
