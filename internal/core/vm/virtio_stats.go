package vm

import (
	"errors"
	"fmt"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func virtioFSStats(fsdevs []*virtio.FS) []virtio.FSStats {
	if len(fsdevs) == 0 {
		return nil
	}
	out := make([]virtio.FSStats, 0, len(fsdevs))
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		out = append(out, fsdev.Stats())
	}
	return out
}

func virtioFSBackingUsage(fsdevs []*virtio.FS) (current, highWater, physical uint64, err error) {
	var errs []error
	var tracker *virtio.FSBackingUsageTracker
	sharedTracker := true
	devices := 0
	for i, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		devices++
		deviceCurrent, deviceHighWater, devicePhysical, deviceErr := fsdev.BackingUsage()
		deviceTracker := fsdev.BackingUsageTracker()
		if deviceTracker == nil {
			sharedTracker = false
		} else if tracker == nil {
			tracker = deviceTracker
		} else if deviceTracker != tracker {
			sharedTracker = false
		}
		current += deviceCurrent
		if tracker == nil || !sharedTracker {
			highWater = max(highWater, deviceHighWater)
		}
		physical += devicePhysical
		if deviceErr != nil {
			errs = append(errs, fmt.Errorf("virtio-fs device %d: %w", i, deviceErr))
		}
	}
	if devices != 0 && tracker != nil && sharedTracker {
		current, highWater = tracker.Usage()
	} else {
		highWater = max(highWater, current)
	}
	return current, highWater, physical, errors.Join(errs...)
}

func virtioFSBackingMetadataUsage(fsdevs []*virtio.FS) (current, highWater uint64) {
	var tracker *virtio.FSBackingUsageTracker
	sharedTracker := true
	devices := 0
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		devices++
		deviceTracker := fsdev.BackingUsageTracker()
		if deviceTracker == nil {
			sharedTracker = false
		} else if tracker == nil {
			tracker = deviceTracker
		} else if deviceTracker != tracker {
			sharedTracker = false
		}
		deviceCurrent, deviceHighWater := fsdev.BackingMetadataUsage()
		current += deviceCurrent
		highWater = max(highWater, deviceHighWater)
	}
	if devices != 0 && tracker != nil && sharedTracker {
		return tracker.MetadataUsage()
	}
	highWater = max(highWater, current)
	return current, highWater
}

func virtioFSBackingCombinedUsage(fsdevs []*virtio.FS) (current, highWater uint64) {
	var tracker *virtio.FSBackingUsageTracker
	sharedTracker := true
	devices := 0
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		devices++
		deviceTracker := fsdev.BackingUsageTracker()
		if deviceTracker == nil {
			sharedTracker = false
		} else if tracker == nil {
			tracker = deviceTracker
		} else if deviceTracker != tracker {
			sharedTracker = false
		}
	}
	if devices != 0 && tracker != nil && sharedTracker {
		return tracker.CombinedUsage()
	}
	data, dataHigh, _, _ := virtioFSBackingUsage(fsdevs)
	metadata, metadataHigh := virtioFSBackingMetadataUsage(fsdevs)
	current = saturatingUint64Add(data, metadata)
	return current, max(current, dataHigh, metadataHigh)
}

func virtioFSBackingSnapshot(fsdevs []*virtio.FS) virtio.FSBackingUsageSnapshot {
	if tracker := virtio.SharedFSBackingUsageTracker(fsdevs); tracker != nil {
		return tracker.Snapshot()
	}
	data, dataHigh, physical, err := virtioFSBackingUsage(fsdevs)
	metadata, metadataHigh := virtioFSBackingMetadataUsage(fsdevs)
	combined := saturatingUint64Add(data, metadata)
	return virtio.FSBackingUsageSnapshot{
		DataBytes: data, DataHighWaterBytes: dataHigh,
		MetadataBytes: metadata, MetadataHighWaterBytes: metadataHigh,
		CombinedBytes: combined, CombinedHighWaterBytes: max(combined, dataHigh, metadataHigh),
		PhysicalBytes: physical, ReclaimError: err,
	}
}

func virtioFSPersistentStatus(fsdevs []*virtio.FS) []virtio.PersistentFSStatus {
	var statuses []virtio.PersistentFSStatus
	for _, device := range fsdevs {
		if device != nil {
			statuses = append(statuses, device.PersistentFSStatus()...)
		}
	}
	return statuses
}
