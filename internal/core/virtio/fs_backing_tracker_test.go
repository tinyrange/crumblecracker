package virtio

import (
	"sync"
	"testing"
	"time"
)

type blockingUsageBackend struct {
	FSBackend
	mu        sync.Mutex
	current   uint64
	highWater uint64
	block     chan struct{}
	started   chan struct{}
}

func (b *blockingUsageBackend) BackingUsage() (uint64, uint64, uint64, error) {
	b.mu.Lock()
	current, highWater, block, started := b.current, b.highWater, b.block, b.started
	b.mu.Unlock()
	if block != nil {
		select {
		case <-started:
		default:
			close(started)
		}
		<-block
	}
	return current, max(current, highWater), current, nil
}

func (b *blockingUsageBackend) BackingMetadataUsage() (uint64, uint64) {
	return 17, 17
}

func TestFSBackingTrackerStatusSnapshotDoesNotWaitForSampler(t *testing.T) {
	backend := &blockingUsageBackend{current: 11}
	device := NewFS(0, 0, 0, "root", backend)
	tracker := AttachFSBackingUsageTracker([]*FS{device})
	backend.mu.Lock()
	backend.current = 23
	backend.block = make(chan struct{})
	backend.started = make(chan struct{})
	block, started := backend.block, backend.started
	backend.mu.Unlock()

	doneMutation := tracker.TrackMutation()
	doneMutation()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background telemetry sample did not start")
	}
	statusDone := make(chan FSBackingUsageSnapshot, 1)
	go func() { statusDone <- tracker.Snapshot() }()
	select {
	case snapshot := <-statusDone:
		if snapshot.DataBytes != 11 || snapshot.MetadataBytes != 17 || snapshot.CombinedBytes != 28 {
			t.Fatalf("last coherent snapshot = %+v", snapshot)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("status snapshot waited for blocked host storage")
	}
	close(block)
	deadline := time.Now().Add(time.Second)
	for tracker.Snapshot().DataBytes != 23 {
		if time.Now().After(deadline) {
			t.Fatal("completed background sample was not published")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFSBackingTrackerPublishesStaleSnapshotsDuringOverlappingMutations(t *testing.T) {
	backend := &blockingUsageBackend{current: 11}
	device := NewFS(0, 0, 0, "root", backend)
	tracker := AttachFSBackingUsageTracker([]*FS{device})
	doneMutation := tracker.TrackMutation()
	backend.mu.Lock()
	backend.current = 23
	backend.mu.Unlock()
	tracker.RequestSample()
	deadline := time.Now().Add(time.Second)
	for tracker.sampling.Load() || tracker.Snapshot().DataBytes != 23 {
		if time.Now().After(deadline) {
			t.Fatal("active-mutation sample was not published")
		}
		time.Sleep(time.Millisecond)
	}
	if snapshot := tracker.Snapshot(); !snapshot.Stale || snapshot.ActiveMutations != 1 {
		t.Fatalf("active mutation snapshot did not disclose in-flight state: %+v", snapshot)
	}
	doneMutation()
	deadline = time.Now().Add(time.Second)
	for snapshot := tracker.Snapshot(); snapshot.Stale || snapshot.ActiveMutations != 0; snapshot = tracker.Snapshot() {
		if time.Now().After(deadline) {
			t.Fatalf("quiescent mutation snapshot was not published: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFSBackingTrackerRetainsBackendPeakMissedBetweenSamples(t *testing.T) {
	backend := &blockingUsageBackend{current: 11, highWater: 97}
	device := NewFS(0, 0, 0, "root", backend)
	tracker := AttachFSBackingUsageTracker([]*FS{device})
	snapshot := tracker.Snapshot()
	if snapshot.DataBytes != 11 || snapshot.DataHighWaterBytes != 97 || snapshot.CombinedHighWaterBytes < 97 {
		t.Fatalf("transient backend peak was not retained: %+v", snapshot)
	}
}

func TestFSBackingTrackerDoesNotPromoteStaleAggregatePeak(t *testing.T) {
	backend := &blockingUsageBackend{current: 11}
	device := NewFS(0, 0, 0, "root", backend)
	tracker := AttachFSBackingUsageTracker([]*FS{device})
	doneMutation := tracker.TrackMutation()
	defer doneMutation()
	backend.mu.Lock()
	backend.current = 100
	backend.highWater = 100
	backend.mu.Unlock()
	tracker.RequestSample()
	deadline := time.Now().Add(time.Second)
	for snapshot := tracker.Snapshot(); snapshot.DataBytes != 100 || !snapshot.Stale; snapshot = tracker.Snapshot() {
		if time.Now().After(deadline) {
			t.Fatalf("stale mutation sample was not published: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	snapshot := tracker.Snapshot()
	if snapshot.CombinedBytes != 117 {
		t.Fatalf("stale current aggregate = %d, want 117", snapshot.CombinedBytes)
	}
	if snapshot.CombinedHighWaterBytes != 100 {
		t.Fatalf("stale sample promoted aggregate high-water to %d, want component lower bound 100", snapshot.CombinedHighWaterBytes)
	}
}
