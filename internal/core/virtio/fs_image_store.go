package virtio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// imageDataStore keeps writable imageFS pages in a sparse host file instead of
// Go heap allocations. The host VM process can therefore reclaim clean pages,
// and backing-filesystem exhaustion is returned to the guest as an ordinary
// write failure rather than becoming an unbounded host-heap failure.
type imageDataStore struct {
	mu           sync.Mutex
	dir          string
	file         *os.File
	path         string
	next         uint64
	nextLocation uint64
	// Stable page tokens are split into independently reclaimable chunks. A
	// high-numbered live token therefore retains one small chunk rather than
	// dense indexes for every earlier, released token.
	locationChunks          map[uint64]*imageLocationChunk
	retainedLocationChunks  int
	liveLocations           int
	free                    []uint64
	freeSet                 map[uint64]struct{}
	pendingReclaim          map[uint64]struct{}
	retainedFreeSet         int
	retainedPendingReclaim  int
	metadataHighWater       uint64
	current                 uint64
	highWater               uint64
	reclaimErr              error
	staleCleanupErr         error
	rangeReclaimUnsupported bool
	closed                  bool
	refs                    int
	stale                   []imageDataStaleFile
}

const imageLocationChunkBits = 8
const imageLocationChunkSize = 1 << imageLocationChunkBits
const imageLocationChunkMask = imageLocationChunkSize - 1

type imageLocationChunk struct {
	offsets [imageLocationChunkSize]uint64
	refs    [imageLocationChunkSize]uint32
	live    uint16
}

type imageDataStaleFile struct {
	file *os.File
	path string
}

type imageDataReclaimQueue struct {
	mu      sync.Mutex
	pending map[*imageDataStore]struct{}
	wake    chan struct{}
}

var globalImageDataReclaimer = newImageDataReclaimQueue()

func newImageDataReclaimQueue() *imageDataReclaimQueue {
	q := &imageDataReclaimQueue{pending: make(map[*imageDataStore]struct{}), wake: make(chan struct{}, 1)}
	go q.run()
	return q
}

func (q *imageDataReclaimQueue) schedule(store *imageDataStore) {
	q.mu.Lock()
	q.pending[store] = struct{}{}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *imageDataReclaimQueue) run() {
	for range q.wake {
		// Gather adjacent 4 KiB releases into filesystem-sized operations.
		time.Sleep(5 * time.Millisecond)
		q.mu.Lock()
		stores := make([]*imageDataStore, 0, len(q.pending))
		for store := range q.pending {
			stores = append(stores, store)
		}
		clear(q.pending)
		q.mu.Unlock()
		for _, store := range stores {
			store.mu.Lock()
			store.flushReclaimLocked()
			store.mu.Unlock()
		}
	}
}

func newImageDataStore() *imageDataStore {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "cc", "imagefs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		dir = os.TempDir()
	}
	return &imageDataStore{dir: dir, refs: 1, locationChunks: make(map[uint64]*imageLocationChunk), freeSet: make(map[uint64]struct{}), pendingReclaim: make(map[uint64]struct{})}
}

func (s *imageDataStore) retain() {
	s.mu.Lock()
	if !s.closed {
		s.refs++
	}
	s.mu.Unlock()
}

func (s *imageDataStore) ensureFileLocked() error {
	if s.closed {
		return os.ErrClosed
	}
	if s.file != nil {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "pages-*")
	if err != nil {
		return err
	}
	s.file = f
	s.path = f.Name()
	// Unix keeps the open inode alive after unlink, which gives us automatic
	// cleanup if ccvm is killed by the very failure this store is meant to
	// contain. Windows keeps the named path and removes it during Close.
	if err := os.Remove(s.path); err == nil {
		s.path = ""
	}
	return nil
}

func (s *imageDataStore) allocatePage(data []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocatePageLocked(data)
}

func (s *imageDataStore) allocatePageLocked(data []byte) (uint64, error) {
	if s.nextLocation == ^uint64(0) {
		return 0, fmt.Errorf("image page token space exhausted")
	}
	if err := s.ensureFileLocked(); err != nil {
		return 0, err
	}
	var offset uint64
	reused := false
	for len(s.free) > 0 {
		offset = s.free[len(s.free)-1]
		s.free = s.free[:len(s.free)-1]
		if _, ok := s.freeSet[offset]; ok {
			delete(s.freeSet, offset)
			delete(s.pendingReclaim, offset)
			reused = true
			break
		}
	}
	if !reused {
		offset = s.next
		s.next += imageDataPageSize
	}
	var page [imageDataPageSize]byte
	copy(page[:], data)
	if _, err := s.file.WriteAt(page[:], int64(offset)); err != nil {
		s.free = append(s.free, offset)
		s.freeSet[offset] = struct{}{}
		s.noteMetadataLocked()
		return 0, err
	}
	s.current += imageDataPageSize
	if s.current > s.highWater {
		s.highWater = s.current
	}
	s.nextLocation++
	location := s.nextLocation
	chunkID, slot := imageLocationIndex(location)
	chunk := s.locationChunks[chunkID]
	if chunk == nil {
		chunk = &imageLocationChunk{}
		s.locationChunks[chunkID] = chunk
		s.retainedLocationChunks = max(s.retainedLocationChunks, len(s.locationChunks))
	}
	chunk.offsets[slot] = offset + 1
	chunk.refs[slot] = 1
	chunk.live++
	s.liveLocations++
	s.noteMetadataLocked()
	return location, nil
}

func (s *imageDataStore) retainPage(location uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chunk, slot, ok := s.locationEntryLocked(location)
	if !ok || chunk.refs[slot] == 0 {
		return os.ErrNotExist
	}
	if chunk.refs[slot] == ^uint32(0) {
		return fmt.Errorf("image page reference count overflow")
	}
	chunk.refs[slot]++
	return nil
}

func (s *imageDataStore) locationOffsetLocked(location uint64) (uint64, bool) {
	chunk, slot, ok := s.locationEntryLocked(location)
	if !ok || chunk.offsets[slot] == 0 {
		return 0, false
	}
	return chunk.offsets[slot] - 1, true
}

func imageLocationIndex(location uint64) (uint64, uint64) {
	return location >> imageLocationChunkBits, location & imageLocationChunkMask
}

func (s *imageDataStore) locationEntryLocked(location uint64) (*imageLocationChunk, uint64, bool) {
	if location == 0 {
		return nil, 0, false
	}
	chunkID, slot := imageLocationIndex(location)
	chunk := s.locationChunks[chunkID]
	return chunk, slot, chunk != nil
}

func (s *imageDataStore) readPage(location uint64, dst []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		clear(dst)
		return nil
	}
	offset, ok := s.locationOffsetLocked(location)
	if !ok {
		return os.ErrNotExist
	}
	n, err := s.file.ReadAt(dst, int64(offset))
	if errors.Is(err, io.EOF) && n == len(dst) {
		err = nil
	}
	return err
}

func (s *imageDataStore) writeAtCOW(location, pageOffset uint64, data []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return 0, os.ErrNotExist
	}
	offset, ok := s.locationOffsetLocked(location)
	if !ok {
		return 0, os.ErrNotExist
	}
	chunk, slot, exists := s.locationEntryLocked(location)
	if !exists || chunk.refs[slot] == 0 {
		return 0, os.ErrNotExist
	}
	if chunk.refs[slot] == 1 {
		_, err := s.file.WriteAt(data, int64(offset+pageOffset))
		return location, err
	}
	var page [imageDataPageSize]byte
	n, err := s.file.ReadAt(page[:], int64(offset))
	if err != nil && !(errors.Is(err, io.EOF) && n == len(page)) {
		return 0, err
	}
	copy(page[pageOffset:pageOffset+uint64(len(data))], data)
	newLocation, err := s.allocatePageLocked(page[:])
	if err != nil {
		return 0, err
	}
	chunk.refs[slot]--
	return newLocation, nil
}

func (s *imageDataStore) releasePage(location uint64) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	offset, exists := s.locationOffsetLocked(location)
	if !exists {
		s.mu.Unlock()
		return
	}
	chunk, slot, exists := s.locationEntryLocked(location)
	if !exists {
		s.mu.Unlock()
		return
	}
	if chunk.refs[slot] > 1 {
		chunk.refs[slot]--
		s.mu.Unlock()
		return
	}
	chunk.offsets[slot] = 0
	chunk.refs[slot] = 0
	chunk.live--
	s.liveLocations--
	if chunk.live == 0 {
		chunkID, _ := imageLocationIndex(location)
		delete(s.locationChunks, chunkID)
		s.compactLocationChunksLocked()
	}
	s.free = append(s.free, offset)
	s.freeSet[offset] = struct{}{}
	scheduleReclaim := len(s.pendingReclaim) == 0
	s.pendingReclaim[offset] = struct{}{}
	s.noteMetadataLocked()
	if s.current >= imageDataPageSize {
		s.current -= imageDataPageSize
	}
	s.mu.Unlock()
	if scheduleReclaim {
		globalImageDataReclaimer.schedule(s)
	}
}

func (s *imageDataStore) compactLocationChunksLocked() {
	if len(s.locationChunks)*4 >= s.retainedLocationChunks || s.retainedLocationChunks < 16 && len(s.locationChunks) != 0 {
		return
	}
	rebuilt := make(map[uint64]*imageLocationChunk, len(s.locationChunks))
	for id, chunk := range s.locationChunks {
		rebuilt[id] = chunk
	}
	s.locationChunks = rebuilt
	s.retainedLocationChunks = len(rebuilt)
}

func (s *imageDataStore) flushReclaimLocked() {
	if s.file == nil || len(s.pendingReclaim) == 0 {
		return
	}
	offsets := make([]uint64, 0, len(s.pendingReclaim))
	for offset := range s.pendingReclaim {
		if _, free := s.freeSet[offset]; free {
			offsets = append(offsets, offset)
		}
	}
	clear(s.pendingReclaim)

	oldNext := s.next
	for s.next >= imageDataPageSize {
		tail := s.next - imageDataPageSize
		if _, free := s.freeSet[tail]; !free {
			break
		}
		delete(s.freeSet, tail)
		s.next = tail
	}
	if s.next != oldNext {
		if err := s.file.Truncate(int64(s.next)); err != nil && s.reclaimErr == nil {
			s.reclaimErr = err
		}
	}

	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	for i := 0; i < len(offsets); {
		start := offsets[i]
		if start >= s.next {
			i++
			continue
		}
		end := start + imageDataPageSize
		i++
		for i < len(offsets) && offsets[i] == end && end < s.next {
			end += imageDataPageSize
			i++
		}
		err := reclaimFileRange(s.file, int64(start), int64(end-start))
		if errors.Is(err, errRangeReclaimUnsupported) {
			s.rangeReclaimUnsupported = true
			break
		}
		if err != nil && s.reclaimErr == nil {
			s.reclaimErr = err
		}
	}
	// Once holes dominate the store, rewrite the live pages even on hosts which
	// support punching. This both bounds logical-file amplification and releases
	// the per-hole allocator metadata which a high live page would otherwise pin.
	if shouldCompactPortableImageStore(s.current, s.next) {
		if err := s.compactLocked(); err != nil && s.reclaimErr == nil {
			s.reclaimErr = err
		}
	}
	s.compactMetadataLocked()
}

func (s *imageDataStore) compactMetadataLocked() {
	if len(s.freeSet)*4 < s.retainedFreeSet && (s.retainedFreeSet >= 64 || len(s.freeSet) == 0) {
		freeSet := make(map[uint64]struct{}, len(s.freeSet))
		free := make([]uint64, 0, len(s.freeSet))
		for offset := range s.freeSet {
			freeSet[offset] = struct{}{}
			free = append(free, offset)
		}
		s.freeSet = freeSet
		s.free = free
		s.retainedFreeSet = len(freeSet)
	}
	if len(s.pendingReclaim)*4 < s.retainedPendingReclaim && (s.retainedPendingReclaim >= 64 || len(s.pendingReclaim) == 0) {
		pending := make(map[uint64]struct{}, len(s.pendingReclaim))
		for offset := range s.pendingReclaim {
			pending[offset] = struct{}{}
		}
		s.pendingReclaim = pending
		s.retainedPendingReclaim = len(pending)
	}
}

func shouldCompactPortableImageStore(live, allocated uint64) bool {
	if allocated < 4<<20 || allocated <= live {
		return false
	}
	dead := allocated - live
	return dead >= (live+1)/2
}

var errRangeReclaimUnsupported = errors.New("backing filesystem does not support range reclamation")

func (s *imageDataStore) compactLocked() error {
	if s.file == nil || s.liveLocations == 0 {
		return nil
	}
	f, err := os.CreateTemp(s.dir, "pages-compact-*")
	if err != nil {
		return err
	}
	path := f.Name()
	if err := os.Remove(path); err == nil {
		path = ""
	}
	cleanup := func() {
		_ = f.Close()
		if path != "" {
			_ = os.Remove(path)
		}
	}
	var page [imageDataPageSize]byte
	var next uint64
	type locationUpdate struct {
		chunk  *imageLocationChunk
		slot   int
		offset uint64
	}
	// Do not publish any replacement offsets until every live page has been
	// copied successfully. On failure the old file remains authoritative, so
	// its location table must remain byte-for-byte usable as well.
	updates := make([]locationUpdate, 0, s.liveLocations)
	for _, chunk := range s.locationChunks {
		for slot, encodedOffset := range chunk.offsets {
			if encodedOffset == 0 {
				continue
			}
			oldOffset := encodedOffset - 1
			n, readErr := s.file.ReadAt(page[:], int64(oldOffset))
			if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(page)) {
				cleanup()
				return readErr
			}
			if _, err := f.WriteAt(page[:], int64(next)); err != nil {
				cleanup()
				return err
			}
			updates = append(updates, locationUpdate{chunk: chunk, slot: slot, offset: next + 1})
			next += imageDataPageSize
		}
	}
	for _, update := range updates {
		update.chunk.offsets[update.slot] = update.offset
	}
	oldFile, oldPath := s.file, s.path
	s.file, s.path, s.next = f, path, next
	s.free = nil
	s.freeSet = make(map[uint64]struct{})
	s.pendingReclaim = make(map[uint64]struct{})
	s.retainedFreeSet = 0
	s.retainedPendingReclaim = 0
	// The replacement is live, but cleanup ownership for the old file must not
	// disappear if Windows or an external scanner makes close/removal fail
	// transiently. Retain it for later sync/usage/close retries and report the
	// failure without rolling back the valid compacted store.
	s.stale = append(s.stale, imageDataStaleFile{file: oldFile, path: oldPath})
	s.staleCleanupErr = s.cleanupStaleLocked()
	return nil
}

func (s *imageDataStore) cleanupStaleLocked() error {
	var pending []imageDataStaleFile
	var errs []error
	for _, stale := range s.stale {
		if stale.file != nil {
			if err := stale.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				errs = append(errs, fmt.Errorf("close replaced image backing file: %w", err))
				pending = append(pending, stale)
				continue
			}
			stale.file = nil
		}
		if stale.path != "" {
			if err := os.Remove(stale.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove replaced image backing file %q: %w", stale.path, err))
				pending = append(pending, stale)
			}
		}
	}
	s.stale = pending
	return errors.Join(errs...)
}

func (s *imageDataStore) usage() (current, highWater, physical uint64, reclaimErr error) {
	if s == nil {
		return 0, 0, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staleCleanupErr = s.cleanupStaleLocked()
	if s.file != nil {
		physical, reclaimErr = allocatedFileBytes(s.file)
	}
	return s.current, s.highWater, physical, errors.Join(s.reclaimErr, s.staleCleanupErr, reclaimErr)
}

func (s *imageDataStore) metadataUsage() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metadataUsageLocked()
}

func (s *imageDataStore) metadataUsageLocked() uint64 {
	usage := uint64(len(s.locationChunks))*uint64(imageLocationChunkSize)*(8+4) + uint64(cap(s.free))*8
	usage += uint64(s.retainedLocationChunks) * 32
	usage += uint64(s.retainedFreeSet+s.retainedPendingReclaim) * 16
	usage += uint64(cap(s.stale)) * 32
	for _, stale := range s.stale {
		usage += uint64(len(stale.path))
	}
	if usage > s.metadataHighWater {
		s.metadataHighWater = usage
	}
	return usage
}

func (s *imageDataStore) noteMetadataLocked() {
	s.retainedFreeSet = max(s.retainedFreeSet, len(s.freeSet))
	s.retainedPendingReclaim = max(s.retainedPendingReclaim, len(s.pendingReclaim))
	s.metadataUsageLocked()
}

func (s *imageDataStore) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushReclaimLocked()
	cleanupErr := s.cleanupStaleLocked()
	s.staleCleanupErr = cleanupErr
	if s.file == nil {
		return cleanupErr
	}
	return errors.Join(cleanupErr, s.file.Sync())
}

func (s *imageDataStore) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.refs--
	if s.refs > 0 {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.flushReclaimLocked()
	f, path, stale := s.file, s.path, s.stale
	s.file, s.path, s.locationChunks, s.free, s.freeSet, s.pendingReclaim, s.stale = nil, "", nil, nil, nil, nil, nil
	s.mu.Unlock()
	var errs []error
	if f != nil {
		errs = append(errs, f.Close())
	}
	if path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, old := range stale {
		if old.file != nil {
			if err := old.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				errs = append(errs, err)
			}
		}
		if old.path != "" {
			if err := os.Remove(old.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
