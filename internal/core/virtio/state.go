package virtio

import "fmt"

type QueueState struct {
	Size         uint16 `json:"size"`
	Ready        bool   `json:"ready"`
	DescAddr     uint64 `json:"desc_addr"`
	AvailAddr    uint64 `json:"avail_addr"`
	UsedAddr     uint64 `json:"used_addr"`
	LastAvailIdx uint16 `json:"last_avail_idx"`
	UsedIdx      uint16 `json:"used_idx"`
	NoNotify     bool   `json:"no_notify,omitempty"`
}

type BalloonPageWord struct {
	Index uint64 `json:"index"`
	Bits  uint64 `json:"bits"`
}

type MMIOState struct {
	DeviceFeatureSel  uint32            `json:"device_feature_sel"`
	DriverFeatureSel  uint32            `json:"driver_feature_sel"`
	DriverFeatures    uint64            `json:"driver_features"`
	QueueSel          uint32            `json:"queue_sel"`
	Status            uint32            `json:"status"`
	ConfigGeneration  uint32            `json:"config_generation"`
	SharedMemorySel   uint32            `json:"shared_memory_sel,omitempty"`
	Legacy            bool              `json:"legacy,omitempty"`
	Queues            []QueueState      `json:"queues,omitempty"`
	BackendPaths      []string          `json:"backend_paths,omitempty"`
	NumPages          uint32            `json:"num_pages,omitempty"`
	ActualPages       uint32            `json:"actual_pages,omitempty"`
	InflatedPageWords []BalloonPageWord `json:"inflated_page_words,omitempty"`
	// InflatedPages is retained for restoring snapshots written before the
	// compact bitmap representation was introduced.
	InflatedPages []uint64 `json:"inflated_pages,omitempty"`
}

func snapshotQueues(queues []queue) []QueueState {
	out := make([]QueueState, len(queues))
	for i := range queues {
		out[i] = snapshotQueue(queues[i])
	}
	return out
}

func snapshotQueue(q queue) QueueState {
	return QueueState{
		Size:         q.size,
		Ready:        q.ready,
		DescAddr:     q.descAddr,
		AvailAddr:    q.availAddr,
		UsedAddr:     q.usedAddr,
		LastAvailIdx: q.lastAvailIdx,
		UsedIdx:      q.usedIdx,
		NoNotify:     q.noNotify,
	}
}

func restoreQueues(queues []queue, states []QueueState, mem GuestMemory) error {
	for i := range queues {
		if i >= len(states) {
			queues[i] = queue{}
			continue
		}
		if err := restoreQueue(&queues[i], states[i], mem); err != nil {
			return fmt.Errorf("restore queue %d: %w", i, err)
		}
	}
	return nil
}

func restoreQueue(q *queue, state QueueState, mem GuestMemory) error {
	q.size = state.Size
	q.ready = state.Ready
	q.descAddr = state.DescAddr
	q.availAddr = state.AvailAddr
	q.usedAddr = state.UsedAddr
	q.lastAvailIdx = state.LastAvailIdx
	q.usedIdx = state.UsedIdx
	q.noNotify = state.NoNotify
	q.clearCache()
	if !q.ready || q.size == 0 || mem == nil {
		q.lastAvailIdx = 0
		q.usedIdx = 0
		return nil
	}
	return nil
}

func (c *Console) SnapshotState() MMIOState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return MMIOState{
		DeviceFeatureSel: c.deviceFeatureSel,
		DriverFeatureSel: c.driverFeatureSel,
		DriverFeatures:   c.driverFeatures,
		QueueSel:         c.queueSel,
		Status:           c.status,
		ConfigGeneration: c.configGeneration,
		Queues:           snapshotQueues(c.queues[:]),
	}
}

func (c *Console) RestoreState(state MMIOState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deviceFeatureSel = state.DeviceFeatureSel
	c.driverFeatureSel = state.DriverFeatureSel
	c.driverFeatures = state.DriverFeatures
	c.queueSel = state.QueueSel
	c.status = state.Status
	c.interruptStatus = 0
	c.irqHigh = false
	c.configGeneration = state.ConfigGeneration
	return restoreQueues(c.queues[:], state.Queues, c.mem)
}

func (r *RNG) SnapshotState() MMIOState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return MMIOState{
		DeviceFeatureSel: r.deviceFeatureSel,
		DriverFeatureSel: r.driverFeatureSel,
		DriverFeatures:   r.driverFeatures,
		QueueSel:         r.queueSel,
		Status:           r.status,
		ConfigGeneration: r.configGeneration,
		Queues:           []QueueState{snapshotQueue(r.queue)},
	}
}

func (r *RNG) RestoreState(state MMIOState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deviceFeatureSel = state.DeviceFeatureSel
	r.driverFeatureSel = state.DriverFeatureSel
	r.driverFeatures = state.DriverFeatures
	r.queueSel = state.QueueSel
	r.status = state.Status
	r.interruptStatus = 0
	r.irqHigh = false
	r.configGeneration = state.ConfigGeneration
	var queues [1]queue
	if err := restoreQueues(queues[:], state.Queues, r.mem); err != nil {
		return err
	}
	r.queue = queues[0]
	return nil
}

func (b *Balloon) SnapshotState() MMIOState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return MMIOState{
		DeviceFeatureSel:  b.deviceFeatureSel,
		DriverFeatureSel:  b.driverFeatureSel,
		DriverFeatures:    b.driverFeatures,
		QueueSel:          b.queueSel,
		Status:            b.status,
		ConfigGeneration:  b.configGeneration,
		Queues:            snapshotQueues(b.queues[:]),
		NumPages:          b.numPages,
		ActualPages:       b.actualPages,
		InflatedPageWords: b.inflatedPageWordsLocked(),
	}
}

func (b *Balloon) RestoreState(state MMIOState) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deviceFeatureSel = state.DeviceFeatureSel
	b.driverFeatureSel = state.DriverFeatureSel
	b.driverFeatures = state.DriverFeatures
	b.queueSel = state.QueueSel
	b.status = state.Status
	b.interruptStatus = 0
	b.irqHigh = false
	b.configGeneration = state.ConfigGeneration
	b.numPages = state.NumPages
	b.actualPages = state.ActualPages
	b.inflated = make(map[uint64]uint64, len(state.InflatedPageWords)+(len(state.InflatedPages)+63)/64)
	for _, word := range state.InflatedPageWords {
		if word.Bits != 0 {
			b.inflated[word.Index] = word.Bits
		}
	}
	if len(state.InflatedPageWords) == 0 {
		for _, pfn := range state.InflatedPages {
			b.markInflatedLocked(pfn)
		}
	}
	return restoreQueues(b.queues[:], state.Queues, b.mem)
}

func (v *Vsock) SnapshotState() MMIOState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return MMIOState{
		DeviceFeatureSel: v.deviceFeatureSel,
		DriverFeatureSel: v.driverFeatureSel,
		DriverFeatures:   v.driverFeatures,
		QueueSel:         v.queueSel,
		Status:           v.status,
		ConfigGeneration: v.configGeneration,
		Queues:           snapshotQueues(v.queues[:]),
	}
}

func (v *Vsock) RestoreState(state MMIOState) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deviceFeatureSel = state.DeviceFeatureSel
	v.driverFeatureSel = state.DriverFeatureSel
	v.driverFeatures = state.DriverFeatures
	v.queueSel = state.QueueSel
	v.status = state.Status
	v.interruptStatus = 0
	v.irqHigh = false
	v.configGeneration = state.ConfigGeneration
	v.connections = make(map[vsockConnKey]*vsockConnection)
	v.pendingRx = nil
	return restoreQueues(v.queues[:], state.Queues, v.mem)
}

func (f *FS) SnapshotState() MMIOState {
	f.mu.Lock()
	backend := f.filesystemBackend()
	state := MMIOState{
		DeviceFeatureSel: f.deviceFeatureSel,
		DriverFeatureSel: f.driverFeatureSel,
		DriverFeatures:   f.driverFeatures,
		QueueSel:         f.queueSel,
		Status:           f.status,
		ConfigGeneration: f.configGeneration,
		SharedMemorySel:  f.sharedMemorySel,
		Queues:           snapshotQueues(f.queues[:]),
	}
	f.mu.Unlock()
	if snapshotter, ok := backend.(interface{ SnapshotNodePaths() []string }); ok {
		state.BackendPaths = snapshotter.SnapshotNodePaths()
	}
	return state
}

func (f *FS) RestoreState(state MMIOState) error {
	f.mu.Lock()
	f.deviceFeatureSel = state.DeviceFeatureSel
	f.driverFeatureSel = state.DriverFeatureSel
	f.driverFeatures = state.DriverFeatures
	f.queueSel = state.QueueSel
	f.status = state.Status
	f.interruptStatus = 0
	f.irqHigh = false
	f.configGeneration = state.ConfigGeneration
	f.sharedMemorySel = state.SharedMemorySel
	if err := restoreQueues(f.queues[:], state.Queues, f.mem); err != nil {
		f.mu.Unlock()
		return err
	}
	backend := f.filesystemBackend()
	f.mu.Unlock()
	if restorer, ok := backend.(interface{ RestoreNodePaths([]string) error }); ok {
		if err := restorer.RestoreNodePaths(state.BackendPaths); err != nil {
			return err
		}
	}
	return nil
}

func (n *Net) SnapshotState() MMIOState {
	n.mu.Lock()
	defer n.mu.Unlock()
	return MMIOState{
		DeviceFeatureSel: n.deviceFeatureSel,
		DriverFeatureSel: n.driverFeatureSel,
		DriverFeatures:   n.driverFeatures,
		QueueSel:         n.queueSel,
		Status:           n.status,
		ConfigGeneration: n.configGeneration,
		Legacy:           n.legacy,
		Queues:           snapshotQueues(n.queues[:]),
	}
}

func (n *Net) RestoreState(state MMIOState) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deviceFeatureSel = state.DeviceFeatureSel
	n.driverFeatureSel = state.DriverFeatureSel
	n.driverFeatures = state.DriverFeatures
	n.queueSel = state.QueueSel
	n.status = state.Status
	n.interruptStatus = 0
	n.irqHigh = false
	n.configGeneration = state.ConfigGeneration
	n.legacy = state.Legacy
	n.pendingRx = nil
	return restoreQueues(n.queues[:], state.Queues, n.mem)
}
