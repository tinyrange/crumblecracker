package vm

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

type aggregateUsageBackend struct {
	virtio.FSBackend
	current, highWater          uint64
	metadata, metadataHighWater uint64
}

func (b *aggregateUsageBackend) BackingUsage() (uint64, uint64, uint64, error) {
	return b.current, b.highWater, b.current, nil
}

func (b *aggregateUsageBackend) BackingCurrent() uint64 { return b.current }

func (b *aggregateUsageBackend) BackingMetadataUsage() (uint64, uint64) {
	return b.metadata, b.metadataHighWater
}

func TestVirtioFSBackingUsageTracksAggregateMutationPeaks(t *testing.T) {
	one := &aggregateUsageBackend{current: 2, highWater: 100, metadata: 7, metadataHighWater: 70}
	two := &aggregateUsageBackend{current: 3, highWater: 200, metadata: 11, metadataHighWater: 110}
	devices := []*virtio.FS{
		virtio.NewFS(0, 0, 0, "one", one),
		virtio.NewFS(0, 0, 0, "two", two),
	}
	tracker := virtio.AttachFSBackingUsageTracker(devices)
	one.current, two.current = 60, 70
	tracker.Sample()
	one.metadata, two.metadata = 70, 110
	tracker.Sample()
	one.current, two.current = 2, 3
	one.metadata, two.metadata = 7, 11
	tracker.Sample()
	current, highWater, _, err := virtioFSBackingUsage(devices)
	if err != nil || current != 5 || highWater != 200 {
		t.Fatalf("aggregate data usage current=%d high-water=%d err=%v", current, highWater, err)
	}
	metadata, metadataHighWater := virtioFSBackingMetadataUsage(devices)
	if metadata != 18 || metadataHighWater != 180 {
		t.Fatalf("aggregate metadata usage current=%d high-water=%d", metadata, metadataHighWater)
	}
	combined, combinedHighWater := virtioFSBackingCombinedUsage(devices)
	if combined != 23 || combinedHighWater != 310 {
		t.Fatalf("aggregate combined usage current=%d high-water=%d", combined, combinedHighWater)
	}
}
