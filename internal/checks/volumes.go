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
}

// SetVolumeStats gives checks which use "volume-stats" the usage of
// volumes. Without it, ctx.volumeStats throws: the stats are not known, which
// is not the same as a PVC which is not mounted.
func (s *Snapshot) SetVolumeStats(stats map[VolumeKey]VolumeStats) {
	if stats == nil {
		stats = map[VolumeKey]VolumeStats{}
	}
	s.volumes = stats
}
