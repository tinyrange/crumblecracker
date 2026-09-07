package virtio

import (
	"errors"
	"sync"
	"sync/atomic"
)

// FSBackingUsageTracker observes the sum of a VM's virtio-fs backing stores at
// request boundaries. Component peaks cannot be combined after the fact: only
// observing the shared current sum preserves a real aggregate high-water mark.
type FSBackingUsageTracker struct {
	sampleMu           sync.Mutex
	mutationMu         sync.Mutex
	activeMutations    uint64
	mutationGeneration uint64
	mu                 sync.Mutex
	backends           []FSBackend
	current            uint64
	highWater          uint64
	metadataCurrent    uint64
	metadataHighWater  uint64
	combinedHighWater  uint64
	physical           uint64
	reclaimErr         error
	stale              bool
	active             atomic.Uint64
	sampling           atomic.Bool
	dirty              atomic.Bool
}

func AttachFSBackingUsageTracker(devices []*FS) *FSBackingUsageTracker {
	tracker := &FSBackingUsageTracker{}
	for _, device := range devices {
		if device == nil {
			continue
		}
		device.mu.Lock()
		tracker.backends = append(tracker.backends, device.filesystemBackend())
		device.filesystem.backingUsageTracker = tracker
		device.mu.Unlock()
	}
	tracker.Sample()
	return tracker
}

func SharedFSBackingUsageTracker(devices []*FS) *FSBackingUsageTracker {
	var tracker *FSBackingUsageTracker
	found := false
	for _, device := range devices {
		if device == nil {
			continue
		}
		found = true
		current := device.BackingUsageTracker()
		if current == nil {
			return nil
		}
		if tracker == nil {
			tracker = current
		} else if tracker != current {
			return nil
		}
	}
	if !found {
		return nil
	}
	return tracker
}

func (t *FSBackingUsageTracker) Sample() {
	if t == nil {
		return
	}
	t.sampleMu.Lock()
	t.sampleStable()
	t.sampleMu.Unlock()
}

func (t *FSBackingUsageTracker) sampleStable() {
	t.mutationMu.Lock()
	generation := t.mutationGeneration
	active := t.activeMutations
	t.mutationMu.Unlock()

	var current, dataPeak, metadata, metadataPeak, physical uint64
	var errs []error
	for _, backend := range t.backends {
		if provider, ok := backend.(interface {
			BackingUsage() (uint64, uint64, uint64, error)
		}); ok {
			value, backendPeak, backendPhysical, err := provider.BackingUsage()
			current += value
			dataPeak = max(dataPeak, backendPeak)
			physical += backendPhysical
			if err != nil {
				errs = append(errs, err)
			}
		} else if provider, ok := backend.(interface{ BackingCurrent() uint64 }); ok {
			current += provider.BackingCurrent()
		}
		if provider, ok := backend.(interface{ BackingMetadataUsage() (uint64, uint64) }); ok {
			value, backendPeak := provider.BackingMetadataUsage()
			metadata += value
			metadataPeak = max(metadataPeak, backendPeak)
		}
	}
	t.mutationMu.Lock()
	stale := active != 0 || t.activeMutations != 0 || t.mutationGeneration != generation
	t.mu.Lock()
	t.current = current
	t.metadataCurrent = metadata
	t.physical = physical
	t.reclaimErr = errors.Join(errs...)
	t.stale = stale
	t.highWater = max(t.highWater, current, dataPeak)
	t.metadataHighWater = max(t.metadataHighWater, metadata, metadataPeak)
	// Only a quiescent sample proves that the independently queried data and
	// metadata values existed together. During a mutation the current values
	// remain useful when disclosed as stale, but their sum must not become a
	// permanent fictional peak.
	if !stale {
		combined := current + metadata
		if combined < current {
			combined = ^uint64(0)
		}
		t.combinedHighWater = max(t.combinedHighWater, combined)
	}
	// A component's independently recorded peak is a safe lower bound for
	// aggregate pressure: every other component was non-negative when that
	// peak occurred. This preserves transient peaks even when overlapping
	// mutations allocate and release between aggregate samples without
	// inventing a sum of peaks that may have happened at different times.
	t.combinedHighWater = max(t.combinedHighWater, dataPeak, metadataPeak)
	t.mu.Unlock()
	t.mutationMu.Unlock()
}

// TrackMutation samples after a filesystem mutation without holding telemetry
// ownership across arbitrary backend I/O. The sampler itself is serialized,
// while status readers use the last complete immutable snapshot.
func (t *FSBackingUsageTracker) TrackMutation() func() {
	if t == nil {
		return func() {}
	}
	t.mutationMu.Lock()
	t.activeMutations++
	t.active.Store(t.activeMutations)
	t.mutationGeneration++
	t.mutationMu.Unlock()
	t.RequestSample()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mutationMu.Lock()
			t.activeMutations--
			t.active.Store(t.activeMutations)
			t.mutationGeneration++
			t.mutationMu.Unlock()
			t.RequestSample()
		})
	}
}

func (t *FSBackingUsageTracker) RequestSample() {
	if t == nil {
		return
	}
	t.dirty.Store(true)
	if !t.sampling.CompareAndSwap(false, true) {
		return
	}
	go t.runSampler()
}

func (t *FSBackingUsageTracker) runSampler() {
	for {
		t.dirty.Store(false)
		t.Sample()
		if t.dirty.Load() {
			continue
		}
		t.sampling.Store(false)
		if !t.dirty.Load() || !t.sampling.CompareAndSwap(false, true) {
			return
		}
	}
}

func (t *FSBackingUsageTracker) Usage() (uint64, uint64) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.current, t.highWater
}

func (t *FSBackingUsageTracker) MetadataUsage() (uint64, uint64) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.metadataCurrent, t.metadataHighWater
}

func (t *FSBackingUsageTracker) CombinedUsage() (uint64, uint64) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	combined := t.current + t.metadataCurrent
	if combined < t.current {
		combined = ^uint64(0)
	}
	return combined, t.combinedHighWater
}

type FSBackingUsageSnapshot struct {
	DataBytes              uint64
	DataHighWaterBytes     uint64
	MetadataBytes          uint64
	MetadataHighWaterBytes uint64
	CombinedBytes          uint64
	CombinedHighWaterBytes uint64
	PhysicalBytes          uint64
	ReclaimError           error
	Stale                  bool
	ActiveMutations        uint64
}

func (t *FSBackingUsageTracker) Snapshot() FSBackingUsageSnapshot {
	if t == nil {
		return FSBackingUsageSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	combined := t.current + t.metadataCurrent
	if combined < t.current {
		combined = ^uint64(0)
	}
	return FSBackingUsageSnapshot{
		DataBytes:              t.current,
		DataHighWaterBytes:     t.highWater,
		MetadataBytes:          t.metadataCurrent,
		MetadataHighWaterBytes: t.metadataHighWater,
		CombinedBytes:          combined,
		CombinedHighWaterBytes: t.combinedHighWater,
		PhysicalBytes:          t.physical,
		ReclaimError:           t.reclaimErr,
		Stale:                  t.stale || t.active.Load() != 0 || t.sampling.Load() || t.dirty.Load(),
		ActiveMutations:        t.active.Load(),
	}
}
