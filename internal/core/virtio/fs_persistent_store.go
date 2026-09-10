package virtio

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

const (
	persistentImageFormatVersion = 1
	persistentImageHeaderSize    = 40
	persistentImageMaxMetadata   = 1 << 30
)

var persistentImageMagic = [8]byte{'C', 'C', 'H', 'O', 'M', 'E', '0', '1'}

const persistentImageFormatFile = "cc-persistent-image-fs 1\n"

var persistentImageSaveTestHook func(string)
var persistentImageFaultTestHook func(string) error

func runPersistentImageSaveTestHook(stage string) {
	if persistentImageSaveTestHook != nil {
		persistentImageSaveTestHook(stage)
	}
}

func runPersistentImageFaultTestHook(stage string) error {
	if persistentImageFaultTestHook != nil {
		return persistentImageFaultTestHook(stage)
	}
	return nil
}

// PersistentImageFSOptions configures a durable imageFS upper layer. The lower
// directory remains immutable and is read directly until a file's first data
// mutation copies it into sparse inode-number-addressed storage.
type PersistentImageFSOptions struct {
	StoreDir   string
	Name       string
	Mount      string
	LowerID    string
	StatFSPath string
	OwnerUID   uint32
	OwnerGID   uint32
	MapOwner   bool
}

type persistentImageStore struct {
	dir             string
	name            string
	mount           string
	lowerID         string
	previousLowerID string
	sequence        uint64
	durableSequence uint64
	metadataSlot    uint64
	lock            *os.File
	pending         *persistentImagePending
	pendingTrim     map[uint64]uint64
	pendingRemove   map[uint64]struct{}
	newDataDirs     map[uint64]string
	dirtyData       map[uint64]struct{}
	durableState    *persistentImageState
	recovery        persistentImageRecovery
	dataUsage       map[uint64]uint64
	dataPhysical    map[uint64]uint64
	currentUsage    uint64
	highWaterUsage  uint64
	physicalUsage   uint64
	usageErr        error
	lastCheckpoint  time.Time
	lastErr         error
	dataSyncMu      sync.Mutex
}

type persistentImageRecovery struct {
	Status         string `json:"status,omitempty"`
	QuarantinePath string `json:"quarantine_path,omitempty"`
	DiscardedBytes uint64 `json:"discarded_bytes,omitempty"`
}

type persistentImageState struct {
	Version      uint32                  `json:"version"`
	Sequence     uint64                  `json:"sequence"`
	LowerID      string                  `json:"lower_id,omitempty"`
	NextNodeID   uint64                  `json:"next_node_id"`
	Nodes        []persistentImageNode   `json:"nodes,omitempty"`
	RemovedNodes []uint64                `json:"removed_nodes,omitempty"`
	Recovery     persistentImageRecovery `json:"recovery,omitempty"`
	LastError    string                  `json:"last_error,omitempty"`
}

type persistentImageNode struct {
	ID            uint64                  `json:"id"`
	Parent        uint64                  `json:"parent"`
	Name          []byte                  `json:"name,omitempty"`
	Kind          string                  `json:"kind"`
	Mode          uint32                  `json:"mode"`
	RawMode       uint32                  `json:"raw_mode,omitempty"`
	UID           uint32                  `json:"uid"`
	GID           uint32                  `json:"gid"`
	RDev          uint32                  `json:"rdev,omitempty"`
	Size          uint64                  `json:"size,omitempty"`
	NLink         uint32                  `json:"nlink,omitempty"`
	SymlinkTarget []byte                  `json:"symlink_target,omitempty"`
	GuestPath     []byte                  `json:"guest_path,omitempty"`
	LowerPath     []byte                  `json:"lower_path,omitempty"`
	Entries       []persistentImageDirent `json:"entries,omitempty"`
	Whiteouts     [][]byte                `json:"whiteouts,omitempty"`
	Xattrs        []persistentImageXattr  `json:"xattrs,omitempty"`
	Data          []persistentImageExtent `json:"data,omitempty"`
	EntriesDone   bool                    `json:"entries_done,omitempty"`
	CopiedUp      bool                    `json:"copied_up,omitempty"`
	Independent   bool                    `json:"independent,omitempty"`
	LowerSize     uint64                  `json:"lower_size,omitempty"`
	ATimeUnixNano int64                   `json:"atime_unix_nano,omitempty"`
	MTimeUnixNano int64                   `json:"mtime_unix_nano,omitempty"`
	CTimeUnixNano int64                   `json:"ctime_unix_nano,omitempty"`
	// PreserveNamespace is used only by WAL deltas for file fsync. File
	// durability must not accidentally make an un-fsynced rename or link
	// durable as well.
	PreserveNamespace bool `json:"preserve_namespace,omitempty"`
}

type persistentImageDirent struct {
	Name  []byte `json:"name"`
	Inode uint64 `json:"inode"`
}

type persistentImageXattr struct {
	Name  []byte `json:"name"`
	Value []byte `json:"value"`
}

type persistentImageExtent struct {
	Page  uint64 `json:"page"`
	Count uint64 `json:"count"`
}

func NewPersistentImageFS(root imagefs.Directory, opts PersistentImageFSOptions) (FSBackend, error) {
	store, state, err := openPersistentImageStore(opts.StoreDir, opts.LowerID)
	if err != nil {
		return nil, err
	}
	var backend *imageFS
	if opts.MapOwner {
		backend = newImageFS(root, opts.StatFSPath, opts.OwnerUID, opts.OwnerGID, true).(*imageFS)
	} else {
		backend = newImageFS(root, opts.StatFSPath, 0, 0, false).(*imageFS)
	}
	backend.persistent = store
	backend.persistentNodes = make(map[uint64]struct{})
	store.name = opts.Name
	store.mount = opts.Mount
	backend.persistentLower = root
	backend.root = store.dir
	if state != nil {
		store.previousLowerID = state.LowerID
		if err := backend.restorePersistentState(state); err != nil {
			_ = store.close()
			_ = backend.dataStore.close()
			return nil, fmt.Errorf("restore persistent image filesystem: %w", err)
		}
	} else {
		backend.mu.Lock()
		err := backend.checkpointPersistentLocked(true)
		backend.mu.Unlock()
		if err != nil {
			_ = store.close()
			_ = backend.dataStore.close()
			return nil, fmt.Errorf("initialize persistent image filesystem: %w", err)
		}
	}
	if err := store.markInitialized(); err != nil {
		_ = store.close()
		_ = backend.dataStore.close()
		return nil, fmt.Errorf("mark persistent image filesystem initialized: %w", err)
	}
	if err := store.markAttached(); err != nil {
		_ = store.close()
		_ = backend.dataStore.close()
		return nil, fmt.Errorf("mark persistent image filesystem attached: %w", err)
	}
	return backend, nil
}

func openPersistentImageStore(dir, lowerID string) (*persistentImageStore, *persistentImageState, error) {
	if dir == "" {
		return nil, nil, fmt.Errorf("persistent image store directory is required")
	}
	dir = filepath.Clean(dir)
	if err := preparePersistentImageStoreLayout(dir); err != nil {
		return nil, nil, err
	}
	lock, err := lockPersistentImageStore(filepath.Join(dir, "LOCK"))
	if err != nil {
		return nil, nil, err
	}
	store := &persistentImageStore{
		dir:           dir,
		lowerID:       lowerID,
		lock:          lock,
		pendingTrim:   make(map[uint64]uint64),
		pendingRemove: make(map[uint64]struct{}),
		newDataDirs:   make(map[uint64]string),
		dirtyData:     make(map[uint64]struct{}),
		dataUsage:     make(map[uint64]uint64),
		dataPhysical:  make(map[uint64]uint64),
	}
	state, err := store.load()
	if err != nil {
		_ = store.close()
		return nil, nil, err
	}
	if state == nil {
		if err := store.ensureEmptyUninitialized(); err != nil {
			_ = store.close()
			return nil, nil, err
		}
	}
	if state != nil {
		store.sequence = state.Sequence
		// Recovery describes work performed while opening this attachment. Do
		// not replay a completed recovery warning on every future startup.
		if lastError := activePersistentLastError(state.LastError); lastError != "" {
			store.lastErr = errors.New(lastError)
		}
		if err := store.replayWAL(state); err != nil {
			_ = store.close()
			return nil, nil, err
		}
		store.durableSequence = store.sequence
		store.durableState = clonePersistentImageState(state)
		if err := store.preserveUnreferencedData(state); err != nil {
			_ = store.close()
			return nil, nil, err
		}
	}
	if err := store.recoverIncompleteAttachment(); err != nil {
		_ = store.close()
		return nil, nil, err
	}
	return store, state, nil
}

func (s *persistentImageStore) preserveUnreferencedData(state *persistentImageState) error {
	if s == nil || state == nil {
		return nil
	}
	referenced := make(map[string]struct{})
	for _, node := range state.Nodes {
		if len(node.Data) != 0 {
			referenced[filepath.Clean(s.dataPath(node.ID))] = struct{}{}
		}
	}
	var orphaned []string
	dataRoot := filepath.Join(s.dir, "data")
	if err := filepath.WalkDir(dataRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if _, ok := referenced[filepath.Clean(path)]; !ok {
			orphaned = append(orphaned, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("scan persistent image data: %w", err)
	}
	if len(orphaned) == 0 {
		return nil
	}
	sort.Strings(orphaned)
	quarantine := filepath.Join(s.dir, "staging", fmt.Sprintf("unreferenced-data-%d", time.Now().UnixNano()))
	if err := os.Mkdir(quarantine, 0o700); err != nil {
		return fmt.Errorf("create unreferenced data quarantine: %w", err)
	}
	sourceDirs := make(map[string]struct{})
	for _, source := range orphaned {
		relative, err := filepath.Rel(dataRoot, source)
		if err != nil {
			return fmt.Errorf("name unreferenced data %q: %w", source, err)
		}
		target := filepath.Join(quarantine, strings.ReplaceAll(relative, string(filepath.Separator), "-"))
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("preserve unreferenced data %q: %w", source, err)
		}
		sourceDirs[filepath.Dir(source)] = struct{}{}
	}
	for sourceDir := range sourceDirs {
		if err := syncPersistentDirectory(sourceDir); err != nil {
			return fmt.Errorf("publish removal of unreferenced data from %q: %w", sourceDir, err)
		}
	}
	if err := syncPersistentDirectory(quarantine); err != nil {
		return fmt.Errorf("publish unreferenced data quarantine: %w", err)
	}
	if err := syncPersistentDirectory(filepath.Dir(quarantine)); err != nil {
		return fmt.Errorf("publish unreferenced data quarantine directory: %w", err)
	}
	if s.recovery.Status == "" {
		s.recovery.Status = "preserved-unreferenced-data"
		s.recovery.QuarantinePath = quarantine
	}
	return nil
}

func activePersistentLastError(value string) string {
	var active []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "previous persistent image attachment did not detach cleanly" {
			continue
		}
		if strings.HasPrefix(line, "preserved ") && strings.Contains(line, " unreferenced inode data files in ") {
			continue
		}
		active = append(active, line)
	}
	return strings.Join(active, "\n")
}

func (s *persistentImageStore) ensureEmptyUninitialized() error {
	if _, err := os.Stat(filepath.Join(s.dir, "INITIALIZED")); err == nil {
		return fmt.Errorf("persistent image store is initialized but both metadata snapshots are missing; refusing to create an empty filesystem")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Stat(s.walPath()); err == nil && info.Size() != 0 {
		return fmt.Errorf("persistent image metadata snapshots are missing but WAL contains %d bytes; refusing to create an empty filesystem", info.Size())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, root := range []string{filepath.Join(s.dir, "data"), filepath.Join(s.dir, "trash")} {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("persistent image metadata snapshots are missing but %q contains data; refusing to create an empty filesystem", root)
		}
	}
	return nil
}

func (s *persistentImageStore) markInitialized() error {
	path := filepath.Join(s.dir, "INITIALIZED")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, fmt.Sprintf("sequence=%d\n", s.sequence))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return syncPersistentDirectory(s.dir)
}

func (s *persistentImageStore) recoverIncompleteAttachment() error {
	path := filepath.Join(s.dir, "DIRTY")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	recoveryPath := filepath.Join(s.dir, "staging", fmt.Sprintf("incomplete-close-%d", time.Now().UnixNano()))
	if err := os.Rename(path, recoveryPath); err != nil {
		return fmt.Errorf("preserve incomplete persistent image close marker: %w", err)
	}
	if err := syncPersistentDirectory(s.dir); err != nil {
		return fmt.Errorf("publish incomplete persistent image close marker: %w", err)
	}
	if s.recovery.Status == "" {
		s.recovery.Status = "recovered-incomplete-close"
		s.recovery.QuarantinePath = recoveryPath
	}
	return nil
}

func (s *persistentImageStore) markAttached() error {
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "attached-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_, writeErr := fmt.Fprintf(tmp, "pid=%d\nsequence=%d\ntime=%s\n", os.Getpid(), s.sequence, time.Now().Format(time.RFC3339Nano))
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := replacePersistentFile(tmpPath, filepath.Join(s.dir, "DIRTY")); err != nil {
		return err
	}
	return syncPersistentDirectory(s.dir)
}

func (s *persistentImageStore) markClean() error {
	err := os.Remove(filepath.Join(s.dir, "DIRTY"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncPersistentDirectory(s.dir)
}

func preparePersistentImageStoreLayout(dir string) error {
	for _, path := range []string{dir, filepath.Join(dir, "data"), filepath.Join(dir, "staging"), filepath.Join(dir, "trash")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create persistent image store directory %q: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect persistent image store directory %q: %w", path, err)
		}
	}
	formatPath := filepath.Join(dir, "FORMAT")
	file, err := os.OpenFile(formatPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		value, readErr := os.ReadFile(formatPath)
		if readErr != nil {
			return fmt.Errorf("read persistent image format: %w", readErr)
		}
		if string(value) != persistentImageFormatFile {
			return fmt.Errorf("unsupported persistent image format %q", string(value))
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("create persistent image format: %w", err)
	}
	_, writeErr := io.WriteString(file, persistentImageFormatFile)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("initialize persistent image format: %w", err)
	}
	if err := syncPersistentDirectory(dir); err != nil {
		return fmt.Errorf("publish persistent image format: %w", err)
	}
	return nil
}

// ErrPersistentStoreInUse identifies a competing writer without matching error text.
var ErrPersistentStoreInUse = errors.New("persistent image store is in use")

func lockPersistentImageStore(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open persistent image lock: %w", err)
	}
	if err := lockPersistentFile(file); err != nil {
		_ = file.Close()
		if persistentLockBusy(err) {
			return nil, fmt.Errorf("%w: %w", ErrPersistentStoreInUse, err)
		}
		return nil, fmt.Errorf("lock persistent image store: %w", err)
	}
	return file, nil
}

func (s *persistentImageStore) close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	err := unlockPersistentFile(s.lock)
	err = errors.Join(err, s.lock.Close())
	s.lock = nil
	return err
}

func (s *persistentImageStore) metadataPath(slot uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("metadata.%d", slot&1))
}

func (s *persistentImageStore) load() (*persistentImageState, error) {
	var best *persistentImageState
	var invalid []error
	var invalidPaths []string
	for slot := uint64(0); slot < 2; slot++ {
		state, err := readPersistentImageState(s.metadataPath(slot))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			invalid = append(invalid, fmt.Errorf("metadata slot %d: %w", slot, err))
			invalidPaths = append(invalidPaths, s.metadataPath(slot))
			continue
		}
		if best == nil || state.Sequence > best.Sequence {
			best = state
			s.metadataSlot = slot
		}
	}
	if best != nil {
		if info, err := os.Stat(s.metadataPath(s.metadataSlot)); err == nil {
			s.lastCheckpoint = info.ModTime()
		}
		if len(invalidPaths) != 0 {
			s.recovery = persistentImageRecovery{
				Status:         "using-valid-metadata-fallback",
				QuarantinePath: invalidPaths[0],
			}
		}
		return best, nil
	}
	if len(invalid) != 0 {
		return nil, fmt.Errorf("no valid persistent image metadata: %w", errors.Join(invalid...))
	}
	return nil, nil
}

func readPersistentImageState(path string) (*persistentImageState, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var header [persistentImageHeaderSize]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if string(header[:8]) != string(persistentImageMagic[:]) {
		return nil, fmt.Errorf("invalid magic")
	}
	if version := binary.LittleEndian.Uint32(header[8:12]); version != persistentImageFormatVersion {
		return nil, fmt.Errorf("unsupported version %d", version)
	}
	length := binary.LittleEndian.Uint64(header[24:32])
	if length > persistentImageMaxMetadata {
		return nil, fmt.Errorf("metadata length %d exceeds maximum", length)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat metadata: %w", err)
	}
	available := info.Size() - int64(len(header))
	if available < 0 || length != uint64(available) {
		return nil, fmt.Errorf("metadata payload length is %d, file contains %d bytes", length, max(int64(0), available))
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(file, payload); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	if got, want := crc32.ChecksumIEEE(payload), binary.LittleEndian.Uint32(header[32:36]); got != want {
		return nil, fmt.Errorf("checksum mismatch: got %#x want %#x", got, want)
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || err == nil {
		return nil, fmt.Errorf("metadata has trailing data")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("check trailing data: %w", err)
	}
	var state persistentImageState
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	if state.Version != persistentImageFormatVersion {
		return nil, fmt.Errorf("payload version %d does not match", state.Version)
	}
	if state.Sequence != binary.LittleEndian.Uint64(header[16:24]) {
		return nil, fmt.Errorf("payload sequence does not match header")
	}
	return &state, nil
}

func (s *persistentImageStore) save(state *persistentImageState, durable bool) error {
	if s == nil {
		return nil
	}
	state.Version = persistentImageFormatVersion
	state.Sequence = s.sequence + 1
	state.LowerID = s.lowerID
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode persistent image metadata: %w", err)
	}
	if len(payload) > persistentImageMaxMetadata {
		return fmt.Errorf("persistent image metadata is %d bytes", len(payload))
	}
	var header [persistentImageHeaderSize]byte
	copy(header[:8], persistentImageMagic[:])
	binary.LittleEndian.PutUint32(header[8:12], persistentImageFormatVersion)
	binary.LittleEndian.PutUint64(header[16:24], state.Sequence)
	binary.LittleEndian.PutUint64(header[24:32], uint64(len(payload)))
	binary.LittleEndian.PutUint32(header[32:36], crc32.ChecksumIEEE(payload))

	slot := (s.metadataSlot + 1) & 1
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "metadata-*")
	if err != nil {
		return fmt.Errorf("create metadata staging file: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod metadata staging file: %w", err)
	}
	if _, err := tmp.Write(header[:]); err != nil {
		return fmt.Errorf("write metadata header: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		return fmt.Errorf("write metadata payload: %w", err)
	}
	runPersistentImageSaveTestHook("metadata-written")
	if durable {
		if err := tmp.Sync(); err != nil {
			return fmt.Errorf("sync metadata staging file: %w", err)
		}
	}
	runPersistentImageSaveTestHook("metadata-synced")
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close metadata staging file: %w", err)
	}
	verified, err := readPersistentImageState(tmpPath)
	if err != nil {
		return fmt.Errorf("verify metadata staging file: %w", err)
	}
	if verified.Sequence != state.Sequence || len(verified.Nodes) != len(state.Nodes) {
		return fmt.Errorf("verify metadata staging file: record count or sequence changed")
	}
	if err := runPersistentImageFaultTestHook("before-metadata-publish"); err != nil {
		return err
	}
	target := s.metadataPath(slot)
	if err := replacePersistentFile(tmpPath, target); err != nil {
		return fmt.Errorf("publish metadata slot %d: %w", slot, err)
	}
	keep = true
	runPersistentImageSaveTestHook("metadata-published")
	if durable {
		if err := syncPersistentDirectory(s.dir); err != nil {
			return fmt.Errorf("sync persistent image directory: %w", err)
		}
	}
	runPersistentImageSaveTestHook("directory-synced")
	s.sequence = state.Sequence
	s.metadataSlot = slot
	if durable {
		s.durableSequence = s.sequence
		s.lastCheckpoint = time.Now()
		s.durableState = clonePersistentImageState(state)
	}
	return nil
}

func (s *persistentImageStore) dataPath(nodeID uint64) string {
	hexID := fmt.Sprintf("%016x", nodeID)
	return filepath.Join(s.dir, "data", hexID[:2], hexID+".data")
}

func (s *persistentImageStore) openData(nodeID uint64, flags int) (*os.File, error) {
	path := s.dataPath(nodeID)
	if flags&os.O_CREATE == 0 {
		return os.OpenFile(path, flags, 0o600)
	}
	if file, err := os.OpenFile(path, flags&^os.O_CREATE, 0o600); err == nil {
		return file, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	shard := filepath.Dir(path)
	if err := os.MkdirAll(shard, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, flags|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return os.OpenFile(path, flags&^os.O_CREATE, 0o600)
	}
	if err == nil {
		s.newDataDirs[nodeID] = shard
	}
	return file, err
}

func (s *persistentImageStore) readData(nodeID uint64, data sparseImageData, dst []byte, off uint64) error {
	if len(data.extents) == 0 || len(dst) == 0 {
		return nil
	}
	file, err := s.openData(nodeID, os.O_RDONLY)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("persistent inode %d data file is missing: %w", nodeID, syscall.EIO)
		}
		return err
	}
	defer file.Close()
	for len(dst) > 0 {
		page := off / imageDataPageSize
		pageOffset := off % imageDataPageSize
		n := min(len(dst), int(imageDataPageSize-pageOffset))
		if _, ok := data.location(page); ok {
			read, err := file.ReadAt(dst[:n], int64(off))
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if read != n {
				return io.ErrUnexpectedEOF
			}
		}
		dst = dst[n:]
		off += uint64(n)
	}
	return nil
}

func (s *persistentImageStore) writeData(nodeID uint64, data *sparseImageData, src []byte, off uint64) (int, error) {
	if len(src) == 0 {
		return 0, nil
	}
	if err := runPersistentImageFaultTestHook("before-data-write"); err != nil {
		return 0, err
	}
	file, err := s.openData(nodeID, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	written, err := file.WriteAt(src, int64(off))
	if written > 0 {
		s.dirtyData[nodeID] = struct{}{}
		if trim, pending := s.pendingTrim[nodeID]; pending {
			s.pendingTrim[nodeID] = max(trim, off+uint64(written))
		}
		first := off / imageDataPageSize
		last := (off + uint64(written) - 1) / imageDataPageSize
		for page := first; page <= last; page++ {
			if _, ok := data.location(page); !ok {
				data.insert(page, page+1)
			}
		}
	}
	return written, err
}

func (s *persistentImageStore) writePage(nodeID uint64, data *sparseImageData, page uint64, payload []byte) error {
	if len(payload) > int(imageDataPageSize) {
		return fmt.Errorf("persistent image page payload is %d bytes", len(payload))
	}
	file, err := s.openData(nodeID, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteAt(payload, int64(page*imageDataPageSize)); err != nil {
		return err
	}
	s.dirtyData[nodeID] = struct{}{}
	if trim, pending := s.pendingTrim[nodeID]; pending {
		s.pendingTrim[nodeID] = max(trim, page*imageDataPageSize+uint64(len(payload)))
	}
	if _, ok := data.location(page); !ok {
		data.insert(page, page+1)
	}
	return nil
}

func (s *persistentImageStore) materializeLowerFile(nodeID uint64, data *sparseImageData, lower imagefs.File, size uint64) error {
	if lower == nil || size == 0 {
		return nil
	}
	if size > uint64(math.MaxInt64) {
		return fmt.Errorf("copy lower inode %d with size %d: %w", nodeID, size, syscall.EFBIG)
	}
	file, err := s.openData(nodeID, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer file.Close()
	const chunkSize = uint64(1 << 20)
	for chunkOffset := uint64(0); chunkOffset < size; chunkOffset += chunkSize {
		length := min(chunkSize, size-chunkOffset)
		chunk, err := lower.ReadAt(chunkOffset, uint32(length))
		if err != nil {
			return err
		}
		if uint64(len(chunk)) != length {
			return io.ErrUnexpectedEOF
		}
		for pageOffset := uint64(0); pageOffset < length; pageOffset += imageDataPageSize {
			absolute := chunkOffset + pageOffset
			page := absolute / imageDataPageSize
			if _, present := data.location(page); present {
				continue
			}
			pageData := chunk[pageOffset:min(length, pageOffset+imageDataPageSize)]
			allZero := true
			for _, value := range pageData {
				if value != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				continue
			}
			if _, err := file.WriteAt(pageData, int64(absolute)); err != nil {
				return err
			}
			s.dirtyData[nodeID] = struct{}{}
			data.insert(page, page+1)
		}
	}
	return nil
}

func (s *persistentImageStore) truncateData(nodeID uint64, data *sparseImageData, size uint64) error {
	if size > uint64(^uint64(0)>>1) {
		return fmt.Errorf("truncate inode %d to %d: %w", nodeID, size, syscall.EFBIG)
	}
	if previous, pending := s.pendingTrim[nodeID]; pending && size > previous {
		// A shrink followed by growth before the next durability barrier must
		// not reveal bytes beyond the earlier logical EOF. Only the retained
		// partial page can overlap this range; later extents were removed below.
		pageEnd := min(size, (previous/imageDataPageSize+1)*imageDataPageSize)
		if pageEnd > previous {
			page := previous / imageDataPageSize
			if _, present := data.location(page); present {
				file, err := s.openData(nodeID, os.O_RDWR)
				if err != nil {
					return err
				}
				zeroes := make([]byte, int(pageEnd-previous))
				_, writeErr := file.WriteAt(zeroes, int64(previous))
				closeErr := file.Close()
				if writeErr != nil {
					return writeErr
				}
				if closeErr != nil {
					return closeErr
				}
				s.dirtyData[nodeID] = struct{}{}
			}
		}
	}
	keepPages := size / imageDataPageSize
	if size%imageDataPageSize != 0 {
		keepPages++
	}
	kept := data.extents[:0]
	for _, extent := range data.extents {
		if extent.page >= keepPages {
			continue
		}
		extent.count = min(extent.count, keepPages-extent.page)
		if extent.count != 0 {
			extent.location = extent.page + 1
			kept = append(kept, extent)
		}
	}
	data.extents = kept
	s.pendingTrim[nodeID] = size
	s.dirtyData[nodeID] = struct{}{}
	return nil
}

func (s *persistentImageStore) retireData(nodeID uint64) {
	s.pendingRemove[nodeID] = struct{}{}
	delete(s.pendingTrim, nodeID)
}

func (s *persistentImageStore) syncData(nodeID uint64) error {
	if err := runPersistentImageFaultTestHook("before-data-sync"); err != nil {
		return err
	}
	s.dataSyncMu.Lock()
	usage := s.dataUsage[nodeID]
	shard, newData := s.newDataDirs[nodeID]
	s.dataSyncMu.Unlock()
	file, err := s.openData(nodeID, os.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		if usage != 0 {
			return fmt.Errorf("persistent inode %d data file is missing: %w", nodeID, syscall.EIO)
		}
		s.dataSyncMu.Lock()
		delete(s.dirtyData, nodeID)
		s.dataSyncMu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}
	err = file.Sync()
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if newData {
		if err := syncPersistentDirectory(shard); err != nil {
			return err
		}
		if err := syncPersistentDirectory(filepath.Join(s.dir, "data")); err != nil {
			return err
		}
	}
	s.dataSyncMu.Lock()
	delete(s.newDataDirs, nodeID)
	delete(s.dirtyData, nodeID)
	s.dataSyncMu.Unlock()
	return nil
}

func (s *persistentImageStore) syncDirtyData() error {
	if s == nil || len(s.dirtyData) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(s.dirtyData))
	for id := range s.dirtyData {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	workers := min(len(ids), max(1, min(runtime.GOMAXPROCS(0), 16)))
	jobs := make(chan int)
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if err := s.syncData(ids[index]); err != nil {
					errs[index] = fmt.Errorf("sync dirty inode %d: %w", ids[index], err)
				}
			}
		}()
	}
	for index := range ids {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return errors.Join(errs...)
}

func (s *persistentImageStore) reclaimDurableData() error {
	var errs []error
	didWork := len(s.pendingTrim) != 0 || len(s.pendingRemove) != 0
	if didWork {
		if err := runPersistentImageFaultTestHook("before-data-reclaim"); err != nil {
			return err
		}
	}
	for nodeID, size := range s.pendingTrim {
		err := os.Truncate(s.dataPath(nodeID), int64(size))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("trim inode %d: %w", nodeID, err))
			continue
		}
		delete(s.pendingTrim, nodeID)
		s.refreshPhysicalUsage(nodeID)
	}
	for nodeID := range s.pendingRemove {
		source := s.dataPath(nodeID)
		trash := filepath.Join(s.dir, "trash", fmt.Sprintf("%016x-%020d.data", nodeID, s.sequence))
		err := os.Rename(source, trash)
		if err == nil {
			runPersistentImageSaveTestHook("data-moved-to-trash")
			err = errors.Join(syncPersistentDirectory(filepath.Dir(source)), syncPersistentDirectory(filepath.Dir(trash)))
			if err == nil {
				err = os.Remove(trash)
				runPersistentImageSaveTestHook("trash-removed")
				err = errors.Join(err, syncPersistentDirectory(filepath.Dir(trash)))
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("retire inode %d data: %w", nodeID, err))
			continue
		}
		delete(s.pendingRemove, nodeID)
		delete(s.newDataDirs, nodeID)
		delete(s.dirtyData, nodeID)
		s.setDataUsage(nodeID, 0, 0)
	}
	if didWork && len(s.pendingRemove) == 0 && len(s.pendingTrim) == 0 {
		if err := syncPersistentDirectory(filepath.Join(s.dir, "data")); err != nil {
			errs = append(errs, fmt.Errorf("sync data directory after reclamation: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (s *persistentImageStore) reclaimDurableFileData(nodeID uint64) error {
	if s == nil {
		return nil
	}
	size, pending := s.pendingTrim[nodeID]
	if !pending {
		return nil
	}
	err := os.Truncate(s.dataPath(nodeID), int64(size))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("trim inode %d: %w", nodeID, err)
	}
	delete(s.pendingTrim, nodeID)
	s.refreshPhysicalUsage(nodeID)
	return nil
}

func (s *persistentImageStore) reclaimDurableNamespaceData() error {
	if s == nil || len(s.pendingRemove) == 0 {
		return nil
	}
	// Namespace fsync makes every pending unlink durable, but deliberately
	// leaves truncation reclamation to the affected file's own fsync.
	trims := s.pendingTrim
	s.pendingTrim = make(map[uint64]uint64)
	err := s.reclaimDurableData()
	s.pendingTrim = trims
	return err
}

func (s *persistentImageStore) setDataUsage(nodeID, current, physical uint64) {
	previous := s.dataUsage[nodeID]
	s.currentUsage = s.currentUsage - previous + current
	if current == 0 {
		delete(s.dataUsage, nodeID)
	} else {
		s.dataUsage[nodeID] = current
	}
	previousPhysical := s.dataPhysical[nodeID]
	s.physicalUsage = s.physicalUsage - previousPhysical + physical
	if physical == 0 {
		delete(s.dataPhysical, nodeID)
	} else {
		s.dataPhysical[nodeID] = physical
	}
	s.highWaterUsage = max(s.highWaterUsage, s.currentUsage)
}

func (s *persistentImageStore) refreshUsage(node *imageNode) {
	if node == nil {
		return
	}
	current := node.data.allocatedBytes(node.size)
	physical := s.dataPhysical[node.id]
	file, err := s.openData(node.id, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		if current != 0 {
			s.usageErr = fmt.Errorf("persistent inode %d data is missing", node.id)
		}
		physical = 0
	} else if err != nil {
		s.usageErr = errors.Join(s.usageErr, fmt.Errorf("open persistent inode %d for accounting: %w", node.id, err))
	} else {
		physical, err = allocatedFileBytes(file)
		err = errors.Join(err, file.Close())
		if err != nil {
			s.usageErr = errors.Join(s.usageErr, fmt.Errorf("account persistent inode %d: %w", node.id, err))
		}
	}
	s.setDataUsage(node.id, current, physical)
}

func (s *persistentImageStore) refreshLogicalUsage(node *imageNode) {
	if node == nil {
		return
	}
	s.setDataUsage(node.id, node.data.allocatedBytes(node.size), s.dataPhysical[node.id])
}

func (s *persistentImageStore) refreshPhysicalUsage(nodeID uint64) {
	current := s.dataUsage[nodeID]
	var physical uint64
	file, err := s.openData(nodeID, os.O_RDONLY)
	if err == nil {
		physical, err = allocatedFileBytes(file)
		err = errors.Join(err, file.Close())
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.usageErr = errors.Join(s.usageErr, fmt.Errorf("account persistent inode %d: %w", nodeID, err))
		return
	}
	s.setDataUsage(nodeID, current, physical)
}

func (p *imageFS) syncAllPersistentDataLocked() error {
	if p == nil || p.persistent == nil {
		return nil
	}
	// Data removed from dirtyData has already completed a durable file or
	// filesystem barrier. Re-syncing every referenced inode made clean shutdown
	// proportional to the entire persistent home rather than this session's
	// outstanding writes.
	return p.persistent.syncDirtyData()
}

func (p *imageFS) persistentStateLocked() *persistentImageState {
	state := &persistentImageState{
		NextNodeID: p.nextNodeID,
		Recovery:   p.persistent.recovery,
	}
	if p.persistent.lastErr != nil {
		state.LastError = p.persistent.lastErr.Error()
	}
	// Build the checkpoint from the namespace rooted at inode 1. Link counts
	// and cached parent/name fields can briefly lag an atomic replacement, but
	// the directory graph is the filesystem contract that survives restart.
	reachable := make(map[uint64]struct{}, len(p.persistentNodes))
	queue := []uint64{1}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, seen := reachable[id]; seen {
			continue
		}
		node := p.nodes[id]
		if node == nil {
			continue
		}
		if _, persistent := p.persistentNodes[id]; id != 1 && !persistent {
			continue
		}
		reachable[id] = struct{}{}
		for _, childID := range node.entries {
			queue = append(queue, childID)
		}
	}
	ids := make([]uint64, 0, len(reachable))
	for id := range reachable {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if node := p.nodes[id]; node != nil {
			state.Nodes = append(state.Nodes, persistentNodeFromImageNode(node, p.pathForNode(id), reachable))
		}
	}
	for id := range p.removedNodes {
		state.RemovedNodes = append(state.RemovedNodes, id)
	}
	sort.Slice(state.RemovedNodes, func(i, j int) bool { return state.RemovedNodes[i] < state.RemovedNodes[j] })
	return state
}

func persistentNodeFromImageNode(node *imageNode, nodePath string, persistentNodes map[uint64]struct{}) persistentImageNode {
	out := persistentImageNode{
		ID:            node.id,
		Parent:        node.parent,
		Name:          []byte(node.name),
		Mode:          uint32(node.mode),
		RawMode:       node.rawMode,
		UID:           node.uid,
		GID:           node.gid,
		RDev:          node.rdev,
		Size:          node.size,
		NLink:         node.nlink,
		SymlinkTarget: []byte(node.symlinkTarget),
		GuestPath:     []byte(nodePath),
		EntriesDone:   node.entriesDone,
		CopiedUp:      node.lowerFile != nil,
		Independent:   node.persistentIndependent,
		LowerSize:     node.lowerSize,
		ATimeUnixNano: timeToUnixNano(node.atime),
		MTimeUnixNano: timeToUnixNano(node.modTime),
		CTimeUnixNano: timeToUnixNano(node.ctime),
	}
	switch {
	case node.isDir():
		out.Kind = "dir"
	case node.isSymlink():
		out.Kind = "symlink"
	default:
		out.Kind = "file"
	}
	if node.abstractDir != nil || node.abstractFile != nil || node.abstractLink != nil || node.lowerFile != nil || node.persistentLowerPath != "" {
		lowerPath := node.persistentLowerPath
		if lowerPath == "" {
			lowerPath = nodePath
		}
		out.LowerPath = []byte(lowerPath)
	}
	for name, inode := range node.entries {
		if persistentNodes != nil {
			if _, persistent := persistentNodes[inode]; !persistent {
				continue
			}
		}
		out.Entries = append(out.Entries, persistentImageDirent{Name: []byte(name), Inode: inode})
	}
	if node.abstractDir != nil {
		out.EntriesDone = false
	}
	sort.Slice(out.Entries, func(i, j int) bool { return string(out.Entries[i].Name) < string(out.Entries[j].Name) })
	for name := range node.whiteouts {
		out.Whiteouts = append(out.Whiteouts, []byte(name))
	}
	sort.Slice(out.Whiteouts, func(i, j int) bool { return string(out.Whiteouts[i]) < string(out.Whiteouts[j]) })
	for name, value := range node.xattrs {
		out.Xattrs = append(out.Xattrs, persistentImageXattr{Name: []byte(name), Value: append([]byte(nil), value...)})
	}
	sort.Slice(out.Xattrs, func(i, j int) bool { return string(out.Xattrs[i].Name) < string(out.Xattrs[j].Name) })
	for _, extent := range node.data.extents {
		out.Data = append(out.Data, persistentImageExtent{Page: extent.page, Count: extent.count})
	}
	return out
}

func timeToUnixNano(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func unixNanoTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value)
}

func (p *imageFS) restorePersistentState(state *persistentImageState) error {
	if state.NextNodeID < 2 {
		return fmt.Errorf("invalid next node ID %d", state.NextNodeID)
	}
	p.nextNodeID = max(p.nextNodeID, state.NextNodeID)
	removedSeen := make(map[uint64]struct{}, len(state.RemovedNodes))
	for _, id := range state.RemovedNodes {
		if id == 0 || id == 1 {
			return fmt.Errorf("invalid removed node ID %d", id)
		}
		if _, exists := removedSeen[id]; exists {
			return fmt.Errorf("duplicate removed node ID %d", id)
		}
		removedSeen[id] = struct{}{}
		p.removedNodes[id] = struct{}{}
	}
	nodeSeen := make(map[uint64]struct{}, len(state.Nodes))
	savedByPath := make(map[string]*imageNode, len(state.Nodes)+1)
	savedNodes := make(map[uint64]*persistentImageNode, len(state.Nodes))
	for _, saved := range state.Nodes {
		if saved.ID == 0 || saved.ID == ^uint64(0) {
			return fmt.Errorf("invalid node ID %d", saved.ID)
		}
		if _, exists := nodeSeen[saved.ID]; exists {
			return fmt.Errorf("duplicate node ID %d", saved.ID)
		}
		nodeSeen[saved.ID] = struct{}{}
		if _, removed := removedSeen[saved.ID]; removed {
			return fmt.Errorf("node ID %d is both present and removed", saved.ID)
		}
		savedNodes[saved.ID] = &saved
	}
	reachableNodes := make(map[uint64]struct{}, len(state.Nodes))
	queue := []uint64{1}
	for len(queue) != 0 {
		id := queue[0]
		queue = queue[1:]
		if _, seen := reachableNodes[id]; seen {
			continue
		}
		reachableNodes[id] = struct{}{}
		if saved := savedNodes[id]; saved != nil {
			for _, entry := range saved.Entries {
				queue = append(queue, entry.Inode)
			}
		}
	}
	for _, saved := range state.Nodes {
		// Older writers could checkpoint an inode kept alive only by an open
		// handle, or a whole orphaned directory tree, after it had been
		// unlinked or atomically replaced. Handles cannot survive restart, so
		// restore only nodes actually reachable from the saved root directory.
		if _, reachable := reachableNodes[saved.ID]; !reachable {
			continue
		}
		guestPath := string(saved.GuestPath)
		if guestPath == "" && saved.ID == 1 {
			guestPath = "/"
		}
		if guestPath == "" || !strings.HasPrefix(guestPath, "/") || path.Clean(guestPath) != guestPath {
			return fmt.Errorf("node %d has invalid guest path %q", saved.ID, guestPath)
		}
		if existing := savedByPath[guestPath]; existing != nil && existing.id != saved.ID {
			switch {
			case existing.nlink == 0 && saved.NLink != 0:
				// Older checkpoints could include an open inode after an
				// atomic replacement. Its handle cannot survive restart, so
				// retain the linked replacement that owns the guest path.
				delete(p.nodes, existing.id)
				delete(p.persistentNodes, existing.id)
			case saved.NLink == 0 && existing.nlink != 0:
				continue
			default:
				return fmt.Errorf("nodes %d and %d both use guest path %q", existing.id, saved.ID, guestPath)
			}
		}
		node := &imageNode{id: saved.ID}
		if saved.ID == 1 {
			if root := p.nodes[1]; root != nil {
				copy := *root
				copy.entries = cloneStringUint64Map(root.entries)
				copy.whiteouts = cloneStringBoolMap(root.whiteouts)
				copy.xattrs = cloneXattrMap(root.xattrs)
				node = &copy
			}
		}
		if err := restorePersistentNode(node, saved); err != nil {
			return fmt.Errorf("node %d: %w", saved.ID, err)
		}
		if p.persistentLower != nil && len(saved.LowerPath) != 0 && !saved.Independent {
			if entry, err := imagefs.LookupPath(p.persistentLower, string(saved.LowerPath)); err == nil {
				switch {
				case entry.Dir != nil:
					node.abstractDir = entry.Dir
				case entry.File != nil:
					if saved.CopiedUp {
						node.lowerFile = entry.File
						node.abstractFile = nil
					} else {
						node.abstractFile = entry.File
					}
				case entry.Symlink != nil:
					node.abstractLink = entry.Symlink
				}
			} else {
				node.persistentLowerMissing = true
				p.persistent.lastErr = errors.Join(p.persistent.lastErr,
					fmt.Errorf("lower path %q for persistent inode %d is unavailable: %w", saved.LowerPath, saved.ID, err))
			}
		}
		node.persistentLowerPath = string(saved.LowerPath)
		p.nodes[saved.ID] = node
		p.persistentNodes[saved.ID] = struct{}{}
		savedByPath[guestPath] = node
		p.nextNodeID = max(p.nextNodeID, saved.ID+1)
	}

	paths := make([]string, 0, len(savedByPath))
	for guestPath := range savedByPath {
		paths = append(paths, guestPath)
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i], "/")
		rightDepth := strings.Count(paths[j], "/")
		if leftDepth == rightDepth {
			return paths[i] < paths[j]
		}
		return leftDepth < rightDepth
	})
	for _, guestPath := range paths {
		node := savedByPath[guestPath]
		if guestPath == "/" {
			node.id, node.parent, node.name = 1, 1, "/"
			p.nodes[1] = node
			continue
		}
		parentPath := path.Dir(guestPath)
		parent, err := p.ensurePersistentRestoreDirectory(parentPath, savedByPath)
		if err != nil {
			return fmt.Errorf("restore parent of %q: %w", guestPath, err)
		}
		name := path.Base(guestPath)
		node.parent, node.name = parent.id, name
		parent.entries[name] = node.id
	}

	for id := range p.persistentNodes {
		node := p.nodes[id]
		if node == nil {
			continue
		}
		if id != 1 && p.imageNodeLocked(node.parent) == nil {
			return fmt.Errorf("node %d parent %d is missing", id, node.parent)
		}
		for name, childID := range node.entries {
			clean, ok := cleanChildName(name)
			if !ok || clean != name {
				return fmt.Errorf("node %d has invalid directory entry %q", id, name)
			}
			if p.imageNodeLocked(childID) == nil {
				// A short-lived writer version could omit an open/replaced
				// inode while leaving its directory entry in the checkpoint.
				// The inode data is already absent, so retain the rest of the
				// filesystem and hide the unrecoverable name.
				delete(node.entries, name)
				node.whiteouts[name] = true
				recoveryErr := fmt.Errorf("discarded dangling directory entry %q from inode %d (missing inode %d)", name, id, childID)
				p.persistent.lastErr = errors.Join(p.persistent.lastErr, recoveryErr)
				if p.persistent.recovery.Status == "" {
					p.persistent.recovery.Status = "recovered-dangling-directory-entry"
				}
			}
		}
	}
	p.recountPersistentMetadataLocked()
	if p.xattrBytes > imageMaxXattrBytes {
		return fmt.Errorf("xattrs exceed filesystem metadata limit")
	}
	for _, node := range p.nodes {
		p.persistent.refreshUsage(node)
	}
	return nil
}

func (p *imageFS) ensurePersistentRestoreDirectory(directoryPath string, savedByPath map[string]*imageNode) (*imageNode, error) {
	if directoryPath == "/" {
		root := p.nodes[1]
		if root == nil {
			return nil, fmt.Errorf("root directory is missing")
		}
		return root, nil
	}
	if saved := savedByPath[directoryPath]; saved != nil {
		if !saved.isDir() {
			return nil, fmt.Errorf("%q is not a directory", directoryPath)
		}
		return saved, nil
	}
	parent, err := p.ensurePersistentRestoreDirectory(path.Dir(directoryPath), savedByPath)
	if err != nil {
		return nil, err
	}
	if existingID, ok := parent.entries[path.Base(directoryPath)]; ok {
		if existing := p.imageNodeLocked(existingID); existing != nil && existing.isDir() {
			return existing, nil
		}
	}
	if parent.abstractDir == nil {
		return nil, fmt.Errorf("lower parent %q is unavailable", path.Dir(directoryPath))
	}
	entry, err := parent.abstractDir.Lookup(path.Base(directoryPath))
	if err != nil {
		return nil, err
	}
	if entry.Dir == nil {
		return nil, fmt.Errorf("lower path %q is not a directory", directoryPath)
	}
	node := imageNodeFromEntry(p.nextNodeID, parent.id, path.Base(directoryPath), entry)
	p.nextNodeID++
	p.nodes[node.id] = node
	parent.entries[node.name] = node.id
	return node, nil
}

func restorePersistentNode(node *imageNode, saved persistentImageNode) error {
	node.id = saved.ID
	node.parent = saved.Parent
	node.name = string(saved.Name)
	node.mode = fs.FileMode(saved.Mode)
	node.rawMode = saved.RawMode
	node.uid = saved.UID
	node.gid = saved.GID
	node.rdev = saved.RDev
	node.size = saved.Size
	node.nlink = saved.NLink
	node.symlinkTarget = string(saved.SymlinkTarget)
	node.entriesDone = saved.EntriesDone
	node.lowerSize = saved.LowerSize
	node.persistentIndependent = saved.Independent
	node.atime = unixNanoTime(saved.ATimeUnixNano)
	node.modTime = unixNanoTime(saved.MTimeUnixNano)
	node.ctime = unixNanoTime(saved.CTimeUnixNano)
	node.entries = make(map[string]uint64, len(saved.Entries))
	for _, entry := range saved.Entries {
		name := string(entry.Name)
		if _, exists := node.entries[name]; exists {
			return fmt.Errorf("duplicate directory entry %q", entry.Name)
		}
		node.entries[name] = entry.Inode
	}
	node.whiteouts = make(map[string]bool, len(saved.Whiteouts))
	for _, encodedName := range saved.Whiteouts {
		name := string(encodedName)
		if _, exists := node.whiteouts[name]; exists {
			return fmt.Errorf("duplicate whiteout %q", name)
		}
		clean, ok := cleanChildName(name)
		if !ok || clean != name {
			return fmt.Errorf("invalid whiteout %q", name)
		}
		node.whiteouts[name] = true
	}
	node.xattrs = make(map[string][]byte, len(saved.Xattrs))
	for _, xattr := range saved.Xattrs {
		name := string(xattr.Name)
		if _, exists := node.xattrs[name]; exists {
			return fmt.Errorf("duplicate xattr %q", xattr.Name)
		}
		node.xattrs[name] = append([]byte(nil), xattr.Value...)
	}
	if imageNodeXattrBytes(node) > imageMaxXattrBytesPerNode {
		return fmt.Errorf("xattrs exceed per-inode metadata limit")
	}
	node.data.extents = make([]imageDataExtent, 0, len(saved.Data))
	var previousEnd uint64
	filePages := saved.Size / imageDataPageSize
	if saved.Size%imageDataPageSize != 0 {
		filePages++
	}
	for i, extent := range saved.Data {
		if extent.Count == 0 || extent.Page > ^uint64(0)-extent.Count || i != 0 && extent.Page < previousEnd {
			return fmt.Errorf("invalid data extent at page %d", extent.Page)
		}
		if extent.Page >= filePages {
			return fmt.Errorf("data extent at page %d exceeds file size %d", extent.Page, saved.Size)
		}
		node.data.extents = append(node.data.extents, imageDataExtent{
			page: extent.Page, location: extent.Page + 1, count: extent.Count,
		})
		previousEnd = extent.Page + extent.Count
	}
	switch saved.Kind {
	case "dir":
		if len(saved.Data) != 0 {
			return fmt.Errorf("directory contains file data")
		}
		node.mode |= fs.ModeDir
	case "symlink":
		if len(saved.Data) != 0 || len(saved.Entries) != 0 {
			return fmt.Errorf("symlink contains file data or directory entries")
		}
		node.mode |= fs.ModeSymlink
	case "file":
		if len(saved.Entries) != 0 || len(saved.Whiteouts) != 0 {
			return fmt.Errorf("regular file contains directory metadata")
		}
		if saved.LowerSize > saved.Size {
			return fmt.Errorf("lower size %d exceeds file size %d", saved.LowerSize, saved.Size)
		}
	default:
		return fmt.Errorf("invalid kind %q", saved.Kind)
	}
	return nil
}

func (p *imageFS) recountPersistentMetadataLocked() {
	p.xattrBytes = 0
	p.retainedNodes = 0
	p.retainedEntries = 0
	p.retainedWhiteouts = 0
	p.dynamicMetadata = 0
	for _, node := range p.nodes {
		if node == nil {
			continue
		}
		node.retainedEntries = len(node.entries)
		node.retainedWhiteouts = len(node.whiteouts)
		node.accountedMetadata = 0
		p.xattrBytes += uint64(imageNodeXattrBytes(node))
		p.retainedEntries += node.retainedEntries
		p.retainedWhiteouts += node.retainedWhiteouts
		p.noteImageNodeAddedLocked(node)
	}
}

func (p *imageFS) savePersistentLocked(durable bool) error {
	if p == nil || p.persistent == nil {
		return nil
	}
	return p.persistent.save(p.persistentStateLocked(), durable)
}

func (p *imageFS) readImageDataLocked(node *imageNode, dst []byte, off uint64) error {
	if p.persistent != nil {
		return p.persistent.readData(node.id, node.data, dst, off)
	}
	return node.data.readAt(p.dataStore, dst, off)
}

func (p *imageFS) writeImageDataLocked(node *imageNode, src []byte, off uint64) (int, error) {
	if p.persistent != nil {
		return p.persistent.writeData(node.id, &node.data, src, off)
	}
	return node.data.writeAt(p.dataStore, src, off)
}

func (p *imageFS) truncateImageDataLocked(node *imageNode, size uint64) error {
	if p.persistent != nil {
		return p.persistent.truncateData(node.id, &node.data, size)
	}
	return node.data.truncate(p.dataStore, size)
}

func (p *imageFS) releaseImageDataLocked(node *imageNode) error {
	if p.persistent != nil {
		return p.persistent.truncateData(node.id, &node.data, 0)
	}
	node.data.release(p.dataStore)
	return nil
}

func (p *imageFS) retireImageDataLocked(node *imageNode) error {
	if p.persistent != nil {
		p.persistent.retireData(node.id)
		return nil
	}
	node.data.release(p.dataStore)
	return nil
}

func (p *imageFS) imageDataAllocatedBytesLocked(node *imageNode, size uint64) uint64 {
	return node.data.allocatedBytes(size)
}

func (p *imageFS) refreshPersistentUsageLocked(node *imageNode) {
	if p != nil && p.persistent != nil {
		p.persistent.refreshLogicalUsage(node)
	}
}

func (p *imageFS) PersistentFSStatus() []PersistentFSStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	store := p.persistent
	if store == nil {
		return nil
	}
	var logical uint64
	for _, node := range p.nodes {
		if node != nil && !node.isDir() && !node.isSymlink() && len(node.data.extents) != 0 {
			logical += node.size
		}
	}
	status := PersistentFSStatus{
		Name:               store.name,
		Mount:              store.mount,
		FormatVersion:      persistentImageFormatVersion,
		LowerID:            store.lowerID,
		Sequence:           store.sequence,
		DurableSequence:    store.durableSequence,
		UpperLogicalBytes:  logical,
		UpperDataBytes:     store.currentUsage,
		UpperPhysicalBytes: store.physicalUsage,
		RecoveryStatus:     store.recovery.Status,
		QuarantinePath:     store.recovery.QuarantinePath,
		DiscardedBytes:     store.recovery.DiscardedBytes,
		LastCheckpoint:     store.lastCheckpoint,
	}
	if store.previousLowerID != store.lowerID {
		status.PreviousLowerID = store.previousLowerID
	}
	status.WALBytes, _ = persistentPathBytes(store.walPath())
	status.StagingBytes, _ = persistentPathBytes(filepath.Join(store.dir, "staging"))
	status.TrashBytes, _ = persistentPathBytes(filepath.Join(store.dir, "trash"))
	if store.lastErr != nil {
		status.LastError = store.lastErr.Error()
	} else if store.usageErr != nil {
		status.LastError = store.usageErr.Error()
	}
	_, _, available, _, _, blockSize, _, _, errno := hostStatFS(store.dir)
	if errno == 0 {
		status.HostFreeBytes = available * blockSize
	}
	return []PersistentFSStatus{status}
}

func persistentPathBytes(root string) (uint64, error) {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if !info.IsDir() {
		return uint64(max(int64(0), info.Size())), nil
	}
	var total uint64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += uint64(max(int64(0), info.Size()))
		}
		return nil
	})
	return total, err
}
