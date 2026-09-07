package virtio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/linuxabi"
)

const (
	linuxEBUSY = linuxabi.EBUSY
	linuxEXDEV = linuxabi.EXDEV
	linuxEROFS = linuxabi.EROFS
)

type ShareMount struct {
	GuestPath string
	Backend   FSBackend
	Writable  bool
	CacheMode string
}

type mountedFS struct {
	root           FSBackend
	mounts         []shareMount
	writebackCache bool
	debugPaths     []string
	debugLog       io.Writer
	closing        bool
	closeMu        sync.Mutex
	closeStart     sync.Once
	closeBackends  []FSBackend
	closeStates    []mountedBackendCloseState
	closeBeginDone chan struct{}

	mu                       sync.RWMutex
	nextNodeID               uint64
	nextInode                uint64
	nextHandle               uint64
	nodes                    map[uint64]*mountedNode
	pathToNode               map[string]uint64
	pathRefs                 map[uint64]uint64
	backendNodes             map[mountedBackendNode]uint64
	inodes                   map[mountedBackendInode]uint64
	handles                  map[uint64]*mountedHandle
	backingDataHighWater     uint64
	backingMetadataHighWater uint64
}

type mountedBackendCloseState struct {
	done   chan struct{}
	err    error
	closed bool
}

func (m *mountedFS) distinctBackends() []FSBackend {
	m.mu.RLock()
	backends := make([]FSBackend, 0, len(m.mounts)+1)
	if m.root != nil {
		backends = append(backends, m.root)
	}
	for _, mount := range m.mounts {
		duplicate := false
		for _, existing := range backends {
			if sameFSBackend(existing, mount.backend) {
				duplicate = true
				break
			}
		}
		if !duplicate && mount.backend != nil {
			backends = append(backends, mount.backend)
		}
	}
	m.mu.RUnlock()
	return backends
}

func sameFSBackend(a, b FSBackend) bool {
	if a == nil || b == nil || reflect.TypeOf(a) != reflect.TypeOf(b) {
		return a == nil && b == nil
	}
	typ := reflect.TypeOf(a)
	if typ.Comparable() {
		return reflect.ValueOf(a).Interface() == reflect.ValueOf(b).Interface()
	}
	return backendPointer(a) != 0 && backendPointer(a) == backendPointer(b)
}

// BackingUsage forwards optional backing-store telemetry through the mount
// router. Each distinct backend is counted once even when it is mounted at
// multiple guest paths.
func (m *mountedFS) BackingUsage() (current, highWater, physical uint64, reclaimErr error) {
	var errs []error
	componentHighWater := uint64(0)
	for _, backend := range m.distinctBackends() {
		provider, ok := backend.(interface {
			BackingUsage() (uint64, uint64, uint64, error)
		})
		if !ok {
			continue
		}
		backendCurrent, backendHighWater, backendPhysical, err := provider.BackingUsage()
		current += backendCurrent
		componentHighWater = max(componentHighWater, backendHighWater)
		physical += backendPhysical
		if err != nil {
			errs = append(errs, err)
		}
	}
	m.mu.Lock()
	observed := max(current, componentHighWater)
	if observed > m.backingDataHighWater {
		m.backingDataHighWater = observed
	}
	highWater = m.backingDataHighWater
	m.mu.Unlock()
	return current, highWater, physical, errors.Join(errs...)
}

func (m *mountedFS) BackingCurrent() (current uint64) {
	for _, backend := range m.distinctBackends() {
		if provider, ok := backend.(interface{ BackingCurrent() uint64 }); ok {
			current += provider.BackingCurrent()
			continue
		}
		if provider, ok := backend.(interface {
			BackingUsage() (uint64, uint64, uint64, error)
		}); ok {
			value, _, _, _ := provider.BackingUsage()
			current += value
		}
	}
	return current
}

// BackingMetadataUsage forwards metadata telemetry independently from backing
// data. Keeping the two peaks separate matters: their maxima need not occur at
// the same time, so adding them invents a combined high-water value that was
// never observed.
func (m *mountedFS) BackingMetadataUsage() (current, highWater uint64) {
	componentHighWater := uint64(0)
	for _, backend := range m.distinctBackends() {
		provider, ok := backend.(interface{ BackingMetadataUsage() (uint64, uint64) })
		if !ok {
			continue
		}
		backendCurrent, backendHighWater := provider.BackingMetadataUsage()
		current += backendCurrent
		componentHighWater = max(componentHighWater, backendHighWater)
	}
	m.mu.Lock()
	observed := max(current, componentHighWater)
	if observed > m.backingMetadataHighWater {
		m.backingMetadataHighWater = observed
	}
	highWater = m.backingMetadataHighWater
	m.mu.Unlock()
	return current, highWater
}

func (m *mountedFS) PersistentFSStatus() []PersistentFSStatus {
	var statuses []PersistentFSStatus
	for _, backend := range m.distinctBackends() {
		if provider, ok := backend.(interface{ PersistentFSStatus() []PersistentFSStatus }); ok {
			statuses = append(statuses, provider.PersistentFSStatus()...)
		}
	}
	return statuses
}

// Close deterministically releases the root and every distinct mounted
// backend. Virtio FS may call this from more than one shutdown path, so the
// ownership boundary is idempotent.
func (m *mountedFS) Close() error {
	return m.closeWithin(2 * time.Second)
}

func (m *mountedFS) closeWithin(timeout time.Duration) error {
	if m == nil {
		return nil
	}
	started := time.Now()
	m.BeginClose()
	remaining := timeout - time.Since(started)
	if remaining <= 0 {
		return &CloseIncompleteError{Resource: "mounted filesystem shutdown signal", Timeout: timeout}
	}
	timer := time.NewTimer(remaining)
	select {
	case <-m.closeBeginDone:
		timer.Stop()
	case <-timer.C:
		return &CloseIncompleteError{Resource: "mounted filesystem shutdown signal", Timeout: timeout}
	}

	type attempt struct {
		index int
		done  chan struct{}
	}
	m.closeMu.Lock()
	attempts := make([]attempt, 0, len(m.closeBackends))
	for i, backend := range m.closeBackends {
		state := &m.closeStates[i]
		if state.closed {
			continue
		}
		if state.done == nil {
			state.done = make(chan struct{})
			done := state.done
			go func(state *mountedBackendCloseState, backend FSBackend) {
				var err error
				if closer, ok := backend.(interface{ Close() error }); ok {
					err = closer.Close()
				}
				m.closeMu.Lock()
				state.err = err
				m.closeMu.Unlock()
				close(done)
			}(state, backend)
		}
		attempts = append(attempts, attempt{index: i, done: state.done})
	}
	m.closeMu.Unlock()

	deadline := started.Add(timeout)
	for _, attempt := range attempts {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &CloseIncompleteError{Resource: "mounted filesystem backends", Timeout: timeout}
		}
		timer := time.NewTimer(remaining)
		select {
		case <-attempt.done:
			timer.Stop()
		case <-timer.C:
			return &CloseIncompleteError{Resource: "mounted filesystem backends", Timeout: timeout}
		}
	}

	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	var errs []error
	for i := range m.closeStates {
		state := &m.closeStates[i]
		if state.closed {
			if state.err != nil {
				errs = append(errs, state.err)
			}
			continue
		}
		var ownedAttempt chan struct{}
		for _, attempt := range attempts {
			if attempt.index == i {
				ownedAttempt = attempt.done
				break
			}
		}
		if state.done != ownedAttempt {
			errs = append(errs, &CloseIncompleteError{Resource: "mounted filesystem backend retry", Timeout: timeout})
			continue
		}
		var incomplete *CloseIncompleteError
		if errors.As(state.err, &incomplete) {
			errs = append(errs, state.err)
			state.done = nil
			state.err = nil
			continue
		}
		state.closed = true
		if state.err != nil {
			errs = append(errs, state.err)
		}
	}
	return errors.Join(errs...)
}

func (m *mountedFS) BeginClose() {
	if m == nil {
		return
	}
	m.closeStart.Do(func() {
		m.mu.Lock()
		m.closing = true
		m.mu.Unlock()
		m.closeBackends = m.distinctBackends()
		m.closeStates = make([]mountedBackendCloseState, len(m.closeBackends))
		m.closeBeginDone = make(chan struct{})
		var wg sync.WaitGroup
		for _, backend := range m.closeBackends {
			starter, ok := backend.(interface{ BeginClose() })
			if !ok {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				starter.BeginClose()
			}()
		}
		go func() {
			wg.Wait()
			close(m.closeBeginDone)
		}()
	})
}

type shareMount struct {
	path     string
	backend  FSBackend
	writable bool
	cache    string
}

type mountedNode struct {
	id              uint64
	path            string
	backend         FSBackend
	backendNodeID   uint64
	backendResolved bool
	backendRoute    string
}

type mountedHandle struct {
	backend FSBackend
	route   string
	nodeID  uint64
	fh      uint64
	dir     bool
	entries []dirEntry
	started bool
}

type mountedBackendInode struct {
	backendType string
	backendPtr  uintptr
	inode       uint64
}

type mountedBackendNode struct {
	backendType string
	backendPtr  uintptr
	route       string
	nodeID      uint64
}

func NewMountedFS(root FSBackend, shares []ShareMount) FSBackend {
	if root == nil {
		root = NewImageFS(imagefs.NewHostFS("", nil), "")
	}
	mounts := make([]shareMount, 0, len(shares))
	for _, share := range shares {
		mountPath := cleanMountPath(share.GuestPath)
		if share.Backend == nil {
			continue
		}
		if mountPath == "/" {
			continue
		}
		mounts = append(mounts, shareMount{
			path:     mountPath,
			backend:  share.Backend,
			writable: share.Writable,
			cache:    normalizeMountCacheMode(share.CacheMode),
		})
	}
	sort.Slice(mounts, func(i, j int) bool {
		if len(mounts[i].path) == len(mounts[j].path) {
			return mounts[i].path < mounts[j].path
		}
		return len(mounts[i].path) < len(mounts[j].path)
	})
	return &mountedFS{
		root:       root,
		mounts:     mounts,
		debugPaths: virtioFSDebugPathsFromEnv(),
		debugLog:   os.Stderr,
		nextNodeID: 2,
		nextInode:  1 << 32,
		nextHandle: 1,
		nodes: map[uint64]*mountedNode{
			1: {id: 1, path: "/"},
		},
		pathToNode:   map[string]uint64{"/": 1},
		pathRefs:     map[uint64]uint64{1: 1},
		backendNodes: map[mountedBackendNode]uint64{},
		inodes:       map[mountedBackendInode]uint64{},
		handles:      map[uint64]*mountedHandle{},
	}
}

type ShareMounter interface {
	AddShare(ShareMount) error
}

type ShareBatchMounter interface {
	AddShares([]ShareMount) error
}

func (m *mountedFS) RootSnapshot() (imagefs.Directory, error) {
	return m.RootSnapshotAt("/")
}

func (m *mountedFS) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	return m.RootSnapshotAtContext(ctx, "/")
}

func (m *mountedFS) RootSnapshotAt(guestPath string) (imagefs.Directory, error) {
	guestPath = cleanMountPath(guestPath)
	if guestPath == "/" {
		if snap, ok := m.root.(interface {
			RootSnapshot() (imagefs.Directory, error)
		}); ok {
			return snap.RootSnapshot()
		}
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	m.mu.RLock()
	for i := range m.mounts {
		if m.mounts[i].path != guestPath {
			continue
		}
		backend := m.mounts[i].backend
		m.mu.RUnlock()
		if snap, ok := backend.(interface {
			RootSnapshot() (imagefs.Directory, error)
		}); ok {
			return snap.RootSnapshot()
		}
		return nil, fmt.Errorf("mount %q cannot be snapshotted", guestPath)
	}
	m.mu.RUnlock()
	return nil, fmt.Errorf("mount %q is not available", guestPath)
}

func (m *mountedFS) RootSnapshotAtContext(ctx context.Context, guestPath string) (imagefs.Directory, error) {
	guestPath = cleanMountPath(guestPath)
	if guestPath == "/" {
		if snap, ok := m.root.(interface {
			RootSnapshotContext(context.Context) (imagefs.Directory, error)
		}); ok {
			return snap.RootSnapshotContext(ctx)
		}
		return nil, fmt.Errorf("root filesystem does not support cancelable snapshots")
	}
	m.mu.RLock()
	for i := range m.mounts {
		mount := &m.mounts[i]
		if mount.path != guestPath {
			continue
		}
		backend := mount.backend
		m.mu.RUnlock()
		if snap, ok := backend.(interface {
			RootSnapshotContext(context.Context) (imagefs.Directory, error)
		}); ok {
			return snap.RootSnapshotContext(ctx)
		}
		return nil, fmt.Errorf("mount %q does not support cancelable snapshots", guestPath)
	}
	m.mu.RUnlock()
	return nil, fmt.Errorf("mount %q is not available", guestPath)
}

func (m *mountedFS) AddShare(share ShareMount) error {
	return m.AddShares([]ShareMount{share})
}

func (m *mountedFS) AddShares(shares []ShareMount) error {
	m.mu.RLock()
	writebackCache := m.writebackCache
	m.mu.RUnlock()
	prepared := make([]shareMount, 0, len(shares))
	for _, share := range shares {
		mountPath := cleanMountPath(share.GuestPath)
		if mountPath == "/" || share.Backend == nil {
			continue
		}
		if be, ok := share.Backend.(fsWritebackCacheBackend); ok {
			be.SetWritebackCache(writebackCache)
		}
		prepared = append(prepared, shareMount{path: mountPath, backend: share.Backend, writable: share.Writable, cache: normalizeMountCacheMode(share.CacheMode)})
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return fmt.Errorf("mounted filesystem is closing; share ownership remains with the caller")
	}
	combined := append(append([]shareMount(nil), m.mounts...), prepared...)
	seen := make(map[string]shareMount, len(combined))
	for _, mount := range combined {
		if existing, ok := seen[mount.path]; ok {
			if existing.writable != mount.writable || !sameFSBackend(existing.backend, mount.backend) || existing.cache != mount.cache {
				return fmt.Errorf("mount path %q is already in use", mount.path)
			}
			continue
		}
		seen[mount.path] = mount
	}
	m.mounts = m.mounts[:0]
	for _, mount := range seen {
		m.mounts = append(m.mounts, mount)
	}
	sort.Slice(m.mounts, func(i, j int) bool {
		if len(m.mounts[i].path) == len(m.mounts[j].path) {
			return m.mounts[i].path < m.mounts[j].path
		}
		return len(m.mounts[i].path) < len(m.mounts[j].path)
	})
	for _, mount := range prepared {
		removed := make(map[uint64]struct{})
		for existingPath, id := range m.pathToNode {
			if existingPath != mount.path && !strings.HasPrefix(existingPath, strings.TrimSuffix(mount.path, "/")+"/") {
				continue
			}
			m.unbindPathLocked(existingPath)
			removed[id] = struct{}{}
		}
		for id := range removed {
			m.deleteNodeIfDetachedLocked(id)
		}
	}
	return nil
}

func normalizeMountCacheMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case fsCacheAggressive:
		return fsCacheAggressive
	case fsCacheNormal:
		return fsCacheNormal
	case fsCacheStrict, "":
		return fsCacheStrict
	default:
		return fsCacheStrict
	}
}

func (m *mountedFS) SetWritebackCache(enabled bool) {
	m.mu.Lock()
	m.writebackCache = enabled
	root := m.root
	backends := make([]FSBackend, 0, len(m.mounts))
	for _, mount := range m.mounts {
		backends = append(backends, mount.backend)
	}
	m.mu.Unlock()

	if be, ok := root.(fsWritebackCacheBackend); ok {
		be.SetWritebackCache(enabled)
	}
	for _, backend := range backends {
		if be, ok := backend.(fsWritebackCacheBackend); ok {
			be.SetWritebackCache(enabled)
		}
	}
}

func (m *mountedFS) SyncFS() int32 {
	m.mu.RLock()
	backends := make([]FSBackend, 0, len(m.mounts)+1)
	backends = append(backends, m.root)
	for _, mount := range m.mounts {
		duplicate := false
		for _, backend := range backends {
			if sameFSBackend(backend, mount.backend) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			backends = append(backends, mount.backend)
		}
	}
	m.mu.RUnlock()
	for _, backend := range backends {
		if syncer, ok := backend.(fsSyncFSBackend); ok {
			if errno := syncer.SyncFS(); errno != 0 {
				return errno
			}
		}
	}
	return 0
}

func (m *mountedFS) CachePolicy(nodeID uint64) FSCachePolicy {
	node := m.node(nodeID)
	if node == nil {
		return cachePolicyForMode(fsCacheStrict)
	}
	if mount := m.mountForPath(node.path); mount != nil {
		return cachePolicyForMode(mount.cache)
	}
	// Root mutations all pass through this FUSE mount, and hard links share one
	// FUSE node. The kernel can therefore update cached inode metadata coherently
	// while directory traversal retains the batched READDIRPLUS results.
	return FSCachePolicy{Mode: fsCacheNormal, EntryTTL: 60 * time.Second, AttrTTL: 60 * time.Second}
}

func cleanMountPath(value string) string {
	if value == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(value, "/"))
}

func (m *mountedFS) Init() (uint32, uint32) {
	return m.root.Init()
}

func (m *mountedFS) SnapshotNodePaths() []string {
	m.mu.RLock()
	ids := make([]int, 0, len(m.nodes))
	for id := range m.nodes {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	paths := make([]string, 0, len(m.pathToNode))
	for _, id := range ids {
		nodeID := uint64(id)
		node := m.nodes[nodeID]
		if node == nil || node.path == "" {
			continue
		}
		paths = append(paths, node.path)
		var aliases []string
		for nodePath, mappedID := range m.pathToNode {
			if mappedID == nodeID && nodePath != node.path {
				aliases = append(aliases, nodePath)
			}
		}
		sort.Strings(aliases)
		paths = append(paths, aliases...)
	}
	m.mu.RUnlock()
	return paths
}

func (m *mountedFS) RestoreNodePaths(paths []string) error {
	for _, nodePath := range paths {
		nodePath = path.Clean("/" + strings.TrimPrefix(nodePath, "/"))
		if nodePath == "/" {
			continue
		}
		if err := m.restoreNodePath(nodePath); err != nil {
			return err
		}
	}
	return nil
}

func (m *mountedFS) restoreNodePath(nodePath string) error {
	parentPath, name := path.Split(nodePath)
	parentPath = path.Clean(parentPath)
	if parentPath == "." {
		parentPath = "/"
	}
	parentID := m.nodeIDForPath(parentPath)
	if parentID == 0 {
		if err := m.restoreNodePath(parentPath); err != nil {
			return err
		}
		parentID = m.nodeIDForPath(parentPath)
		if parentID == 0 {
			return fmt.Errorf("restore mountedfs node %q: parent %q was not created", nodePath, parentPath)
		}
	}
	childID, _, errno := m.Lookup(parentID, name)
	if errno != 0 {
		childID, _, errno = m.Mkdir(parentID, name, 0o755, 0, 0)
		if errno != 0 && errno != -linuxabi.EEXIST {
			return fmt.Errorf("restore mountedfs node %q: lookup errno %d", nodePath, errno)
		}
		if errno == -linuxabi.EEXIST {
			childID, _, errno = m.Lookup(parentID, name)
			if errno != 0 {
				return fmt.Errorf("restore mountedfs node %q: lookup after mkdir errno %d", nodePath, errno)
			}
		}
	}
	if mappedID := m.nodeIDForPath(nodePath); mappedID != childID {
		return fmt.Errorf("restore mountedfs node %q: mapped to node %d, want %d", nodePath, mappedID, childID)
	}
	return nil
}

func (m *mountedFS) nodeIDForPath(nodePath string) uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pathToNode[nodePath]
}

func (m *mountedFS) GetAttr(nodeID uint64) (FuseAttr, int32) {
	node := m.node(nodeID)
	if node == nil {
		return FuseAttr{}, -linuxENOENT
	}
	attr, errno := m.resolveAttr(node)
	if errno != 0 {
		return FuseAttr{}, errno
	}
	return attr, 0
}

func (m *mountedFS) Lookup(parent uint64, name string) (uint64, FuseAttr, int32) {
	parentNode := m.node(parent)
	if parentNode == nil {
		return 0, FuseAttr{}, -linuxENOENT
	}
	name = path.Base(path.Clean("/" + name))
	switch name {
	case ".", "/":
		attr, errno := m.GetAttr(parent)
		return parent, attr, errno
	case "..":
		if parentNode.path == "/" {
			attr, errno := m.GetAttr(1)
			return 1, attr, errno
		}
		parentPath := path.Dir(parentNode.path)
		id := m.ensureNode(parentPath)
		attr, errno := m.GetAttr(id)
		return id, attr, errno
	}

	childPath := path.Join(parentNode.path, name)
	if m.isSyntheticPath(childPath) {
		attr := syntheticDirAttr()
		id := m.ensureNode(childPath)
		attr.Ino = id
		return id, attr, 0
	}
	backend, backendNodeID, _, errno := m.resolveBackendNode(childPath)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	attr, errno := backend.GetAttr(backendNodeID)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	id := m.ensureResolvedNode(childPath, backend, backendNodeID)
	attr = m.backendAttr(backend, backendNodeID, attr)
	return id, attr, 0
}

func (m *mountedFS) Open(nodeID uint64, flags uint32) (uint64, int32) {
	return m.OpenForCaller(nodeID, flags, 0, 0)
}

func (m *mountedFS) OpenForCaller(nodeID uint64, flags uint32, uid uint32, gid uint32) (uint64, int32) {
	node := m.node(nodeID)
	if node == nil {
		return 0, -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno != 0 {
		return 0, errno
	}
	var fh uint64
	if callerBE, ok := backend.(fsOpenCallerBackend); ok {
		fh, errno = callerBE.OpenForCaller(backendNodeID, flags, uid, gid)
	} else {
		fh, errno = backend.Open(backendNodeID, flags)
	}
	if errno != 0 {
		return 0, errno
	}
	return m.storeHandle(node.path, backend, backendNodeID, fh, false, nil), 0
}

func (m *mountedFS) Release(nodeID uint64, fh uint64) {
	handle := m.takeHandle(fh, false)
	if handle == nil {
		return
	}
	handle.backend.Release(handle.nodeID, handle.fh)
	m.collectDetachedNode(nodeID)
}

func (m *mountedFS) Read(nodeID uint64, fh uint64, off uint64, size uint32) ([]byte, int32) {
	handle := m.handle(fh, false)
	if handle == nil {
		return nil, -linuxEBADF
	}
	return handle.backend.Read(handle.nodeID, handle.fh, off, size)
}

func (m *mountedFS) GetXattr(nodeID uint64, name string) ([]byte, int32) {
	node := m.node(nodeID)
	if node == nil {
		return nil, -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno != 0 {
		return nil, errno
	}
	xattrBackend, ok := backend.(fsXattrBackend)
	if !ok {
		return nil, -linuxENODATA
	}
	return xattrBackend.GetXattr(backendNodeID, name)
}

func (m *mountedFS) ListXattr(nodeID uint64) ([]byte, int32) {
	node := m.node(nodeID)
	if node == nil {
		return nil, -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno != 0 {
		return nil, errno
	}
	xattrBackend, ok := backend.(fsXattrBackend)
	if !ok {
		return nil, 0
	}
	return xattrBackend.ListXattr(backendNodeID)
}

func (m *mountedFS) SetXattr(nodeID uint64, name string, value []byte, flags uint32) int32 {
	node := m.node(nodeID)
	if node == nil {
		return -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno != 0 {
		return errno
	}
	xattrBackend, ok := backend.(fsXattrMutationBackend)
	if !ok {
		return -linuxabi.ENOSYS
	}
	return xattrBackend.SetXattr(backendNodeID, name, value, flags)
}

func (m *mountedFS) RemoveXattr(nodeID uint64, name string) int32 {
	node := m.node(nodeID)
	if node == nil {
		return -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno != 0 {
		return errno
	}
	xattrBackend, ok := backend.(fsXattrMutationBackend)
	if !ok {
		return -linuxabi.ENOSYS
	}
	return xattrBackend.RemoveXattr(backendNodeID, name)
}

func (m *mountedFS) OpenDir(nodeID uint64, flags uint32) (uint64, int32) {
	node := m.node(nodeID)
	if node == nil {
		return 0, -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno == 0 {
		fh, errno := backend.OpenDir(backendNodeID, flags)
		if errno != 0 {
			return 0, errno
		}
		entries, errno := m.snapshotDirEntries(node, backend, backendNodeID, fh)
		if errno != 0 {
			backend.ReleaseDir(backendNodeID, fh)
			return 0, errno
		}
		return m.storeHandle(node.path, backend, backendNodeID, fh, true, entries), 0
	}
	if errno != -linuxENOENT || !m.isSyntheticPath(node.path) {
		return 0, errno
	}
	entries, errno := m.snapshotDirEntries(node, nil, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return m.storeHandle(node.path, nil, 0, 0, true, entries), 0
}

func (m *mountedFS) ReadDir(nodeID uint64, fh uint64, off uint64, maxBytes uint32) ([]byte, int32) {
	node := m.node(nodeID)
	if node == nil {
		return nil, -linuxENOENT
	}
	handle := m.handle(fh, true)
	if handle == nil {
		return nil, -linuxEBADF
	}
	m.mu.Lock()
	refresh := off == 0 && handle.started
	if off == 0 {
		handle.started = true
	}
	entries := append([]dirEntry(nil), handle.entries...)
	m.mu.Unlock()
	if refresh {
		refreshed, errno := m.snapshotDirEntries(node, handle.backend, handle.nodeID, handle.fh)
		if errno != 0 {
			return nil, errno
		}
		m.mu.Lock()
		if current := m.handles[fh]; current == handle {
			handle.entries = refreshed
			entries = append(entries[:0], refreshed...)
		} else {
			m.mu.Unlock()
			return nil, -linuxEBADF
		}
		m.mu.Unlock()
	}
	return encodeDirEntries(entries, off, maxBytes), 0
}

func (m *mountedFS) snapshotDirEntries(node *mountedNode, backend FSBackend, backendNodeID uint64, fh uint64) ([]dirEntry, int32) {

	entries := []dirEntry{
		{name: ".", typ: dirTypeDir, ino: node.id},
		{name: "..", typ: dirTypeDir, ino: m.ensureNode(parentPath(node.path))},
	}

	names := map[string]dirEntry{}
	if backend != nil {
		children, errno := m.readBackendDir(backend, backendNodeID, fh)
		if errno != 0 {
			return nil, errno
		}
		for _, child := range children {
			if child.name == "." || child.name == ".." {
				continue
			}
			childPath := path.Join(node.path, child.name)
			childID := m.ensureResolvedNode(childPath, backend, child.ino)
			child.ino = childID
			names[child.name] = child
		}
	}
	for _, mountChild := range m.mountChildren(node.path) {
		childID := m.ensureNode(path.Join(node.path, mountChild))
		names[mountChild] = dirEntry{name: mountChild, typ: dirTypeDir, ino: childID}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		entries = append(entries, names[name])
	}
	return entries, 0
}

func (m *mountedFS) ReleaseDir(nodeID uint64, fh uint64) {
	handle := m.takeHandle(fh, true)
	if handle == nil || handle.backend == nil {
		return
	}
	handle.backend.ReleaseDir(handle.nodeID, handle.fh)
	m.collectDetachedNode(nodeID)
}

func (m *mountedFS) Readlink(nodeID uint64) (string, int32) {
	node := m.node(nodeID)
	if node == nil {
		return "", -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno != 0 {
		return "", errno
	}
	return backend.Readlink(backendNodeID)
}

func (m *mountedFS) StatFS(nodeID uint64) (uint64, uint64, uint64, uint64, uint64, uint64, uint64, uint64, int32) {
	node := m.node(nodeID)
	if node == nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, -linuxENOENT
	}
	backend, backendNodeID, errno := m.resolveBackendNodeCached(node.path)
	if errno != 0 {
		return m.root.StatFS(1)
	}
	return backend.StatFS(backendNodeID)
}

func (m *mountedFS) Mkdir(parent uint64, name string, mode uint32, uid uint32, gid uint32) (uint64, FuseAttr, int32) {
	parentNode := m.node(parent)
	if parentNode == nil {
		return 0, FuseAttr{}, -linuxENOENT
	}
	childPath := path.Join(parentNode.path, path.Base(path.Clean("/"+name)))
	backend, backendParent, mount, errno := m.resolveBackendNode(parentNode.path)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	if !isRootOrWritableMount(mount) {
		return 0, FuseAttr{}, -linuxEROFS
	}
	mkdirBackend, ok := backend.(fsMkdirBackend)
	if !ok {
		return 0, FuseAttr{}, -linuxEROFS
	}
	nodeID, attr, errno := mkdirBackend.Mkdir(backendParent, name, mode, uid, gid)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	id := m.ensureResolvedNode(childPath, backend, nodeID)
	attr = m.backendAttr(backend, nodeID, attr)
	return id, attr, 0
}

func (m *mountedFS) Mknod(parent uint64, name string, mode uint32, rdev uint32, uid uint32, gid uint32) (uint64, FuseAttr, int32) {
	parentNode := m.node(parent)
	if parentNode == nil {
		return 0, FuseAttr{}, -linuxENOENT
	}
	childName, ok := cleanChildName(name)
	if !ok {
		return 0, FuseAttr{}, -linuxEINVAL
	}
	childPath := path.Join(parentNode.path, path.Base(childName))
	if m.isMountPath(childPath) || m.isSyntheticPath(childPath) {
		return 0, FuseAttr{}, -linuxEEXIST
	}
	backend, backendParent, mount, errno := m.resolveBackendNode(parentNode.path)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	if !isRootOrWritableMount(mount) {
		return 0, FuseAttr{}, -linuxEROFS
	}
	mknodBackend, ok := backend.(fsMknodBackend)
	if !ok {
		return 0, FuseAttr{}, -linuxEROFS
	}
	backendNodeID, attr, errno := mknodBackend.Mknod(backendParent, childName, mode, rdev, uid, gid)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	id := m.ensureResolvedNode(childPath, backend, backendNodeID)
	attr = m.backendAttr(backend, backendNodeID, attr)
	return id, attr, 0
}

func (m *mountedFS) Symlink(parent uint64, name string, target string, uid uint32, gid uint32) (uint64, FuseAttr, int32) {
	parentNode := m.node(parent)
	if parentNode == nil {
		return 0, FuseAttr{}, -linuxENOENT
	}
	childName, ok := cleanChildName(name)
	if !ok {
		return 0, FuseAttr{}, -linuxEINVAL
	}
	childPath := path.Join(parentNode.path, path.Base(childName))
	if m.isMountPath(childPath) || m.isSyntheticPath(childPath) {
		return 0, FuseAttr{}, -linuxEEXIST
	}
	backend, backendParent, mount, errno := m.resolveBackendNode(parentNode.path)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	if !isRootOrWritableMount(mount) {
		return 0, FuseAttr{}, -linuxEROFS
	}
	symlinkBackend, ok := backend.(fsSymlinkBackend)
	if !ok {
		return 0, FuseAttr{}, -linuxEROFS
	}
	backendNodeID, attr, errno := symlinkBackend.Symlink(backendParent, childName, target, uid, gid)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	id := m.ensureResolvedNode(childPath, backend, backendNodeID)
	attr = m.backendAttr(backend, backendNodeID, attr)
	return id, attr, 0
}

func (m *mountedFS) Link(nodeID uint64, newParent uint64, newName string) (uint64, FuseAttr, int32) {
	return m.LinkForCaller(nodeID, newParent, newName, 0, 0)
}

func (m *mountedFS) LinkForCaller(nodeID uint64, newParent uint64, newName string, uid uint32, gid uint32) (uint64, FuseAttr, int32) {
	node := m.node(nodeID)
	parentNode := m.node(newParent)
	if node == nil || parentNode == nil {
		return 0, FuseAttr{}, -linuxENOENT
	}
	childName, ok := cleanChildName(newName)
	if !ok {
		return 0, FuseAttr{}, -linuxEINVAL
	}
	childPath := path.Join(parentNode.path, path.Base(childName))
	if m.isMountPath(childPath) || m.isSyntheticPath(childPath) {
		return 0, FuseAttr{}, -linuxEEXIST
	}
	backend, backendNodeID, mount, errno := m.resolveBackendNode(node.path)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	_, newParentID, newMount, errno := m.resolveBackendNode(parentNode.path)
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	if !sameWritableMount(mount, newMount) {
		return 0, FuseAttr{}, -linuxEXDEV
	}
	linkBackend, ok := backend.(fsLinkBackend)
	if !ok {
		return 0, FuseAttr{}, -linuxEROFS
	}
	var backendLinkedID uint64
	var attr FuseAttr
	if callerBE, ok := backend.(fsLinkCallerBackend); ok {
		backendLinkedID, attr, errno = callerBE.LinkForCaller(backendNodeID, newParentID, childName, uid, gid)
	} else {
		backendLinkedID, attr, errno = linkBackend.Link(backendNodeID, newParentID, childName)
	}
	if errno != 0 {
		return 0, FuseAttr{}, errno
	}
	id := m.ensureResolvedNode(childPath, backend, backendLinkedID)
	attr = m.backendAttr(backend, backendLinkedID, attr)
	return id, attr, 0
}

func (m *mountedFS) RmDir(parent uint64, name string) int32 {
	return m.RmDirForCaller(parent, name, 0, 0)
}

func (m *mountedFS) RmDirForCaller(parent uint64, name string, uid uint32, gid uint32) int32 {
	parentNode := m.node(parent)
	if parentNode == nil {
		return -linuxENOENT
	}
	childPath := path.Join(parentNode.path, path.Base(path.Clean("/"+name)))
	m.debugPathf("rmdir", childPath, "parent=%q", parentNode.path)
	if m.isMountPath(childPath) || m.isSyntheticPath(childPath) {
		m.debugPathf("rmdir-error", childPath, "errno=%d", -linuxEBUSY)
		return -linuxEBUSY
	}
	backend, backendParent, mount, errno := m.resolveBackendNode(parentNode.path)
	if errno != 0 {
		m.debugPathf("rmdir-error", childPath, "resolve_parent_errno=%d", errno)
		return errno
	}
	if !isRootOrWritableMount(mount) {
		m.debugPathf("rmdir-error", childPath, "errno=%d", -linuxEROFS)
		return -linuxEROFS
	}
	rmBackend, ok := backend.(fsRmDirBackend)
	if !ok {
		m.debugPathf("rmdir-error", childPath, "errno=%d", -linuxEROFS)
		return -linuxEROFS
	}
	if callerBE, ok := backend.(fsRmDirCallerBackend); ok {
		errno = callerBE.RmDirForCaller(backendParent, name, uid, gid)
	} else {
		errno = rmBackend.RmDir(backendParent, name)
	}
	if errno != 0 {
		m.debugPathf("rmdir-error", childPath, "backend_errno=%d", errno)
		return errno
	}
	m.removeNode(childPath)
	return 0
}

func (m *mountedFS) Create(parent uint64, name string, flags uint32, mode uint32, uid uint32, gid uint32) (uint64, uint64, FuseAttr, int32) {
	return m.CreateForCaller(parent, name, flags, mode, uid, gid)
}

func (m *mountedFS) CreateForCaller(parent uint64, name string, flags uint32, mode uint32, uid uint32, gid uint32) (uint64, uint64, FuseAttr, int32) {
	parentNode := m.node(parent)
	if parentNode == nil {
		return 0, 0, FuseAttr{}, -linuxENOENT
	}
	childName, ok := cleanChildName(name)
	if !ok {
		return 0, 0, FuseAttr{}, -linuxEINVAL
	}
	childPath := path.Join(parentNode.path, path.Base(childName))
	m.debugPathf("create", childPath, "parent=%q flags=%#x mode=%#o uid=%d gid=%d", parentNode.path, flags, mode, uid, gid)
	if m.isMountPath(childPath) || m.isSyntheticPath(childPath) {
		m.debugPathf("create-error", childPath, "errno=%d", -linuxEEXIST)
		return 0, 0, FuseAttr{}, -linuxEEXIST
	}
	backend, backendParent, mount, errno := m.resolveBackendNode(parentNode.path)
	if errno != 0 {
		m.debugPathf("create-error", childPath, "resolve_parent_errno=%d", errno)
		return 0, 0, FuseAttr{}, errno
	}
	if !isRootOrWritableMount(mount) {
		m.debugPathf("create-error", childPath, "errno=%d", -linuxEROFS)
		return 0, 0, FuseAttr{}, -linuxEROFS
	}
	createBackend, ok := backend.(fsCreateBackend)
	if !ok {
		m.debugPathf("create-error", childPath, "errno=%d", -linuxEROFS)
		return 0, 0, FuseAttr{}, -linuxEROFS
	}
	var backendNodeID uint64
	var fh uint64
	var attr FuseAttr
	if callerBE, ok := backend.(fsCreateCallerBackend); ok {
		backendNodeID, fh, attr, errno = callerBE.CreateForCaller(backendParent, childName, flags, mode, uid, gid)
	} else {
		backendNodeID, fh, attr, errno = createBackend.Create(backendParent, childName, flags, mode, uid, gid)
	}
	if errno != 0 {
		m.debugPathf("create-error", childPath, "backend_errno=%d", errno)
		return 0, 0, FuseAttr{}, errno
	}
	id := m.ensureResolvedNode(childPath, backend, backendNodeID)
	attr = m.backendAttr(backend, backendNodeID, attr)
	return id, m.storeHandle(childPath, backend, backendNodeID, fh, false, nil), attr, 0
}

func (m *mountedFS) Write(nodeID uint64, fh uint64, off uint64, data []byte, flags uint32) (uint32, int32) {
	return m.WriteForCaller(nodeID, fh, off, data, flags, 0, 0)
}

func (m *mountedFS) WriteForCaller(nodeID uint64, fh uint64, off uint64, data []byte, flags uint32, uid uint32, gid uint32) (uint32, int32) {
	handle := m.handle(fh, false)
	if handle == nil {
		return 0, -linuxEBADF
	}
	writeBackend, ok := handle.backend.(fsWriteBackend)
	if !ok {
		return 0, -linuxEROFS
	}
	if callerBE, ok := handle.backend.(fsWriteCallerBackend); ok {
		return callerBE.WriteForCaller(handle.nodeID, handle.fh, off, data, flags, uid, gid)
	}
	return writeBackend.Write(handle.nodeID, handle.fh, off, data, flags)
}

func (m *mountedFS) SetAttr(nodeID uint64, valid uint32, fh uint64, size uint64, mode uint32, uid uint32, gid uint32, atime time.Time, mtime time.Time) (FuseAttr, int32) {
	return m.SetAttrForCaller(nodeID, valid, fh, size, mode, uid, gid, atime, mtime, 0, 0)
}

func (m *mountedFS) SetAttrForCaller(nodeID uint64, valid uint32, fh uint64, size uint64, mode uint32, uid uint32, gid uint32, atime time.Time, mtime time.Time, callerUID uint32, callerGID uint32) (FuseAttr, int32) {
	node := m.node(nodeID)
	if node == nil {
		return FuseAttr{}, -linuxENOENT
	}
	backend, backendNodeID, mount, errno := m.resolveBackendNode(node.path)
	if errno != 0 {
		return FuseAttr{}, errno
	}
	if !isRootOrWritableMount(mount) {
		return FuseAttr{}, -linuxEROFS
	}
	setAttrBackend, ok := backend.(fsSetAttrBackend)
	if !ok {
		return FuseAttr{}, -linuxEROFS
	}
	var backendFH uint64
	if valid&fattrFH != 0 {
		handle := m.handle(fh, false)
		if handle == nil {
			return FuseAttr{}, -linuxEBADF
		}
		backendFH = handle.fh
	}
	var attr FuseAttr
	if callerBE, ok := backend.(fsSetAttrCallerBackend); ok {
		attr, errno = callerBE.SetAttrForCaller(backendNodeID, valid, backendFH, size, mode, uid, gid, atime, mtime, callerUID, callerGID)
	} else {
		attr, errno = setAttrBackend.SetAttr(backendNodeID, valid, backendFH, size, mode, uid, gid, atime, mtime)
	}
	if errno != 0 {
		return FuseAttr{}, errno
	}
	attr = m.backendAttr(backend, backendNodeID, attr)
	return attr, 0
}

func (m *mountedFS) Unlink(parent uint64, name string) int32 {
	return m.UnlinkForCaller(parent, name, 0, 0)
}

func (m *mountedFS) UnlinkForCaller(parent uint64, name string, uid uint32, gid uint32) int32 {
	parentNode := m.node(parent)
	if parentNode == nil {
		return -linuxENOENT
	}
	childPath := path.Join(parentNode.path, path.Base(path.Clean("/"+name)))
	m.debugPathf("unlink", childPath, "parent=%q", parentNode.path)
	if m.isMountPath(childPath) || m.isSyntheticPath(childPath) {
		m.debugPathf("unlink-error", childPath, "errno=%d", -linuxEBUSY)
		return -linuxEBUSY
	}
	backend, backendParent, mount, errno := m.resolveBackendNode(parentNode.path)
	if errno != 0 {
		m.debugPathf("unlink-error", childPath, "resolve_parent_errno=%d", errno)
		return errno
	}
	if !isRootOrWritableMount(mount) {
		m.debugPathf("unlink-error", childPath, "errno=%d", -linuxEROFS)
		return -linuxEROFS
	}
	unlinkBackend, ok := backend.(fsUnlinkBackend)
	if !ok {
		m.debugPathf("unlink-error", childPath, "errno=%d", -linuxEROFS)
		return -linuxEROFS
	}
	if callerBE, ok := backend.(fsUnlinkCallerBackend); ok {
		errno = callerBE.UnlinkForCaller(backendParent, name, uid, gid)
	} else {
		errno = unlinkBackend.Unlink(backendParent, name)
	}
	if errno != 0 {
		m.debugPathf("unlink-error", childPath, "backend_errno=%d", errno)
		return errno
	}
	m.removeNode(childPath)
	return 0
}

func (m *mountedFS) Rename(parent uint64, name string, newParent uint64, newName string, flags uint32) int32 {
	return m.RenameForCaller(parent, name, newParent, newName, flags, 0, 0)
}

func (m *mountedFS) RenameForCaller(parent uint64, name string, newParent uint64, newName string, flags uint32, uid uint32, gid uint32) int32 {
	oldParentNode := m.node(parent)
	newParentNode := m.node(newParent)
	if oldParentNode == nil || newParentNode == nil {
		return -linuxENOENT
	}
	oldPath := path.Join(oldParentNode.path, path.Base(path.Clean("/"+name)))
	newPath := path.Join(newParentNode.path, path.Base(path.Clean("/"+newName)))
	m.debugPathf("rename", oldPath, "new=%q flags=%#x", newPath, flags)
	m.debugPathf("rename-target", newPath, "old=%q flags=%#x", oldPath, flags)
	if m.isMountPath(oldPath) || m.isSyntheticPath(oldPath) || m.isMountPath(newPath) || m.isSyntheticPath(newPath) {
		m.debugPathf("rename-error", oldPath, "new=%q errno=%d", newPath, -linuxEBUSY)
		return -linuxEBUSY
	}
	oldBackend, oldParentID, oldMount, errno := m.resolveBackendNode(oldParentNode.path)
	if errno != 0 {
		m.debugPathf("rename-error", oldPath, "resolve_old_parent_errno=%d", errno)
		return errno
	}
	_, newParentID, newMount, errno := m.resolveBackendNode(newParentNode.path)
	if errno != 0 {
		m.debugPathf("rename-error", newPath, "resolve_new_parent_errno=%d", errno)
		return errno
	}
	if !sameWritableMount(oldMount, newMount) {
		m.debugPathf("rename-error", oldPath, "new=%q errno=%d", newPath, -linuxEXDEV)
		return -linuxEXDEV
	}
	renameBackend, ok := oldBackend.(fsRenameBackend)
	if !ok {
		m.debugPathf("rename-error", oldPath, "new=%q errno=%d", newPath, -linuxEROFS)
		return -linuxEROFS
	}
	if callerBE, ok := oldBackend.(fsRenameCallerBackend); ok {
		errno = callerBE.RenameForCaller(oldParentID, name, newParentID, newName, flags, uid, gid)
	} else {
		errno = renameBackend.Rename(oldParentID, name, newParentID, newName, flags)
	}
	if errno != 0 {
		m.debugPathf("rename-error", oldPath, "new=%q backend_errno=%d", newPath, errno)
		return errno
	}
	m.renameNode(oldPath, newPath)
	return 0
}

func (m *mountedFS) Flush(nodeID uint64, fh uint64, lockOwner uint64) int32 {
	handle := m.handle(fh, false)
	if handle == nil {
		return -linuxEBADF
	}
	flushBackend, ok := handle.backend.(fsFlushBackend)
	if !ok {
		return 0
	}
	return flushBackend.Flush(handle.nodeID, handle.fh, lockOwner)
}

func isRootOrWritableMount(mount *shareMount) bool {
	return mount == nil || mount.writable
}

func sameWritableMount(a, b *shareMount) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.writable && b.writable && a.path == b.path
}

func (m *mountedFS) Fsync(nodeID uint64, fh uint64, flags uint32) int32 {
	handle := m.handle(fh, false)
	if handle == nil {
		return -linuxEBADF
	}
	fsyncBackend, ok := handle.backend.(fsFsyncBackend)
	if !ok {
		return 0
	}
	return fsyncBackend.Fsync(handle.nodeID, handle.fh, flags)
}

func (m *mountedFS) FsyncDir(nodeID uint64, fh uint64, flags uint32) int32 {
	handle := m.handle(fh, true)
	if handle == nil {
		return -linuxEBADF
	}
	fsyncBackend, ok := handle.backend.(fsFsyncDirBackend)
	if !ok {
		return 0
	}
	return fsyncBackend.FsyncDir(handle.nodeID, handle.fh, flags)
}

func (m *mountedFS) Lseek(nodeID uint64, fh uint64, offset uint64, whence uint32) (uint64, int32) {
	handle := m.handle(fh, false)
	if handle == nil {
		return 0, -linuxEBADF
	}
	lseekBackend, ok := handle.backend.(fsLseekBackend)
	if !ok {
		return 0, -linuxEINVAL
	}
	return lseekBackend.Lseek(handle.nodeID, handle.fh, offset, whence)
}

func (m *mountedFS) resolveAttr(node *mountedNode) (FuseAttr, int32) {
	if node.path == "/" {
		attr, errno := m.root.GetAttr(1)
		if errno != 0 {
			return FuseAttr{}, errno
		}
		attr.Ino = 1
		return attr, 0
	}
	if m.isSyntheticPath(node.path) {
		return syntheticDirAttr(), 0
	}
	backend, nodeID, errno := node.backend, node.backendNodeID, int32(0)
	if !node.backendResolved {
		backend, nodeID, errno = m.resolveBackendNodeCached(node.path)
	}
	if errno != 0 {
		return FuseAttr{}, errno
	}
	attr, errno := backend.GetAttr(nodeID)
	if errno != 0 {
		return FuseAttr{}, errno
	}
	return m.backendAttr(backend, nodeID, attr), 0
}

func (m *mountedFS) backendAttr(backend FSBackend, backendNodeID uint64, attr FuseAttr) FuseAttr {
	if attr.Ino == 0 {
		attr.Ino = backendNodeID
	}
	key := mountedBackendInode{
		backendType: reflect.TypeOf(backend).String(),
		backendPtr:  backendPointer(backend),
		inode:       attr.Ino,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if inode, ok := m.inodes[key]; ok {
		attr.Ino = inode
		return attr
	}
	inode := m.nextInode
	m.nextInode++
	m.inodes[key] = inode
	attr.Ino = inode
	return attr
}

func backendPointer(backend FSBackend) uintptr {
	v := reflect.ValueOf(backend)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		if v.IsNil() {
			return 0
		}
		return v.Pointer()
	default:
		return 0
	}
}

func (m *mountedFS) resolveBackendNodeCached(guestPath string) (FSBackend, uint64, int32) {
	guestPath = cleanMountPath(guestPath)
	if guestPath == "/" {
		return m.root, 1, 0
	}
	if node := m.nodeForPath(guestPath); node != nil && node.backendResolved {
		return node.backend, node.backendNodeID, 0
	}
	backend, nodeID, _, errno := m.resolveBackendNode(guestPath)
	return backend, nodeID, errno
}

func (m *mountedFS) resolveBackendNode(guestPath string) (FSBackend, uint64, *shareMount, int32) {
	guestPath = cleanMountPath(guestPath)
	if guestPath == "/" {
		return m.root, 1, nil, 0
	}
	mount := m.mountForPath(guestPath)
	if node := m.nodeForPath(guestPath); node != nil && node.backendResolved {
		return node.backend, node.backendNodeID, mount, 0
	}
	if mount == nil {
		nodeID, _, errno := backendLookupPath(m.root, guestPath)
		return m.root, nodeID, nil, errno
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(guestPath, mount.path), "/")
	if rel == "" {
		return mount.backend, 1, mount, 0
	}
	nodeID, _, errno := backendLookupPath(mount.backend, "/"+rel)
	return mount.backend, nodeID, mount, errno
}

func (m *mountedFS) mountForPath(guestPath string) *shareMount {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *shareMount
	for i := range m.mounts {
		mount := &m.mounts[i]
		if guestPath == mount.path || strings.HasPrefix(guestPath, mount.path+"/") {
			if best == nil || len(mount.path) > len(best.path) {
				best = mount
			}
		}
	}
	if best == nil {
		return nil
	}
	copy := *best
	return &copy
}

func (m *mountedFS) isMountPath(guestPath string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.mounts {
		if m.mounts[i].path == guestPath {
			return true
		}
	}
	return false
}

func (m *mountedFS) isSyntheticPath(guestPath string) bool {
	if guestPath == "/" {
		return false
	}
	if m.isMountPath(guestPath) {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.mounts {
		mount := m.mounts[i].path
		if strings.HasPrefix(mount, guestPath+"/") {
			return true
		}
	}
	return false
}

func (m *mountedFS) mountChildren(parent string) []string {
	parent = cleanMountPath(parent)
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := map[string]struct{}{}
	for _, mount := range m.mounts {
		if parent == "/" {
			trimmed := strings.TrimPrefix(mount.path, "/")
			if trimmed == "" {
				continue
			}
			name := strings.Split(trimmed, "/")[0]
			set[name] = struct{}{}
			continue
		}
		if !strings.HasPrefix(mount.path, parent+"/") {
			continue
		}
		rel := strings.TrimPrefix(mount.path, parent+"/")
		if rel == "" {
			continue
		}
		name := strings.Split(rel, "/")[0]
		set[name] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *mountedFS) readBackendDir(backend FSBackend, nodeID uint64, fh uint64) ([]dirEntry, int32) {
	var entries []dirEntry
	offset := uint64(0)
	for {
		data, errno := backend.ReadDir(nodeID, fh, offset, 1<<20)
		if errno != 0 {
			return nil, errno
		}
		if len(data) == 0 {
			return entries, 0
		}
		page, next, ok := decodeDirEntries(data)
		if !ok || len(page) == 0 || next <= offset {
			return nil, -linuxEIO
		}
		entries = append(entries, page...)
		offset = next
	}
}

func encodeDirEntries(entries []dirEntry, off uint64, maxBytes uint32) []byte {
	var out []byte
	for i := int(off); i < len(entries); i++ {
		entry := entries[i]
		nameBytes := []byte(entry.name)
		reclen := align8(fuseDirentBaseSize + len(nameBytes))
		if len(out)+reclen > int(maxBytes) {
			break
		}
		start := len(out)
		out = append(out, make([]byte, reclen)...)
		putLE64(out[start:start+8], entry.ino)
		putLE64(out[start+8:start+16], uint64(i+1))
		putLE32(out[start+16:start+20], uint32(len(nameBytes)))
		putLE32(out[start+20:start+24], entry.typ)
		copy(out[start+24:start+24+len(nameBytes)], nameBytes)
	}
	return out
}

func decodeDirEntries(data []byte) ([]dirEntry, uint64, bool) {
	var entries []dirEntry
	var next uint64
	for len(data) >= fuseDirentBaseSize {
		ino := readLE64(data[0:8])
		next = readLE64(data[8:16])
		nameLen := int(readLE32(data[16:20]))
		typ := readLE32(data[20:24])
		reclen := align8(fuseDirentBaseSize + nameLen)
		if nameLen < 0 || len(data) < reclen {
			return nil, 0, false
		}
		name := string(data[24 : 24+nameLen])
		entries = append(entries, dirEntry{name: name, typ: typ, ino: ino})
		data = data[reclen:]
	}
	return entries, next, len(data) == 0
}

func backendLookupPath(backend FSBackend, guestPath string) (uint64, FuseAttr, int32) {
	guestPath = cleanMountPath(guestPath)
	if guestPath == "/" {
		attr, errno := backend.GetAttr(1)
		return 1, attr, errno
	}
	nodeID := uint64(1)
	var attr FuseAttr
	for _, part := range strings.Split(strings.TrimPrefix(guestPath, "/"), "/") {
		var errno int32
		nodeID, attr, errno = backend.Lookup(nodeID, part)
		if errno != 0 {
			return 0, FuseAttr{}, errno
		}
	}
	return nodeID, attr, 0
}

func syntheticDirAttr() FuseAttr {
	modTime := time.Unix(0, 0)
	return FuseAttr{
		ATimeSec:  uint64(modTime.Unix()),
		MTimeSec:  uint64(modTime.Unix()),
		CTimeSec:  uint64(modTime.Unix()),
		ATimeNsec: uint32(modTime.Nanosecond()),
		MTimeNsec: uint32(modTime.Nanosecond()),
		CTimeNsec: uint32(modTime.Nanosecond()),
		Mode:      linuxSIFDIR | 0o755,
		NLink:     2,
		UID:       0,
		GID:       0,
		BlkSize:   4096,
	}
}

func (m *mountedFS) node(id uint64) *mountedNode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nodes[id]
}

func (m *mountedFS) ensureNode(guestPath string) uint64 {
	return m.ensureResolvedNode(guestPath, nil, 0)
}

func (m *mountedFS) ensureResolvedNode(guestPath string, backend FSBackend, backendNodeID uint64) uint64 {
	guestPath = cleanMountPath(guestPath)
	m.mu.Lock()
	defer m.mu.Unlock()
	var key mountedBackendNode
	if backend != nil {
		key = m.backendNodeKeyLocked(guestPath, backend, backendNodeID)
		if id := m.backendNodes[key]; id != 0 {
			m.bindPathLocked(guestPath, id)
			return id
		}
	}
	if id, ok := m.pathToNode[guestPath]; ok {
		if node := m.nodes[id]; node != nil && backend != nil {
			node.backend = backend
			node.backendNodeID = backendNodeID
			node.backendResolved = true
			node.backendRoute = m.backendRouteForPathLocked(guestPath)
			m.backendNodes[key] = id
		}
		return id
	}
	id := m.nextNodeID
	m.nextNodeID++
	m.nodes[id] = &mountedNode{
		id:              id,
		path:            guestPath,
		backend:         backend,
		backendNodeID:   backendNodeID,
		backendResolved: backend != nil,
		backendRoute:    m.backendRouteForPathLocked(guestPath),
	}
	m.bindPathLocked(guestPath, id)
	if backend != nil {
		m.backendNodes[key] = id
	}
	return id
}

func (m *mountedFS) bindPathLocked(guestPath string, nodeID uint64) {
	var detached uint64
	if oldID, ok := m.pathToNode[guestPath]; ok {
		if oldID == nodeID {
			return
		}
		if m.pathRefs[oldID] > 1 {
			m.pathRefs[oldID]--
		} else {
			delete(m.pathRefs, oldID)
			detached = oldID
		}
	}
	m.pathToNode[guestPath] = nodeID
	m.pathRefs[nodeID]++
	if detached != 0 {
		m.deleteNodeIfDetachedLocked(detached)
	}
}

func (m *mountedFS) unbindPathLocked(guestPath string) uint64 {
	nodeID, ok := m.pathToNode[guestPath]
	if !ok {
		return 0
	}
	delete(m.pathToNode, guestPath)
	if m.pathRefs[nodeID] > 1 {
		m.pathRefs[nodeID]--
		if node := m.nodes[nodeID]; node != nil && node.path == guestPath {
			for remainingPath, remainingID := range m.pathToNode {
				if remainingID == nodeID {
					node.path = remainingPath
					break
				}
			}
		}
	} else {
		delete(m.pathRefs, nodeID)
	}
	return nodeID
}

func (m *mountedFS) deleteNodeIfDetachedLocked(nodeID uint64) {
	if m.pathRefs[nodeID] != 0 || m.nodeHasHandleLocked(m.nodes[nodeID]) {
		return
	}
	for key, mappedID := range m.backendNodes {
		if mappedID == nodeID {
			delete(m.backendNodes, key)
		}
	}
	delete(m.nodes, nodeID)
}

func (m *mountedFS) backendNodeKeyLocked(guestPath string, backend FSBackend, backendNodeID uint64) mountedBackendNode {
	return mountedBackendNode{
		backendType: reflect.TypeOf(backend).String(),
		backendPtr:  backendPointer(backend),
		route:       m.backendRouteForPathLocked(guestPath),
		nodeID:      backendNodeID,
	}
}

func (m *mountedFS) nodeForPath(guestPath string) *mountedNode {
	guestPath = cleanMountPath(guestPath)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nodes[m.pathToNode[guestPath]]
}

func (m *mountedFS) removeNode(guestPath string) {
	guestPath = cleanMountPath(guestPath)
	m.debugPathf("cache-remove", guestPath, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := make(map[uint64]struct{})
	for existingPath, id := range m.pathToNode {
		if existingPath != guestPath && !strings.HasPrefix(existingPath, strings.TrimSuffix(guestPath, "/")+"/") {
			continue
		}
		m.unbindPathLocked(existingPath)
		removed[id] = struct{}{}
	}
	for id := range removed {
		m.deleteNodeIfDetachedLocked(id)
	}
}

func (m *mountedFS) nodeHasHandleLocked(node *mountedNode) bool {
	if node == nil || !node.backendResolved {
		return false
	}
	for _, handle := range m.handles {
		if handle.route == node.backendRoute && handle.nodeID == node.backendNodeID {
			return true
		}
	}
	return false
}

func (m *mountedFS) collectDetachedNode(nodeID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	node := m.nodes[nodeID]
	if node == nil || m.nodeHasHandleLocked(node) {
		return
	}
	if m.pathRefs[nodeID] != 0 {
		return
	}
	delete(m.nodes, nodeID)
}

func (m *mountedFS) renameNode(oldPath, newPath string) {
	oldPath = cleanMountPath(oldPath)
	newPath = cleanMountPath(newPath)
	m.debugPathf("cache-rename", oldPath, "new=%q", newPath)
	m.debugPathf("cache-rename-target", newPath, "old=%q", oldPath)
	m.mu.Lock()
	defer m.mu.Unlock()
	updates := map[string]uint64{}
	for existingPath, id := range m.pathToNode {
		if existingPath != oldPath && !strings.HasPrefix(existingPath, strings.TrimSuffix(oldPath, "/")+"/") {
			continue
		}
		suffix := strings.TrimPrefix(existingPath, oldPath)
		updates[cleanMountPath(newPath+suffix)] = id
		m.unbindPathLocked(existingPath)
	}
	for updatedPath, id := range updates {
		m.bindPathLocked(updatedPath, id)
		if node := m.nodes[id]; node != nil {
			node.path = updatedPath
		}
	}
}

func (m *mountedFS) storeHandle(guestPath string, backend FSBackend, nodeID uint64, fh uint64, dir bool, entries []dirEntry) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextHandle
	m.nextHandle++
	m.handles[id] = &mountedHandle{backend: backend, route: m.backendRouteForPathLocked(guestPath), nodeID: nodeID, fh: fh, dir: dir, entries: entries}
	return id
}

func (m *mountedFS) backendRouteForPathLocked(guestPath string) string {
	guestPath = cleanMountPath(guestPath)
	best := "/"
	for i := range m.mounts {
		mount := &m.mounts[i]
		if (guestPath == mount.path || strings.HasPrefix(guestPath, mount.path+"/")) && len(mount.path) > len(best) {
			best = mount.path
		}
	}
	return best
}

func (m *mountedFS) handle(id uint64, dir bool) *mountedHandle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	handle := m.handles[id]
	if handle == nil || handle.dir != dir {
		return nil
	}
	return handle
}

func (m *mountedFS) takeHandle(id uint64, dir bool) *mountedHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	handle := m.handles[id]
	if handle == nil || handle.dir != dir {
		return nil
	}
	delete(m.handles, id)
	return handle
}

func (m *mountedFS) DebugPath(nodeID uint64) string {
	node := m.node(nodeID)
	if node == nil {
		return ""
	}
	return node.path
}

func (m *mountedFS) debugPathMatch(guestPath string) bool {
	if len(m.debugPaths) == 0 || guestPath == "" {
		return false
	}
	guestPath = cleanMountPath(guestPath)
	for _, prefix := range m.debugPaths {
		prefix = cleanMountPath(prefix)
		if guestPath == prefix || strings.HasPrefix(guestPath, strings.TrimSuffix(prefix, "/")+"/") {
			return true
		}
	}
	return false
}

func (m *mountedFS) debugPathf(op string, guestPath string, format string, args ...any) {
	if m.debugLog == nil || !m.debugPathMatch(guestPath) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if msg != "" {
		msg = " " + msg
	}
	_, _ = fmt.Fprintf(m.debugLog, "virtiofs:mount %s path=%q%s\n", op, cleanMountPath(guestPath), msg)
}

func parentPath(guestPath string) string {
	guestPath = cleanMountPath(guestPath)
	if guestPath == "/" {
		return "/"
	}
	return path.Dir(guestPath)
}

func putLE32(dst []byte, v uint32) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 24)
}

func putLE64(dst []byte, v uint64) {
	putLE32(dst[0:4], uint32(v))
	putLE32(dst[4:8], uint32(v>>32))
}

func readLE32(src []byte) uint32 {
	return uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16 | uint32(src[3])<<24
}

func readLE64(src []byte) uint64 {
	return uint64(readLE32(src[0:4])) | uint64(readLE32(src[4:8]))<<32
}

var _ FSBackend = (*mountedFS)(nil)
var _ ShareMounter = (*mountedFS)(nil)
var _ fsMkdirBackend = (*mountedFS)(nil)
var _ fsRmDirBackend = (*mountedFS)(nil)
var _ fsRmDirCallerBackend = (*mountedFS)(nil)
var _ fsCreateBackend = (*mountedFS)(nil)
var _ fsCreateCallerBackend = (*mountedFS)(nil)
var _ fsWriteBackend = (*mountedFS)(nil)
var _ fsWriteCallerBackend = (*mountedFS)(nil)
var _ fsSetAttrBackend = (*mountedFS)(nil)
var _ fsSetAttrCallerBackend = (*mountedFS)(nil)
var _ fsUnlinkBackend = (*mountedFS)(nil)
var _ fsUnlinkCallerBackend = (*mountedFS)(nil)
var _ fsRenameBackend = (*mountedFS)(nil)
var _ fsRenameCallerBackend = (*mountedFS)(nil)
var _ fsOpenCallerBackend = (*mountedFS)(nil)
var _ fsLinkCallerBackend = (*mountedFS)(nil)
var _ fsFlushBackend = (*mountedFS)(nil)
var _ fsFsyncBackend = (*mountedFS)(nil)
var _ fsFsyncDirBackend = (*mountedFS)(nil)
var _ fsLseekBackend = (*mountedFS)(nil)
