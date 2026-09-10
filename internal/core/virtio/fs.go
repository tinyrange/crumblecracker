package virtio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	mmioDeviceIDFS = 26

	fsQueueHiprio       = 0
	fsControlQueueCount = 1
	fsQueueRequest      = fsQueueHiprio + fsControlQueueCount
	fsRequestQueueCount = 4
	fsQueueCount        = fsControlQueueCount + fsRequestQueueCount

	fsCfgTagSize           = 36
	fsCfgNumQueueOff       = fsCfgTagSize
	fsCfgTotalSize         = fsCfgTagSize + 4
	fsInterruptVring       = 0x1
	fsAvailNoInterrupt     = 0x1
	virtioStatusFeaturesOK = 0x8
	featureRingEventIdx    = uint64(1) << 29
	fuseInHeaderSize       = 40
	fuseOutHeaderSize      = 16
	fuseAttrSize           = 88
	fuseEntryOutSize       = 40 + fuseAttrSize
	fuseAttrOutSize        = 16 + fuseAttrSize
	fuseOpenOutSize        = 16
	fuseInitOutSize        = 40
	fuseStatfsOutSize      = 80
	fuseStatxOutSize       = 288
	fuseDirentBaseSize     = 24
	fuseDirentPlusBaseSize = fuseEntryOutSize + fuseDirentBaseSize
	fuseWriteOutSize       = 8
	fuseLKInSize           = 48
	fuseLKOutSize          = 24
	linuxFUnlck            = 2
	fuseFlushLockOwner     = 1 << 1
	fuseReleaseFlockUnlock = 1 << 1
)

const (
	fuseLookup      = 1
	fuseForget      = 2
	fuseGetAttr     = 3
	fuseSetAttr     = 4
	fuseReadlink    = 5
	fuseSymlink     = 6
	fuseMknod       = 8
	fuseMkdir       = 9
	fuseUnlink      = 10
	fuseRmDir       = 11
	fuseRename      = 12
	fuseLink        = 13
	fuseOpen        = 14
	fuseRead        = 15
	fuseWrite       = 16
	fuseStatfs      = 17
	fuseRelease     = 18
	fuseFsync       = 20
	fuseSetXattr    = 21
	fuseGetXattr    = 22
	fuseListXattr   = 23
	fuseRemoveXattr = 24
	fuseFlush       = 25
	fuseInit        = 26
	fuseOpenDir     = 27
	fuseReadDir     = 28
	fuseReleaseDir  = 29
	fuseFsyncDir    = 30
	fuseGetLK       = 31
	fuseSetLK       = 32
	fuseSetLKW      = 33
	fuseAccess      = 34
	fuseCreate      = 35
	fuseDestroy     = 38
	fuseIoctl       = 39
	fusePoll        = 40
	fuseReadDirPlus = 44
	fuseRename2     = 45
	fuseLseek       = 46
	fuseSyncFS      = 50
	fuseTmpfile     = 51
	fuseStatx       = 52
)

const (
	fattrMode     = 1 << 0
	fattrUID      = 1 << 1
	fattrGID      = 1 << 2
	fattrSize     = 1 << 3
	fattrATime    = 1 << 4
	fattrMTime    = 1 << 5
	fattrFH       = 1 << 6
	fattrATimeNow = 1 << 7
	fattrMTimeNow = 1 << 8
)

const (
	linuxOACCMODE = 3
	linuxORDONLY  = 0
	linuxOWRONLY  = 1
	linuxORDWR    = 2
	linuxOCREAT   = 0x40
	linuxOEXCL    = 0x80
	linuxOTRUNC   = 0x200
	linuxOAPPEND  = 0x400
)

const (
	linuxRenameNoReplace = 1
	linuxRenameExchange  = 2
)

const (
	fuseCapBigWrites      = 1 << 5
	fuseCapDoReadDirPlus  = 1 << 13
	fuseCapWritebackCache = 1 << 16
	fuseCapMaxPages       = 1 << 22
)

const (
	fuseOpenKeepCache = 1 << 1
	fuseOpenCacheDir  = 1 << 3
	fuseOpenNoFlush   = 1 << 5
)

const (
	fsCacheStrict     = "strict"
	fsCacheNormal     = "normal"
	fsCacheAggressive = "aggressive"
)

const (
	statxBasicStats = 0x000007ff
)

const (
	dirTypeUnknown = 0
	dirTypeFIFO    = 1
	dirTypeChar    = 2
	dirTypeDir     = 4
	dirTypeBlock   = 6
	dirTypeFile    = 8
	dirTypeLink    = 10
	dirTypeSocket  = 12
)

type FS struct {
	Base           uint64
	Size           uint64
	IRQ            uint32
	Log            io.Writer
	Strict         bool
	Async          bool
	RecordTiming   func(name string, duration time.Duration)
	cacheMode      string
	writebackCache bool
	directMemory   bool
	eventIdx       bool
	kickPoll       bool
	kickPollIdle   time.Duration
	kickPollSleep  time.Duration
	kickPollActive bool
	workerCount    int
	entryTTL       time.Duration
	attrTTL        time.Duration

	mu               sync.Mutex
	workerOnce       sync.Once
	mem              GuestMemory
	irq              IRQController
	dispatcher       fsRequestDispatcher
	filesystem       *fuseServer
	tag              [fsCfgTagSize]byte
	deviceFeatureSel uint32
	driverFeatureSel uint32
	driverFeatures   uint64
	sharedMemorySel  uint32
	queueSel         uint32
	status           uint32
	interruptStatus  uint32
	irqHigh          bool
	configGeneration uint32
	queues           []queue
	mmioReads        uint64
	mmioWrites       uint64
	queueNotifies    []uint64
	kickPollLoops    uint64
	kickPollHits     uint64
	kickPollMisses   uint64
	kickPollWorks    uint64
	fuseRequests     atomic.Uint64
	interruptRaises  uint64
	irqTransitions   uint64
	closeStart       sync.Once
	cleanupOnce      sync.Once
	closeMu          sync.Mutex
	backendClosed    bool
	backendCloseErr  error
	backendCloseDone chan struct{}
	beginCloseDone   chan struct{}
	closed           chan struct{}
	workersDone      chan struct{}
	workerWG         sync.WaitGroup
	kickPollWG       sync.WaitGroup
	workCh           chan *fsWork
	nextWorkSeq      []uint64
	nextCompleteSeq  []uint64
	completions      map[fsCompletionKey]fsCompletion
	fuseOpStats      [fuseStatsSlots]fuseOpStat
	stageStats       [fsStageCount]timingStat
	scratch16        [16]byte
	scratch8         [8]byte
	scratch4         [4]byte
	scratch2         [2]byte
}

type CloseIncompleteError struct {
	Resource string
	Timeout  time.Duration
}

func (e *CloseIncompleteError) Error() string {
	return fmt.Sprintf("close %s: work did not stop within %s; resources remain quarantined for retry", e.Resource, e.Timeout)
}

const fuseStatsSlots = 64
const fsStageCount = 4
const fsInlineRespDescs = 32
const fsPooledReqSize = 4096
const fsWorkQueueDepth = 128

const (
	fsStageQueueHarvest = iota
	fsStageInlineDispatch
	fsStageInlineComplete
	fsStageAsyncComplete
)

type fsWork struct {
	qidx       int
	head       uint16
	seq        uint64
	generation uint32
	opcode     uint32
	req        []byte
	pooledReq  bool
	respCount  int
	respDescs  [fsInlineRespDescs]fsDesc
	respExtra  []fsDesc
}

func (f *FS) filesystemBackend() FSBackend {
	if f == nil || f.filesystem == nil {
		return nil
	}
	return f.filesystem.backend
}

func (f *FS) usageTracker() *FSBackingUsageTracker {
	if f == nil || f.filesystem == nil {
		return nil
	}
	return f.filesystem.backingUsageTracker
}

type fsCompletionKey struct {
	qidx int
	seq  uint64
}

type fsCompletion struct {
	work  fsWork
	reply fsReply
	err   error
}

type fsInlineCompletion struct {
	work  fsWork
	reply fsReply
	err   error
}

var fsReqPool = sync.Pool{
	New: func() any {
		return make([]byte, fsPooledReqSize)
	},
}

type FSStats struct {
	Tag                           string        `json:"tag"`
	Async                         bool          `json:"async"`
	WorkerCount                   int           `json:"worker_count"`
	CacheMode                     string        `json:"cache_mode"`
	WritebackCache                bool          `json:"writeback_cache"`
	EventIdx                      bool          `json:"event_idx"`
	MMIOReads                     uint64        `json:"mmio_reads"`
	MMIOWrites                    uint64        `json:"mmio_writes"`
	QueueNotifies                 []uint64      `json:"queue_notifies"`
	KickPollLoops                 uint64        `json:"kick_poll_loops"`
	KickPollHits                  uint64        `json:"kick_poll_hits"`
	KickPollMisses                uint64        `json:"kick_poll_misses"`
	KickPollWorks                 uint64        `json:"kick_poll_works"`
	FUSERequests                  uint64        `json:"fuse_requests"`
	InterruptRaises               uint64        `json:"interrupt_raises"`
	IRQTransitions                uint64        `json:"irq_transitions"`
	IRQHigh                       bool          `json:"irq_high"`
	InterruptStatus               uint32        `json:"interrupt_status"`
	QueueReady                    []bool        `json:"queue_ready"`
	QueueLastAvail                []uint16      `json:"queue_last_avail"`
	QueueAvailIdx                 []uint16      `json:"queue_avail_idx"`
	QueueUsedIdx                  []uint16      `json:"queue_used_idx"`
	QueueNoNotify                 []bool        `json:"queue_no_notify"`
	FUSEOps                       []FUSEOpStats `json:"fuse_ops"`
	Stages                        []TimingStats `json:"stages"`
	BackingBytes                  uint64        `json:"backing_bytes,omitempty"`
	BackingHighWaterBytes         uint64        `json:"backing_high_water_bytes,omitempty"`
	BackingPhysicalBytes          uint64        `json:"backing_physical_bytes,omitempty"`
	BackingMetadataBytes          uint64        `json:"backing_metadata_bytes,omitempty"`
	BackingMetadataHighWaterBytes uint64        `json:"backing_metadata_high_water_bytes,omitempty"`
	BackingReclaimError           string        `json:"backing_reclaim_error,omitempty"`
}

type FUSEOpStats struct {
	Opcode       uint32 `json:"opcode"`
	Name         string `json:"name"`
	Count        uint64 `json:"count"`
	TotalNanos   int64  `json:"total_nanos"`
	MaxNanos     int64  `json:"max_nanos"`
	AverageNanos int64  `json:"average_nanos"`
}

type fuseOpStat struct {
	timingStat
}

type TimingStats struct {
	Name         string `json:"name"`
	Count        uint64 `json:"count"`
	TotalNanos   int64  `json:"total_nanos"`
	MaxNanos     int64  `json:"max_nanos"`
	AverageNanos int64  `json:"average_nanos"`
}

type timingStat struct {
	count      atomic.Uint64
	totalNanos atomic.Int64
	maxNanos   atomic.Int64
}

type fsDesc struct {
	addr   uint64
	length uint32
	flags  uint16
	next   uint16
	write  bool
}

type guestMemorySlicer interface {
	SliceIPA(addr uint64, size int) ([]byte, error)
}

func NewFS(base, size uint64, irq uint32, tag string, backend FSBackend) *FS {
	cacheMode, entryTTL, attrTTL := resolveFSCachePolicy()
	if backend == nil {
		backend = NewPassthroughFS("", nil)
	}
	fs := &FS{
		Base:           base,
		Size:           size,
		IRQ:            irq,
		Async:          resolveVirtioFSAsync(),
		cacheMode:      cacheMode,
		writebackCache: strings.TrimSpace(os.Getenv("CCX3_VIRTIOFS_WRITEBACK")) != "",
		directMemory:   resolveVirtioFSDirectMemory(),
		eventIdx:       resolveVirtioFSEventIdx(),
		kickPoll:       resolveVirtioFSKickPoll(),
		kickPollIdle:   resolveVirtioFSKickPollDuration("CCX3_VIRTIOFS_KICK_POLL_IDLE", 500*time.Microsecond),
		kickPollSleep:  resolveVirtioFSKickPollDuration("CCX3_VIRTIOFS_KICK_POLL_SLEEP", 5*time.Microsecond),
		workerCount:    resolveVirtioFSWorkerCount(),
		entryTTL:       entryTTL,
		attrTTL:        attrTTL,
		closed:         make(chan struct{}),
		// A virtqueue can expose at most 128 heads in one notification. Queue
		// pointers so an idle device does not reserve the large inline descriptor
		// array in every fsWork slot.
		workCh:      make(chan *fsWork, fsWorkQueueDepth),
		completions: make(map[fsCompletionKey]fsCompletion),
	}
	fs.resetQueueStateLocked()
	fs.filesystem = &fuseServer{device: fs, backend: backend, locks: newFuseLockManager()}
	fs.dispatcher = fs.filesystem
	if be, ok := backend.(fsWritebackCacheBackend); ok {
		be.SetWritebackCache(fs.writebackCache)
	}
	copy(fs.tag[:], []byte(tag))
	fs.resetLocked()
	return fs
}

func (f *FS) Close() error {
	return f.closeWithin(2 * time.Second)
}

func (f *FS) closeWithin(timeout time.Duration) error {
	if f == nil {
		return nil
	}
	started := time.Now()
	f.closeStart.Do(func() {
		close(f.closed)
		if f.filesystem != nil && f.filesystem.locks != nil {
			f.filesystem.locks.close()
		}
		// Prevent a concurrent enqueue from adding workers after the wait starts.
		f.workerOnce.Do(func() {})
		f.mu.Lock()
		f.kickPoll = false
		f.kickPollActive = false
		f.configGeneration++
		f.mu.Unlock()
		f.beginCloseDone = make(chan struct{})
		go func() {
			if starter, ok := f.filesystemBackend().(interface{ BeginClose() }); ok {
				starter.BeginClose()
			}
			close(f.beginCloseDone)
		}()
		f.workersDone = make(chan struct{})
		go func() {
			f.workerWG.Wait()
			f.kickPollWG.Wait()
			close(f.workersDone)
		}()
	})
	waitWithin := func(done <-chan struct{}, resource string) error {
		remaining := timeout - time.Since(started)
		if remaining <= 0 {
			return &CloseIncompleteError{Resource: resource, Timeout: timeout}
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-done:
			return nil
		case <-timer.C:
			return &CloseIncompleteError{Resource: resource, Timeout: timeout}
		}
	}
	if err := waitWithin(f.beginCloseDone, "virtio-fs backend shutdown signal"); err != nil {
		return err
	}
	if err := waitWithin(f.workersDone, "virtio-fs workers"); err != nil {
		return err
	}
	f.cleanupOnce.Do(func() {
		for {
			select {
			case work := <-f.workCh:
				putFSReqBuffer(work.req, work.pooledReq)
			default:
				f.mu.Lock()
				irq := f.irq
				f.irq = nil
				f.mem = nil
				f.clearQueueCachesLocked()
				f.interruptStatus = 0
				f.irqHigh = false
				f.mu.Unlock()
				if irq != nil {
					_ = irq.SetIRQ(f.IRQ, false)
				}
				return
			}
		}
	})
	remaining := timeout - time.Since(started)
	if remaining <= 0 {
		return &CloseIncompleteError{Resource: "virtio-fs backend", Timeout: timeout}
	}
	return f.closeBackendWithin(remaining)
}

func (f *FS) closeBackendWithin(timeout time.Duration) error {
	f.closeMu.Lock()
	if f.backendClosed {
		err := f.backendCloseErr
		f.closeMu.Unlock()
		return err
	}
	if f.backendCloseDone == nil {
		f.backendCloseDone = make(chan struct{})
		done := f.backendCloseDone
		go func() {
			var err error
			if closer, ok := f.filesystemBackend().(interface{ Close() error }); ok {
				err = closer.Close()
			}
			f.closeMu.Lock()
			f.backendCloseErr = err
			f.closeMu.Unlock()
			close(done)
		}()
	}
	done := f.backendCloseDone
	f.closeMu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		f.closeMu.Lock()
		if f.backendCloseDone != done {
			f.closeMu.Unlock()
			return &CloseIncompleteError{Resource: "virtio-fs backend retry", Timeout: timeout}
		}
		err := f.backendCloseErr
		var incomplete *CloseIncompleteError
		if errors.As(err, &incomplete) {
			f.backendCloseDone = nil
			f.backendCloseErr = nil
		} else {
			f.backendClosed = true
		}
		f.closeMu.Unlock()
		return err
	case <-timer.C:
		return &CloseIncompleteError{Resource: "virtio-fs backend", Timeout: timeout}
	}
}

func resolveVirtioFSKickPoll() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CCX3_VIRTIOFS_KICK_POLL"))) {
	case "1", "true", "yes", "on":
		return true
	case "", "0", "false", "no", "off":
		return false
	default:
		return false
	}
}

func resolveVirtioFSEventIdx() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CCX3_VIRTIOFS_EVENT_IDX"))) {
	case "1", "true", "yes", "on":
		return true
	case "", "0", "false", "no", "off":
		return false
	default:
		return false
	}
}

func resolveVirtioFSKickPollDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func resolveVirtioFSDirectMemory() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CCX3_VIRTIOFS_DIRECT_MEMORY"))) {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func resolveVirtioFSAsync() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CCX3_VIRTIOFS_ASYNC"))) {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func resolveVirtioFSWorkerCount() int {
	const maxWorkers = 64
	// Four request queues are exposed to the guest. Keep one worker available
	// per queue so a blocking lock request or slow durability operation cannot
	// prevent an unlock or unrelated filesystem request from being dispatched.
	workers := fsRequestQueueCount
	if value := strings.TrimSpace(os.Getenv("CCX3_VIRTIOFS_WORKERS")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			workers = parsed
		}
	}
	if workers < 1 {
		return 1
	}
	if workers > maxWorkers {
		return maxWorkers
	}
	return workers
}

func resolveFSCachePolicy() (string, time.Duration, time.Duration) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CCX3_VIRTIOFS_CACHE"))) {
	case fsCacheStrict:
		return fsCacheStrict, 0, 0
	case fsCacheAggressive:
		return fsCacheAggressive, 60 * time.Second, 60 * time.Second
	default:
		return fsCacheNormal, 0, 0
	}
}

func cachePolicyForMode(mode string) FSCachePolicy {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case fsCacheStrict:
		return FSCachePolicy{Mode: fsCacheStrict}
	case fsCacheAggressive:
		return FSCachePolicy{Mode: fsCacheAggressive, EntryTTL: 60 * time.Second, AttrTTL: 60 * time.Second}
	default:
		return FSCachePolicy{Mode: fsCacheNormal}
	}
}

func (s *fuseServer) cachePolicy(nodeID uint64) FSCachePolicy {
	if be, ok := s.backend.(fsCachePolicyBackend); ok {
		policy := be.CachePolicy(nodeID)
		if policy.Mode != "" {
			return policy
		}
	}
	f := s.device
	return FSCachePolicy{Mode: f.cacheMode, EntryTTL: f.entryTTL, AttrTTL: f.attrTTL}
}

func (f *FS) Attach(mem GuestMemory, irq IRQController) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mem = mem
	f.irq = irq
	f.clearQueueCachesLocked()
}

func (f *FS) Contains(addr uint64, size int) bool {
	return addr >= f.Base && addr+uint64(size) <= f.Base+f.Size
}

func (f *FS) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("virtio@%x", f.Base),
		Properties: map[string]fdt.Property{
			"compatible": {Strings: []string{"virtio,mmio"}},
			"reg":        {U64: []uint64{f.Base, f.Size}},
			"interrupts": {U32: []uint32{0, f.IRQ, 4}},
			"status":     {Strings: []string{"okay"}},
		},
	}
}

func (f *FS) Read(addr uint64, size int) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mmioReads++

	offset := addr - f.Base
	switch offset {
	case regMagicValue:
		return truncateValue(mmioMagicValue, size), nil
	case regVersion:
		return truncateValue(mmioVersion, size), nil
	case regDeviceID:
		return truncateValue(mmioDeviceIDFS, size), nil
	case regVendorID:
		return truncateValue(mmioVendorID, size), nil
	case regDeviceFeatures:
		if f.deviceFeatureSel == 0 {
			return truncateValue(f.deviceFeaturesLocked(), size), nil
		}
		if f.deviceFeatureSel == 1 {
			return truncateValue(f.deviceFeaturesLocked()>>32, size), nil
		}
		return 0, nil
	case regQueueNumMax:
		if f.queueSel < uint32(len(f.queues)) {
			return truncateValue(128, size), nil
		}
		return 0, nil
	case regQueueNum:
		if q := f.selectedQueueLocked(); q != nil {
			return truncateValue(uint64(q.size), size), nil
		}
		return 0, nil
	case regQueueReady:
		if q := f.selectedQueueLocked(); q != nil && q.ready {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regInterruptStatus:
		if f.Log != nil {
			f.logf("mmio-read interrupt-status size=%d value=%#x", size, f.interruptStatus)
		}
		return truncateValue(uint64(f.interruptStatus), size), nil
	case regStatus:
		if f.Log != nil {
			f.logf("mmio-read status size=%d value=%#x", size, f.status)
		}
		return truncateValue(uint64(f.status), size), nil
	case regSharedMemoryLenLow, regSharedMemoryLenHigh:
		return truncateValue(^uint64(0), size), nil
	case regSharedMemoryBaseLow, regSharedMemoryBaseHigh:
		return truncateValue(^uint64(0), size), nil
	case regConfigGen:
		return truncateValue(uint64(f.configGeneration), size), nil
	}
	if offset >= regConfig && offset+uint64(size) <= regConfig+fsCfgTotalSize {
		cfg := f.configBytesLocked()
		return truncateValue(readConfigValue(cfg[offset-regConfig:], size), size), nil
	}
	return 0, nil
}

func (f *FS) Write(addr uint64, size int, value uint64) error {
	f.mu.Lock()
	f.mmioWrites++

	offset := addr - f.Base
	if f.Log != nil {
		switch offset {
		case regQueueSel, regQueueNum, regQueueReady, regInterruptAck, regStatus:
			f.logf("mmio-write off=%#x size=%d value=%#x", offset, size, value)
		}
	}
	switch offset {
	case regDeviceFeatSel:
		f.deviceFeatureSel = uint32(value)
	case regDriverFeatSel:
		f.driverFeatureSel = uint32(value)
	case regDriverFeatures:
		if f.driverFeatureSel == 0 {
			f.driverFeatures = (f.driverFeatures &^ 0xffffffff) | uint64(uint32(value))
		} else if f.driverFeatureSel == 1 {
			f.driverFeatures = (f.driverFeatures & 0xffffffff) | (uint64(uint32(value)) << 32)
		}
	case regSharedMemorySel:
		f.sharedMemorySel = uint32(value)
	case regQueueSel:
		f.queueSel = uint32(value)
	case regQueueNum:
		if q := f.selectedQueueLocked(); q != nil {
			q.size = uint16(value)
			q.clearCache()
		}
	case regQueueReady:
		if q := f.selectedQueueLocked(); q != nil {
			q.ready = value != 0
			if value == 0 {
				q.lastAvailIdx = 0
				q.usedIdx = 0
				q.noNotify = false
				q.clearCache()
			} else if f.driverFeatures&featureRingEventIdx != 0 {
				if err := f.ensureQueueCacheLocked(q); err != nil {
					f.mu.Unlock()
					return err
				}
				if err := f.writeAvailEventLocked(q); err != nil {
					f.mu.Unlock()
					return err
				}
			}
		}
	case regQueueDescLow:
		if q := f.selectedQueueLocked(); q != nil {
			f.setQueueAddr(&q.descAddr, uint32(value), true)
			q.clearCache()
		}
	case regQueueDescHigh:
		if q := f.selectedQueueLocked(); q != nil {
			f.setQueueAddr(&q.descAddr, uint32(value), false)
			q.clearCache()
		}
	case regQueueAvailLow:
		if q := f.selectedQueueLocked(); q != nil {
			f.setQueueAddr(&q.availAddr, uint32(value), true)
			q.clearCache()
		}
	case regQueueAvailHigh:
		if q := f.selectedQueueLocked(); q != nil {
			f.setQueueAddr(&q.availAddr, uint32(value), false)
			q.clearCache()
		}
	case regQueueUsedLow:
		if q := f.selectedQueueLocked(); q != nil {
			f.setQueueAddr(&q.usedAddr, uint32(value), true)
			q.clearCache()
		}
	case regQueueUsedHigh:
		if q := f.selectedQueueLocked(); q != nil {
			f.setQueueAddr(&q.usedAddr, uint32(value), false)
			q.clearCache()
		}
	case regInterruptAck:
		if f.Log != nil {
			f.logf("interrupt-ack value=%#x", value)
		}
		f.interruptStatus &^= uint32(value)
		err := f.updateIRQLocked()
		f.mu.Unlock()
		return err
	case regStatus:
		status := uint32(value)
		if status&virtioStatusFeaturesOK != 0 && f.driverFeatures&^f.deviceFeaturesLocked() != 0 {
			status &^= virtioStatusFeaturesOK
		}
		f.status = status
		if f.status == 0 {
			f.resetLocked()
		}
	case regQueueNotify:
		if int(value) < len(f.queues) {
			f.queueNotifies[int(value)]++
			harvestStart := time.Now()
			var workScratch [16]fsWork
			works, err := f.processQueueAsyncLocked(int(value), workScratch[:0])
			f.recordStageDuration(fsStageQueueHarvest, time.Since(harvestStart))
			if err != nil {
				f.mu.Unlock()
				return err
			}
			if f.Async && f.kickPoll && f.driverFeatures&featureRingEventIdx == 0 {
				// Keep guest kicks enabled while polling. Suppressing them creates
				// a lost-wakeup window when the poller becomes idle while the guest
				// is posting a descriptor based on the previous no-notify flag.
				f.startKickPollerLocked()
			}
			f.mu.Unlock()
			if f.Async {
				f.enqueueWorks(works)
				return nil
			}
			return f.processWorksInline(works)
		}
	}
	f.mu.Unlock()
	return nil
}

func (f *FS) processQueueAsyncLocked(qidx int, works []fsWork) ([]fsWork, error) {
	q := &f.queues[qidx]
	if !q.ready || q.size == 0 || f.mem == nil {
		return nil, nil
	}

	for {
		_, availIdx, err := f.readAvailHeaderLocked(q)
		if err != nil {
			return nil, err
		}
		for q.lastAvailIdx != availIdx {
			slot := q.lastAvailIdx % q.size
			head, err := f.readAvailRingEntryLocked(q, slot)
			if err != nil {
				return nil, err
			}
			if f.Log != nil {
				f.logf("queue-notify q=%d head=%d", qidx, head)
			}
			work, err := f.prepareRequestLocked(qidx, q, head)
			if err != nil {
				return nil, err
			}
			works = append(works, work)
			q.lastAvailIdx++
		}
		if f.driverFeatures&featureRingEventIdx == 0 {
			break
		}
		if err := f.writeAvailEventLocked(q); err != nil {
			return nil, err
		}
		_, latestAvailIdx, err := f.readAvailHeaderLocked(q)
		if err != nil {
			return nil, err
		}
		if q.lastAvailIdx == latestAvailIdx {
			break
		}
	}
	return works, nil
}

func (f *FS) prepareRequestLocked(qidx int, q *queue, head uint16) (fsWork, error) {
	var prepareStart time.Time
	if f.RecordTiming != nil {
		prepareStart = time.Now()
	}
	seq := f.nextWorkSeq[qidx]
	f.nextWorkSeq[qidx]++
	work := fsWork{qidx: qidx, head: head, seq: seq, generation: f.configGeneration}
	var descScratch [8]fsDesc
	descs, err := f.readDescriptorChainLocked(q, head, descScratch[:0])
	if err != nil {
		return work, err
	}
	var reqScratch [4]fsDesc
	var respScratch [4]fsDesc
	reqDescs := reqScratch[:0]
	respDescs := respScratch[:0]
	for _, d := range descs {
		if d.write {
			respDescs = append(respDescs, d)
			continue
		}
		if len(respDescs) != 0 {
			return work, fmt.Errorf("virtio-fs descriptor order invalid")
		}
		reqDescs = append(reqDescs, d)
	}
	if len(reqDescs) == 0 {
		return work, fmt.Errorf("virtio-fs missing request descriptors")
	}
	reqLen := 0
	for _, d := range reqDescs {
		reqLen += int(d.length)
	}
	req, pooledReq := takeFSReqBuffer(reqLen)
	reqOff := 0
	for _, d := range reqDescs {
		if err := f.readIPAInto(d.addr, req[reqOff:reqOff+int(d.length)]); err != nil {
			putFSReqBuffer(req, pooledReq)
			return work, err
		}
		reqOff += int(d.length)
	}
	work.req = req
	work.pooledReq = pooledReq
	work.respCount = len(respDescs)
	if len(respDescs) <= len(work.respDescs) {
		copy(work.respDescs[:], respDescs)
	} else {
		work.respExtra = append([]fsDesc(nil), respDescs...)
	}
	if !prepareStart.IsZero() {
		f.recordOpcodeTiming("prepare", work.opcode, time.Since(prepareStart))
	}
	return work, nil
}

func (f *FS) enqueueWorks(works []fsWork) {
	if len(works) == 0 {
		return
	}
	f.workerOnce.Do(f.startWorkers)
	for i := range works {
		work := new(fsWork)
		*work = works[i]
		select {
		case <-f.closed:
			putFSReqBuffer(work.req, work.pooledReq)
		case f.workCh <- work:
		}
	}
}

func (f *FS) startKickPollerLocked() {
	select {
	case <-f.closed:
		return
	default:
	}
	if f.kickPollActive {
		return
	}
	f.kickPollActive = true
	generation := f.configGeneration
	f.kickPollWG.Add(1)
	go f.runKickPoller(generation)
}

func (f *FS) runKickPoller(generation uint32) {
	defer f.kickPollWG.Done()
	idleUntil := time.Now().Add(f.kickPollIdle)
	for {
		select {
		case <-f.closed:
			f.mu.Lock()
			if generation == f.configGeneration {
				f.kickPollActive = false
			}
			f.mu.Unlock()
			return
		default:
		}
		var allWorks []fsWork
		processed := 0
		stop := false

		f.mu.Lock()
		if generation != f.configGeneration || !f.kickPoll {
			f.kickPollActive = false
			f.mu.Unlock()
			return
		}
		f.kickPollLoops++
		for qidx := fsQueueRequest; qidx < fsQueueRequest+fsRequestQueueCount && qidx < len(f.queues); qidx++ {
			var workScratch [16]fsWork
			works, err := f.processQueueAsyncLocked(qidx, workScratch[:0])
			if err != nil {
				f.logf("kick-poll q=%d: %v", qidx, err)
				continue
			}
			if len(works) == 0 {
				continue
			}
			processed += len(works)
			allWorks = append(allWorks, works...)
		}
		if processed != 0 {
			f.kickPollHits++
			f.kickPollWorks += uint64(processed)
			idleUntil = time.Now().Add(f.kickPollIdle)
		} else if !time.Now().Before(idleUntil) {
			f.kickPollMisses++
			if err := f.setRequestQueueNoNotifyLocked(false); err != nil {
				f.logf("kick-poll clear no-notify: %v", err)
			}
			// Publish the notification re-enable before the final queue check.
			// Without this release/acquire boundary, a guest can observe the old
			// no-notify flag, post work without kicking, and race past the
			// poller's final check, leaving the request asleep indefinitely.
			f.mu.Unlock()
			runtime.Gosched()
			f.mu.Lock()
			if generation != f.configGeneration || !f.kickPoll {
				f.kickPollActive = false
				f.mu.Unlock()
				f.enqueueWorks(allWorks)
				return
			}
			for qidx := fsQueueRequest; qidx < fsQueueRequest+fsRequestQueueCount && qidx < len(f.queues); qidx++ {
				var workScratch [16]fsWork
				works, err := f.processQueueAsyncLocked(qidx, workScratch[:0])
				if err != nil {
					f.logf("kick-poll final q=%d: %v", qidx, err)
					continue
				}
				if len(works) == 0 {
					continue
				}
				processed += len(works)
				allWorks = append(allWorks, works...)
			}
			if processed != 0 {
				f.kickPollHits++
				f.kickPollWorks += uint64(processed)
				idleUntil = time.Now().Add(f.kickPollIdle)
			} else {
				f.kickPollActive = false
				stop = true
			}
		} else {
			f.kickPollMisses++
		}
		f.mu.Unlock()

		f.enqueueWorks(allWorks)
		if stop {
			return
		}
		if processed == 0 && f.kickPollSleep > 0 {
			timer := time.NewTimer(f.kickPollSleep)
			select {
			case <-f.closed:
				timer.Stop()
				f.mu.Lock()
				if generation == f.configGeneration {
					f.kickPollActive = false
				}
				f.mu.Unlock()
				return
			case <-timer.C:
			}
		} else {
			runtime.Gosched()
		}
	}
}

func (f *FS) processWorksInline(works []fsWork) error {
	if len(works) == 0 {
		return nil
	}
	completions := make([]fsInlineCompletion, 0, len(works))
	dispatchStart := time.Now()
	for _, work := range works {
		result, err := f.dispatcher.Dispatch(work.req)
		putFSReqBuffer(work.req, work.pooledReq)
		work.req = nil
		work.pooledReq = false
		if err != nil {
			return err
		}
		work.opcode = result.opcode
		completions = append(completions, fsInlineCompletion{work: work, reply: result.reply})
	}
	f.recordStageDuration(fsStageInlineDispatch, time.Since(dispatchStart))
	return f.completeWorksInline(completions)
}

func (f *FS) startWorkers() {
	count := f.workerCount
	if count < 1 {
		count = 1
	}
	for i := 0; i < count; i++ {
		f.workerWG.Add(1)
		go f.runWorker()
	}
}

func (f *FS) runWorker() {
	defer f.workerWG.Done()
	for {
		select {
		case <-f.closed:
			return
		default:
		}
		var work *fsWork
		select {
		case <-f.closed:
			return
		case work = <-f.workCh:
		}
		select {
		case <-f.closed:
			putFSReqBuffer(work.req, work.pooledReq)
			return
		default:
		}
		result, err := f.dispatcher.Dispatch(work.req)
		putFSReqBuffer(work.req, work.pooledReq)
		work.req = nil
		work.pooledReq = false
		work.opcode = result.opcode
		if err != nil {
			f.logf("async-fuse-error q=%d head=%d: %v", work.qidx, work.head, err)
			result.reply = fuseReply(result.unique, -linuxEIO, nil)
			err = nil
		}
		select {
		case <-f.closed:
			return
		default:
		}
		if err := f.completeWork(*work, result.reply, err); err != nil {
			f.logf("async-complete-error q=%d head=%d: %v", work.qidx, work.head, err)
		}
	}
}

func (f *FS) completeWork(work fsWork, reply fsReply, workErr error) error {
	defer f.recordStageTiming(fsStageAsyncComplete, time.Now())
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.closed:
		return nil
	default:
	}
	if work.generation != f.configGeneration || work.qidx < 0 || work.qidx >= len(f.queues) {
		return nil
	}
	if f.completions == nil {
		f.completions = make(map[fsCompletionKey]fsCompletion)
	}
	f.completions[fsCompletionKey{qidx: work.qidx, seq: work.seq}] = fsCompletion{work: work, reply: reply, err: workErr}
	return f.drainCompletionsLocked(work.qidx)
}

func (f *FS) completeWorksInline(completions []fsInlineCompletion) error {
	if len(completions) == 0 {
		return nil
	}
	defer f.recordStageTiming(fsStageInlineComplete, time.Now())
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := 0; index < len(completions); {
		qidx := completions[index].work.qidx
		if qidx < 0 || qidx >= len(f.queues) {
			index++
			continue
		}
		q := &f.queues[qidx]
		oldUsedIdx := q.usedIdx
		wroteCompletion := false
		for index < len(completions) && completions[index].work.qidx == qidx {
			completion := completions[index]
			index++
			if completion.work.generation != f.configGeneration {
				continue
			}
			if completion.err != nil {
				return completion.err
			}
			if err := f.writeCompletionUsedLocked(q, completion.work, completion.reply); err != nil {
				return err
			}
			wroteCompletion = true
		}
		if !wroteCompletion || !f.isCompletingQueue(qidx) {
			continue
		}
		shouldInterrupt, err := f.shouldInterruptCompletionLocked(q, oldUsedIdx)
		if err != nil {
			return err
		}
		if shouldInterrupt {
			f.interruptStatus |= fsInterruptVring
			f.interruptRaises++
			if f.Log != nil {
				f.logf("interrupt-raise status=%#x", f.interruptStatus)
			}
			if err := f.updateIRQLocked(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *FS) drainCompletionsLocked(qidx int) error {
	q := &f.queues[qidx]
	for {
		seq := f.nextCompleteSeq[qidx]
		key := fsCompletionKey{qidx: qidx, seq: seq}
		completion, ok := f.completions[key]
		if !ok {
			return nil
		}
		delete(f.completions, key)
		f.nextCompleteSeq[qidx]++
		if completion.err != nil {
			return completion.err
		}
		if err := f.writeCompletionLocked(q, completion.work, completion.reply); err != nil {
			return err
		}
	}
}

func (f *FS) writeCompletionLocked(q *queue, work fsWork, reply fsReply) error {
	oldUsedIdx := q.usedIdx
	if err := f.writeCompletionUsedLocked(q, work, reply); err != nil {
		return err
	}
	if f.isCompletingQueue(work.qidx) {
		shouldInterrupt, err := f.shouldInterruptCompletionLocked(q, oldUsedIdx)
		if err != nil {
			return err
		}
		if !shouldInterrupt {
			return nil
		}
		f.interruptStatus |= fsInterruptVring
		f.interruptRaises++
		if f.Log != nil {
			f.logf("interrupt-raise status=%#x", f.interruptStatus)
		}
		return f.updateIRQLocked()
	}
	return nil
}

func (f *FS) shouldInterruptCompletionLocked(q *queue, oldUsedIdx uint16) (bool, error) {
	if f.driverFeatures&featureRingEventIdx != 0 {
		return f.shouldInterruptLocked(q, oldUsedIdx, q.usedIdx, 0), nil
	}
	flags, _, err := f.readAvailHeaderLocked(q)
	if err != nil {
		return false, err
	}
	return flags&fsAvailNoInterrupt == 0, nil
}

// Even requests without a FUSE response (FORGET) must return their descriptor
// to the used ring. Linux drains these outstanding requests before unmounting.
func (f *FS) writeCompletionUsedLocked(q *queue, work fsWork, reply fsReply) error {
	var completeStart time.Time
	if f.RecordTiming != nil {
		completeStart = time.Now()
	}
	if err := f.writeReplyToResponseDescsLocked(work, reply); err != nil {
		return err
	}
	if err := f.writeUsedLocked(q, work.head, uint32(reply.Len())); err != nil {
		return err
	}
	if f.Log != nil {
		f.logf("used-ring q=%d head=%d len=%d", work.qidx, work.head, reply.Len())
	}
	if !completeStart.IsZero() {
		f.recordOpcodeTiming("complete", work.opcode, time.Since(completeStart))
	}
	return nil
}

func (f *FS) writeReplyToResponseDescsLocked(work fsWork, reply fsReply) error {
	if !reply.ok {
		return nil
	}
	reply.EncodeHeader(f.scratch16[:])
	segments := [2][]byte{f.scratch16[:], reply.extra}
	descIndex := 0
	descOffset := uint32(0)
	written := 0
	for _, segment := range segments {
		for len(segment) != 0 {
			if descIndex >= work.respCount {
				return fmt.Errorf("virtio-fs response truncated: need %d have %d", reply.Len(), written)
			}
			desc := work.responseDesc(descIndex)
			if descOffset >= desc.length {
				descIndex++
				descOffset = 0
				continue
			}
			chunk := len(segment)
			space := int(desc.length - descOffset)
			if chunk > space {
				chunk = space
			}
			if err := f.writeIPAFrom(desc.addr+uint64(descOffset), segment[:chunk]); err != nil {
				return err
			}
			segment = segment[chunk:]
			descOffset += uint32(chunk)
			written += chunk
		}
	}
	return nil
}

func (w *fsWork) responseDesc(index int) fsDesc {
	if w.respExtra != nil {
		return w.respExtra[index]
	}
	return w.respDescs[index]
}

func fuseMayChangeBacking(opcode uint32) bool {
	switch opcode {
	case fuseLookup, fuseForget, fuseSetAttr, fuseSymlink, fuseMknod, fuseMkdir,
		fuseUnlink, fuseRmDir, fuseRename, fuseRename2, fuseLink, fuseOpen,
		fuseWrite, fuseRelease, fuseSetXattr, fuseRemoveXattr, fuseOpenDir,
		fuseReleaseDir, fuseCreate, fuseTmpfile, fuseDestroy:
		return true
	default:
		return false
	}
}

func fuseOpcodeName(opcode uint32) string {
	switch opcode {
	case fuseLookup:
		return "LOOKUP"
	case fuseForget:
		return "FORGET"
	case fuseGetAttr:
		return "GETATTR"
	case fuseSetAttr:
		return "SETATTR"
	case fuseReadlink:
		return "READLINK"
	case fuseSymlink:
		return "SYMLINK"
	case fuseMknod:
		return "MKNOD"
	case fuseMkdir:
		return "MKDIR"
	case fuseUnlink:
		return "UNLINK"
	case fuseRmDir:
		return "RMDIR"
	case fuseRename:
		return "RENAME"
	case fuseRename2:
		return "RENAME2"
	case fuseLink:
		return "LINK"
	case fuseOpen:
		return "OPEN"
	case fuseRead:
		return "READ"
	case fuseWrite:
		return "WRITE"
	case fuseStatfs:
		return "STATFS"
	case fuseRelease:
		return "RELEASE"
	case fuseFsync:
		return "FSYNC"
	case fuseSetXattr:
		return "SETXATTR"
	case fuseGetXattr:
		return "GETXATTR"
	case fuseListXattr:
		return "LISTXATTR"
	case fuseRemoveXattr:
		return "REMOVEXATTR"
	case fuseFlush:
		return "FLUSH"
	case fuseInit:
		return "INIT"
	case fuseOpenDir:
		return "OPENDIR"
	case fuseReadDir:
		return "READDIR"
	case fuseReadDirPlus:
		return "READDIRPLUS"
	case fuseReleaseDir:
		return "RELEASEDIR"
	case fuseFsyncDir:
		return "FSYNCDIR"
	case fuseGetLK:
		return "GETLK"
	case fuseSetLK:
		return "SETLK"
	case fuseSetLKW:
		return "SETLKW"
	case fuseAccess:
		return "ACCESS"
	case fuseCreate:
		return "CREATE"
	case fuseDestroy:
		return "DESTROY"
	case fuseIoctl:
		return "IOCTL"
	case fusePoll:
		return "POLL"
	case fuseLseek:
		return "LSEEK"
	case fuseSyncFS:
		return "SYNCFS"
	case fuseTmpfile:
		return "TMPFILE"
	case fuseStatx:
		return "STATX"
	default:
		return "UNKNOWN"
	}
}

func fuseOpcodeMetricName(opcode uint32) string {
	return strings.ToLower(fuseOpcodeName(opcode))
}

func (f *FS) recordFUSEDispatchTiming(opcode uint32, start time.Time) {
	duration := time.Since(start)
	f.fuseRequests.Add(1)
	if opcode < uint32(len(f.fuseOpStats)) {
		recordTimingStat(&f.fuseOpStats[opcode].timingStat, duration)
	}
	f.recordOpcodeTiming("fuse", opcode, duration)
}

func (f *FS) recordOpcodeTiming(stage string, opcode uint32, duration time.Duration) {
	if f.RecordTiming == nil {
		return
	}
	tag := strings.TrimRight(string(f.tag[:]), "\x00")
	if tag == "" {
		tag = "unknown"
	}
	f.RecordTiming("virtio.fs."+tag+"."+stage+"."+fuseOpcodeMetricName(opcode), duration)
}

func (f *FS) recordStageTiming(stage int, start time.Time) {
	f.recordStageDuration(stage, time.Since(start))
}

func (f *FS) recordStageDuration(stage int, duration time.Duration) {
	if stage < 0 || stage >= len(f.stageStats) {
		return
	}
	recordTimingStat(&f.stageStats[stage], duration)
	if f.RecordTiming == nil {
		return
	}
	tag := strings.TrimRight(string(f.tag[:]), "\x00")
	if tag == "" {
		tag = "unknown"
	}
	f.RecordTiming("virtio.fs."+tag+".stage."+fsStageName(stage), duration)
}

func recordTimingStat(stat *timingStat, duration time.Duration) {
	nanos := duration.Nanoseconds()
	stat.count.Add(1)
	stat.totalNanos.Add(nanos)
	for {
		oldMax := stat.maxNanos.Load()
		if nanos <= oldMax || stat.maxNanos.CompareAndSwap(oldMax, nanos) {
			break
		}
	}
}

func fsStageName(stage int) string {
	switch stage {
	case fsStageQueueHarvest:
		return "queue_harvest"
	case fsStageInlineDispatch:
		return "inline_dispatch"
	case fsStageInlineComplete:
		return "inline_complete"
	case fsStageAsyncComplete:
		return "async_complete"
	default:
		return "unknown"
	}
}

type fsReply struct {
	unique uint64
	errno  int32
	extra  []byte
	ok     bool
}

func fuseReply(unique uint64, errno int32, extra []byte) fsReply {
	return fsReply{unique: unique, errno: errno, extra: extra, ok: true}
}

func (r fsReply) Len() int {
	if !r.ok {
		return 0
	}
	return fuseOutHeaderSize + len(r.extra)
}

func (r fsReply) EncodeHeader(dst []byte) {
	binary.LittleEndian.PutUint32(dst[0:4], uint32(r.Len()))
	binary.LittleEndian.PutUint32(dst[4:8], uint32(r.errno))
	binary.LittleEndian.PutUint64(dst[8:16], r.unique)
}

func (r fsReply) Bytes() []byte {
	if !r.ok {
		return nil
	}
	out := make([]byte, r.Len())
	r.EncodeHeader(out[:fuseOutHeaderSize])
	copy(out[fuseOutHeaderSize:], r.extra)
	return out
}

func (s *fuseServer) encodeFuseEntryOut(dst []byte, nodeID uint64) {
	policy := s.cachePolicy(nodeID)
	binary.LittleEndian.PutUint64(dst[0:8], nodeID)
	binary.LittleEndian.PutUint64(dst[8:16], 1)
	encodeFuseTTL(dst[16:24], dst[32:36], policy.EntryTTL)
	encodeFuseTTL(dst[24:32], dst[36:40], policy.AttrTTL)
}

func (s *fuseServer) readDirPlus(nodeID uint64, fh uint64, off uint64, maxBytes uint32) ([]byte, int32) {
	var out []byte
	for {
		entries, errno := s.backend.ReadDir(nodeID, fh, off, maxBytes)
		if errno != 0 {
			return nil, errno
		}
		if len(entries) == 0 {
			return out, 0
		}
		for cursor := 0; cursor < len(entries); {
			if len(entries)-cursor < fuseDirentBaseSize {
				return nil, -linuxEIO
			}
			nameBytes := int(binary.LittleEndian.Uint32(entries[cursor+16 : cursor+20]))
			direntBytes := align8(fuseDirentBaseSize + nameBytes)
			if direntBytes > len(entries)-cursor {
				return nil, -linuxEIO
			}
			plusBytes := align8(fuseDirentPlusBaseSize + nameBytes)
			if len(out)+plusBytes > int(maxBytes) {
				return out, 0
			}
			off = binary.LittleEndian.Uint64(entries[cursor+8 : cursor+16])
			childID := binary.LittleEndian.Uint64(entries[cursor : cursor+8])
			attr, attrErrno := s.backend.GetAttr(childID)
			cursor += direntBytes
			if attrErrno == -linuxENOENT {
				continue
			}
			if attrErrno != 0 {
				return nil, attrErrno
			}
			start := len(out)
			out = append(out, make([]byte, plusBytes)...)
			s.encodeFuseEntryOut(out[start:start+fuseEntryOutSize], childID)
			encodeFuseAttr(out[start+40:start+fuseEntryOutSize], attr)
			copy(out[start+fuseEntryOutSize:], entries[cursor-direntBytes:cursor])
		}
		if len(out) != 0 {
			return out, 0
		}
		// Every entry in this page disappeared between READDIR and GETATTR.
		// Continue from the last cookie instead of reporting a false EOF.
	}
}

func encodeFuseAttrTTL(dst []byte, ttl time.Duration) {
	encodeFuseTTL(dst[0:8], dst[8:12], ttl)
}

func encodeFuseTTL(secDst []byte, nsecDst []byte, ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	sec := ttl / time.Second
	nsec := ttl % time.Second
	binary.LittleEndian.PutUint64(secDst, uint64(sec))
	binary.LittleEndian.PutUint32(nsecDst, uint32(nsec))
}

func (s *fuseServer) openResponseFlags(nodeID uint64, openFlags uint32, dir bool) uint32 {
	policy := s.cachePolicy(nodeID)
	flags := uint32(fuseOpenNoFlush)
	if policy.Mode == fsCacheStrict {
		return flags
	}
	if dir {
		return flags | fuseOpenCacheDir
	}
	if openFlags&linuxOACCMODE == linuxORDONLY {
		flags |= fuseOpenKeepCache
	}
	return flags
}

func takeFSReqBuffer(size int) ([]byte, bool) {
	if size > fsPooledReqSize {
		return make([]byte, size), false
	}
	buf := fsReqPool.Get().([]byte)
	return buf[:size], true
}

func putFSReqBuffer(buf []byte, pooled bool) {
	if !pooled {
		return
	}
	buf = buf[:fsPooledReqSize]
	fsReqPool.Put(buf)
}

func (f *FS) logf(format string, args ...any) {
	if f.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(f.Log, format+"\n", args...)
}

func (s *fuseServer) logPathf(op string, nodeID uint64, suffix string) {
	f := s.device
	if f.Log == nil {
		return
	}
	if resolver, ok := s.backend.(interface{ DebugPath(uint64) string }); ok {
		_, _ = fmt.Fprintf(f.Log, "%s node=%d path=%q%s\n", op, nodeID, resolver.DebugPath(nodeID), suffix)
		return
	}
	_, _ = fmt.Fprintf(f.Log, "%s node=%d%s\n", op, nodeID, suffix)
}

func (f *FS) readIPAInto(addr uint64, dst []byte) error {
	if f.mem == nil {
		return fmt.Errorf("guest memory is detached")
	}
	if reader, ok := f.mem.(guestMemoryReaderInto); ok {
		return reader.ReadIPAInto(addr, dst)
	}
	buf, err := f.mem.ReadIPA(addr, len(dst))
	if err != nil {
		return err
	}
	copy(dst, buf)
	return nil
}

func (f *FS) writeIPAFrom(addr uint64, src []byte) error {
	if len(src) == 0 {
		return nil
	}
	if f.mem == nil {
		return fmt.Errorf("guest memory is detached")
	}
	return f.mem.WriteIPA(addr, src)
}

func (f *FS) ensureQueueCacheLocked(q *queue) error {
	if q.size == 0 || q.descAddr == 0 || q.availAddr == 0 || q.usedAddr == 0 {
		return nil
	}
	if q.descMem != nil && q.availMem != nil && q.usedMem != nil {
		return nil
	}
	if !f.directMemory {
		return nil
	}
	slicer, ok := f.mem.(guestMemorySlicer)
	if !ok {
		return nil
	}
	descLen := int(q.size) * 16
	availLen := 4 + int(q.size)*2 + 2
	usedLen := 4 + int(q.size)*8 + 2
	descMem, err := slicer.SliceIPA(q.descAddr, descLen)
	if err != nil {
		return err
	}
	availMem, err := slicer.SliceIPA(q.availAddr, availLen)
	if err != nil {
		return err
	}
	usedMem, err := slicer.SliceIPA(q.usedAddr, usedLen)
	if err != nil {
		return err
	}
	q.descMem = descMem
	q.availMem = availMem
	q.usedMem = usedMem
	return nil
}

func (f *FS) readAvailHeaderLocked(q *queue) (flags uint16, idx uint16, err error) {
	if err := f.ensureQueueCacheLocked(q); err != nil {
		return 0, 0, err
	}
	if len(q.availMem) >= 4 {
		return binary.LittleEndian.Uint16(q.availMem[0:2]), binary.LittleEndian.Uint16(q.availMem[2:4]), nil
	}
	if err := f.readIPAInto(q.availAddr, f.scratch4[:]); err != nil {
		return 0, 0, err
	}
	return binary.LittleEndian.Uint16(f.scratch4[0:2]), binary.LittleEndian.Uint16(f.scratch4[2:4]), nil
}

func (f *FS) readAvailRingEntryLocked(q *queue, slot uint16) (uint16, error) {
	offset := 4 + int(slot)*2
	if len(q.availMem) >= offset+2 {
		return binary.LittleEndian.Uint16(q.availMem[offset : offset+2]), nil
	}
	if err := f.readIPAInto(q.availAddr+uint64(offset), f.scratch2[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(f.scratch2[:]), nil
}

func (f *FS) readDescriptorChainLocked(q *queue, head uint16, out []fsDesc) ([]fsDesc, error) {
	if err := f.ensureQueueCacheLocked(q); err != nil {
		return nil, err
	}
	index := head
	for i := uint16(0); i < q.size; i++ {
		if index >= q.size {
			return nil, fmt.Errorf("virtio-fs descriptor index %d out of range", index)
		}
		var raw []byte
		offset := int(index) * 16
		if len(q.descMem) >= offset+16 {
			raw = q.descMem[offset : offset+16]
		} else {
			if err := f.readIPAInto(q.descAddr+uint64(offset), f.scratch16[:]); err != nil {
				return nil, err
			}
			raw = f.scratch16[:]
		}
		desc := fsDesc{
			addr:   binary.LittleEndian.Uint64(raw[0:8]),
			length: binary.LittleEndian.Uint32(raw[8:12]),
			flags:  binary.LittleEndian.Uint16(raw[12:14]),
			next:   binary.LittleEndian.Uint16(raw[14:16]),
		}
		desc.write = desc.flags&descFWrite != 0
		out = append(out, desc)
		if desc.flags&descFNext == 0 {
			return out, nil
		}
		index = desc.next
	}
	return nil, fmt.Errorf("virtio-fs descriptor loop")
}

func (f *FS) writeUsedLocked(q *queue, head uint16, usedLen uint32) error {
	slot := q.usedIdx % q.size
	if err := f.ensureQueueCacheLocked(q); err != nil {
		return err
	}
	offset := 4 + int(slot)*8
	if len(q.usedMem) >= offset+8 {
		binary.LittleEndian.PutUint32(q.usedMem[offset:offset+4], uint32(head))
		binary.LittleEndian.PutUint32(q.usedMem[offset+4:offset+8], usedLen)
	} else {
		binary.LittleEndian.PutUint32(f.scratch8[0:4], uint32(head))
		binary.LittleEndian.PutUint32(f.scratch8[4:8], usedLen)
		if err := f.writeIPAFrom(q.usedAddr+4+uint64(slot)*8, f.scratch8[:]); err != nil {
			return err
		}
	}
	q.usedIdx++
	if len(q.usedMem) >= 4 {
		binary.LittleEndian.PutUint16(q.usedMem[2:4], q.usedIdx)
		return nil
	}
	binary.LittleEndian.PutUint16(f.scratch2[:], q.usedIdx)
	return f.writeIPAFrom(q.usedAddr+2, f.scratch2[:])
}

func (f *FS) shouldInterruptLocked(q *queue, oldUsedIdx, newUsedIdx, availFlags uint16) bool {
	if oldUsedIdx == newUsedIdx {
		return false
	}
	if f.driverFeatures&featureRingEventIdx == 0 {
		return availFlags&1 == 0
	}
	offset := 4 + int(q.size)*2
	if len(q.availMem) >= offset+2 {
		usedEvent := binary.LittleEndian.Uint16(q.availMem[offset : offset+2])
		return vringNeedEvent(usedEvent, newUsedIdx, oldUsedIdx)
	}
	if err := f.readIPAInto(q.availAddr+uint64(offset), f.scratch2[:]); err != nil {
		if f.Log != nil {
			f.logf("used-event-read-error: %v", err)
		}
		return true
	}
	usedEvent := binary.LittleEndian.Uint16(f.scratch2[:])
	return vringNeedEvent(usedEvent, newUsedIdx, oldUsedIdx)
}

func (f *FS) setRequestQueueNoNotifyLocked(suppress bool) error {
	for qidx := fsQueueRequest; qidx < fsQueueRequest+fsRequestQueueCount && qidx < len(f.queues); qidx++ {
		q := &f.queues[qidx]
		if !q.ready || q.size == 0 {
			continue
		}
		if err := f.setQueueNoNotifyLocked(q, suppress); err != nil {
			return err
		}
	}
	return nil
}

func (f *FS) setQueueNoNotifyLocked(q *queue, suppress bool) error {
	if q.size == 0 || q.usedAddr == 0 || q.noNotify == suppress {
		return nil
	}
	if err := f.ensureQueueCacheLocked(q); err != nil {
		return err
	}
	flags := uint16(0)
	if suppress {
		flags = 1
	}
	if len(q.usedMem) >= 2 {
		binary.LittleEndian.PutUint16(q.usedMem[0:2], flags)
	} else {
		binary.LittleEndian.PutUint16(f.scratch2[:], flags)
		if err := f.writeIPAFrom(q.usedAddr, f.scratch2[:]); err != nil {
			return err
		}
	}
	q.noNotify = suppress
	return nil
}

func (f *FS) writeAvailEventLocked(q *queue) error {
	if q.size == 0 || q.usedAddr == 0 {
		return nil
	}
	if err := f.ensureQueueCacheLocked(q); err != nil {
		return err
	}
	offset := 4 + int(q.size)*8
	if len(q.usedMem) >= offset+2 {
		binary.LittleEndian.PutUint16(q.usedMem[offset:offset+2], q.lastAvailIdx)
		return nil
	}
	binary.LittleEndian.PutUint16(f.scratch2[:], q.lastAvailIdx)
	return f.writeIPAFrom(q.usedAddr+uint64(offset), f.scratch2[:])
}

func vringNeedEvent(eventIdx, newIdx, oldIdx uint16) bool {
	return uint16(newIdx-eventIdx-1) < uint16(newIdx-oldIdx)
}

func (f *FS) updateIRQLocked() error {
	if f.irq == nil {
		return nil
	}
	level := f.interruptStatus != 0
	if f.irqHigh == level {
		if level {
			return f.irq.SetIRQ(f.IRQ, true)
		}
		return nil
	}
	f.irqHigh = level
	f.irqTransitions++
	if f.Log != nil {
		f.logf("set-irq irq=%d level=%v", f.IRQ, level)
	}
	return f.irq.SetIRQ(f.IRQ, level)
}

func (f *FS) IRQAsserted() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interruptStatus != 0 || f.irqHigh
}

// Poke asks the guest driver to rescan completed filesystem requests. It is a
// recovery path for a guest that went to sleep across an interrupt-suppression
// transition after the host had already published a used-ring entry.
func (f *FS) Poke() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.irq == nil {
		return nil
	}
	f.interruptStatus |= fsInterruptVring
	if f.irqHigh {
		if err := f.irq.SetIRQ(f.IRQ, false); err != nil {
			return err
		}
		f.irqHigh = false
		f.irqTransitions++
	}
	return f.updateIRQLocked()
}

func (f *FS) Summary() string {
	if f == nil {
		return "virtio-fs=<nil>"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tag := strings.TrimRight(string(f.tag[:]), "\x00")
	queueNotifies := append([]uint64(nil), f.queueNotifies...)
	queueReady := f.queueReadySnapshotLocked()
	queueLastAvail := f.queueLastAvailSnapshotLocked()
	return fmt.Sprintf(
		"virtio-fs tag=%q mmio_reads=%d mmio_writes=%d status=%#x queue_notifies=%v fuse_requests=%d interrupt_raises=%d irq_transitions=%d irq_high=%t interrupt_status=%#x queue_ready=%v queue_last=%v",
		tag,
		f.mmioReads,
		f.mmioWrites,
		f.status,
		queueNotifies,
		f.fuseRequests.Load(),
		f.interruptRaises,
		f.irqTransitions,
		f.irqHigh,
		f.interruptStatus,
		queueReady,
		queueLastAvail,
	)
}

func (f *FS) Stats() FSStats {
	if f == nil {
		return FSStats{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tag := strings.TrimRight(string(f.tag[:]), "\x00")
	ops := make([]FUSEOpStats, 0, len(f.fuseOpStats))
	for opcode := range f.fuseOpStats {
		stat := &f.fuseOpStats[opcode].timingStat
		count := stat.count.Load()
		if count == 0 {
			continue
		}
		totalNanos := stat.totalNanos.Load()
		avg := int64(0)
		if count != 0 {
			avg = totalNanos / int64(count)
		}
		opcodeValue := uint32(opcode)
		ops = append(ops, FUSEOpStats{
			Opcode:       opcodeValue,
			Name:         fuseOpcodeName(opcodeValue),
			Count:        count,
			TotalNanos:   totalNanos,
			MaxNanos:     stat.maxNanos.Load(),
			AverageNanos: avg,
		})
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Count == ops[j].Count {
			return ops[i].Opcode < ops[j].Opcode
		}
		return ops[i].Count > ops[j].Count
	})
	stages := make([]TimingStats, 0, len(f.stageStats))
	for stage := range f.stageStats {
		if stats, ok := timingStatsSnapshot(fsStageName(stage), &f.stageStats[stage]); ok {
			stages = append(stages, stats)
		}
	}
	stats := FSStats{
		Tag:             tag,
		Async:           f.Async,
		WorkerCount:     f.workerCount,
		CacheMode:       f.cacheMode,
		WritebackCache:  f.writebackCache,
		EventIdx:        f.eventIdx,
		MMIOReads:       f.mmioReads,
		MMIOWrites:      f.mmioWrites,
		QueueNotifies:   append([]uint64(nil), f.queueNotifies...),
		KickPollLoops:   f.kickPollLoops,
		KickPollHits:    f.kickPollHits,
		KickPollMisses:  f.kickPollMisses,
		KickPollWorks:   f.kickPollWorks,
		FUSERequests:    f.fuseRequests.Load(),
		InterruptRaises: f.interruptRaises,
		IRQTransitions:  f.irqTransitions,
		IRQHigh:         f.irqHigh,
		InterruptStatus: f.interruptStatus,
		QueueReady:      f.queueReadySnapshotLocked(),
		QueueLastAvail:  f.queueLastAvailSnapshotLocked(),
		QueueAvailIdx:   f.queueAvailIdxSnapshotLocked(),
		QueueUsedIdx:    f.queueUsedIdxSnapshotLocked(),
		QueueNoNotify:   f.queueNoNotifySnapshotLocked(),
		FUSEOps:         ops,
		Stages:          stages,
	}
	if provider, ok := f.filesystemBackend().(interface {
		BackingUsage() (uint64, uint64, uint64, error)
	}); ok {
		current, highWater, physical, usageErr := provider.BackingUsage()
		stats.BackingBytes, stats.BackingHighWaterBytes = current, highWater
		stats.BackingPhysicalBytes = physical
		if usageErr != nil {
			stats.BackingReclaimError = usageErr.Error()
		}
	}
	if provider, ok := f.filesystemBackend().(interface{ BackingMetadataUsage() (uint64, uint64) }); ok {
		stats.BackingMetadataBytes, stats.BackingMetadataHighWaterBytes = provider.BackingMetadataUsage()
	}
	return stats
}

func (f *FS) BackingUsage() (current, highWater, physical uint64, reclaimErr error) {
	if f == nil {
		return 0, 0, 0, nil
	}
	f.mu.Lock()
	backend := f.filesystemBackend()
	f.mu.Unlock()
	provider, ok := backend.(interface {
		BackingUsage() (uint64, uint64, uint64, error)
	})
	if !ok {
		return 0, 0, 0, nil
	}
	return provider.BackingUsage()
}

func (f *FS) BackingUsageTracker() *FSBackingUsageTracker {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usageTracker()
}

func (f *FS) BackingMetadataUsage() (current, highWater uint64) {
	if f == nil {
		return 0, 0
	}
	f.mu.Lock()
	backend := f.filesystemBackend()
	f.mu.Unlock()
	provider, ok := backend.(interface{ BackingMetadataUsage() (uint64, uint64) })
	if !ok {
		return 0, 0
	}
	return provider.BackingMetadataUsage()
}

func (f *FS) PersistentFSStatus() []PersistentFSStatus {
	if f == nil {
		return nil
	}
	backend := f.filesystemBackend()
	if provider, ok := backend.(interface{ PersistentFSStatus() []PersistentFSStatus }); ok {
		return provider.PersistentFSStatus()
	}
	return nil
}

func timingStatsSnapshot(name string, stat *timingStat) (TimingStats, bool) {
	count := stat.count.Load()
	if count == 0 {
		return TimingStats{}, false
	}
	totalNanos := stat.totalNanos.Load()
	avg := int64(0)
	if count != 0 {
		avg = totalNanos / int64(count)
	}
	return TimingStats{
		Name:         name,
		Count:        count,
		TotalNanos:   totalNanos,
		MaxNanos:     stat.maxNanos.Load(),
		AverageNanos: avg,
	}, true
}

func (f *FS) queueReadySnapshotLocked() []bool {
	ready := make([]bool, len(f.queues))
	for i := range f.queues {
		ready[i] = f.queues[i].ready
	}
	return ready
}

func (f *FS) queueLastAvailSnapshotLocked() []uint16 {
	last := make([]uint16, len(f.queues))
	for i := range f.queues {
		last[i] = f.queues[i].lastAvailIdx
	}
	return last
}

func (f *FS) queueAvailIdxSnapshotLocked() []uint16 {
	idxs := make([]uint16, len(f.queues))
	for i := range f.queues {
		if !f.queues[i].ready || f.queues[i].size == 0 {
			continue
		}
		_, idx, err := f.readAvailHeaderLocked(&f.queues[i])
		if err == nil {
			idxs[i] = idx
		}
	}
	return idxs
}

func (f *FS) queueUsedIdxSnapshotLocked() []uint16 {
	idxs := make([]uint16, len(f.queues))
	for i := range f.queues {
		idxs[i] = f.queues[i].usedIdx
	}
	return idxs
}

func (f *FS) queueNoNotifySnapshotLocked() []bool {
	flags := make([]bool, len(f.queues))
	for i := range f.queues {
		flags[i] = f.queues[i].noNotify
	}
	return flags
}

func (f *FS) isCompletingQueue(qidx int) bool {
	return qidx >= 0 && qidx < len(f.queues)
}

func (f *FS) selectedQueueLocked() *queue {
	if f.queueSel >= uint32(len(f.queues)) {
		return nil
	}
	return &f.queues[f.queueSel]
}

func (f *FS) setQueueAddr(target *uint64, value uint32, low bool) {
	if low {
		*target = (*target &^ 0xffffffff) | uint64(value)
	} else {
		*target = (*target & 0xffffffff) | (uint64(value) << 32)
	}
}

func (f *FS) resetLocked() {
	f.deviceFeatureSel = 0
	f.driverFeatureSel = 0
	f.driverFeatures = 0
	f.sharedMemorySel = 0
	f.queueSel = 0
	f.status = 0
	f.interruptStatus = 0
	f.irqHigh = false
	f.configGeneration++
	f.kickPollActive = false
	f.resetQueueStateLocked()
}

func (f *FS) clearQueueCachesLocked() {
	for i := range f.queues {
		f.queues[i].descMem = nil
		f.queues[i].availMem = nil
		f.queues[i].usedMem = nil
	}
}

func (f *FS) deviceFeaturesLocked() uint64 {
	features := featureVersion1
	if f.eventIdx && !f.kickPoll {
		features |= featureRingEventIdx
	}
	return features
}

func (f *FS) configBytesLocked() []byte {
	cfg := make([]byte, fsCfgTotalSize)
	copy(cfg[:fsCfgTagSize], f.tag[:])
	binary.LittleEndian.PutUint32(cfg[fsCfgNumQueueOff:fsCfgNumQueueOff+4], fsRequestQueueCount)
	return cfg
}

func (f *FS) resetQueueStateLocked() {
	queueCount := fsQueueCount
	if cap(f.queues) < queueCount {
		f.queues = make([]queue, queueCount)
	} else {
		f.queues = f.queues[:queueCount]
		clear(f.queues)
	}
	if len(f.queueNotifies) != queueCount {
		old := f.queueNotifies
		f.queueNotifies = make([]uint64, queueCount)
		copy(f.queueNotifies, old)
	}
	if cap(f.nextWorkSeq) < queueCount {
		f.nextWorkSeq = make([]uint64, queueCount)
	} else {
		f.nextWorkSeq = f.nextWorkSeq[:queueCount]
		clear(f.nextWorkSeq)
	}
	if cap(f.nextCompleteSeq) < queueCount {
		f.nextCompleteSeq = make([]uint64, queueCount)
	} else {
		f.nextCompleteSeq = f.nextCompleteSeq[:queueCount]
		clear(f.nextCompleteSeq)
	}
	f.completions = make(map[fsCompletionKey]fsCompletion)
}

func encodeFuseAttr(dst []byte, attr FuseAttr) {
	binary.LittleEndian.PutUint64(dst[0:8], attr.Ino)
	binary.LittleEndian.PutUint64(dst[8:16], attr.Size)
	binary.LittleEndian.PutUint64(dst[16:24], attr.Blocks)
	binary.LittleEndian.PutUint64(dst[24:32], attr.ATimeSec)
	binary.LittleEndian.PutUint64(dst[32:40], attr.MTimeSec)
	binary.LittleEndian.PutUint64(dst[40:48], attr.CTimeSec)
	binary.LittleEndian.PutUint32(dst[48:52], attr.ATimeNsec)
	binary.LittleEndian.PutUint32(dst[52:56], attr.MTimeNsec)
	binary.LittleEndian.PutUint32(dst[56:60], attr.CTimeNsec)
	binary.LittleEndian.PutUint32(dst[60:64], attr.Mode)
	binary.LittleEndian.PutUint32(dst[64:68], attr.NLink)
	binary.LittleEndian.PutUint32(dst[68:72], attr.UID)
	binary.LittleEndian.PutUint32(dst[72:76], attr.GID)
	binary.LittleEndian.PutUint32(dst[76:80], attr.RDev)
	binary.LittleEndian.PutUint32(dst[80:84], attr.BlkSize)
	binary.LittleEndian.PutUint32(dst[84:88], attr.Flags)
}

func encodeFuseStatx(dst []byte, attr FuseAttr) {
	blkSize := attr.BlkSize
	if blkSize == 0 {
		blkSize = 4096
	}
	binary.LittleEndian.PutUint32(dst[0:4], statxBasicStats)
	binary.LittleEndian.PutUint32(dst[4:8], blkSize)
	binary.LittleEndian.PutUint32(dst[16:20], attr.NLink)
	binary.LittleEndian.PutUint32(dst[20:24], attr.UID)
	binary.LittleEndian.PutUint32(dst[24:28], attr.GID)
	binary.LittleEndian.PutUint16(dst[28:30], uint16(attr.Mode))
	binary.LittleEndian.PutUint64(dst[32:40], attr.Ino)
	binary.LittleEndian.PutUint64(dst[40:48], attr.Size)
	binary.LittleEndian.PutUint64(dst[48:56], attr.Blocks)
	encodeFuseStatxTime(dst[64:80], attr.ATimeSec, attr.ATimeNsec)
	encodeFuseStatxTime(dst[96:112], attr.CTimeSec, attr.CTimeNsec)
	encodeFuseStatxTime(dst[112:128], attr.MTimeSec, attr.MTimeNsec)
}

func encodeFuseStatxTime(dst []byte, sec uint64, nsec uint32) {
	binary.LittleEndian.PutUint64(dst[0:8], sec)
	binary.LittleEndian.PutUint32(dst[8:12], nsec)
}

func readCStringName(buf []byte) string {
	if i := bytesIndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf)
}

func readTwoCStringNames(buf []byte) (string, string, bool) {
	firstEnd := bytesIndexByte(buf, 0)
	if firstEnd < 0 {
		return "", "", false
	}
	second := buf[firstEnd+1:]
	secondEnd := bytesIndexByte(second, 0)
	if secondEnd < 0 {
		return "", "", false
	}
	return string(buf[:firstEnd]), string(second[:secondEnd]), true
}

func cleanChildName(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if strings.IndexByte(name, '/') < 0 && name != "." && name != ".." {
		return name, true
	}
	clean := path.Clean("/" + name)
	if clean == "/" {
		return "", false
	}
	return strings.TrimPrefix(clean, "/"), true
}

func bytesIndexByte(buf []byte, want byte) int {
	for i, b := range buf {
		if b == want {
			return i
		}
	}
	return -1
}

func align8(n int) int {
	return (n + 7) &^ 7
}
