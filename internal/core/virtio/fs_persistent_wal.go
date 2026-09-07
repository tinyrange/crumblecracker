package virtio

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var persistentImageWALMagic = [8]byte{'C', 'C', 'H', 'W', 'A', 'L', '0', '1'}
var persistentImageCheckpointWALBytes int64 = 32 << 20

type persistentImagePending struct {
	nodes     map[uint64]struct{}
	deleted   map[uint64]struct{}
	dirents   map[persistentDirentKey]persistentImageDirentOp
	whiteouts map[persistentDirentKey]persistentImageWhiteoutOp
}

type persistentDirentKey struct {
	parent uint64
	name   string
}

type persistentImageDelta struct {
	Version    uint32                      `json:"version"`
	Sequence   uint64                      `json:"sequence"`
	NextNodeID uint64                      `json:"next_node_id"`
	Committed  bool                        `json:"committed,omitempty"`
	Nodes      []persistentImageNode       `json:"nodes,omitempty"`
	Deleted    []uint64                    `json:"deleted,omitempty"`
	Dirents    []persistentImageDirentOp   `json:"dirents,omitempty"`
	Whiteouts  []persistentImageWhiteoutOp `json:"whiteouts,omitempty"`
}

type persistentImageDirentOp struct {
	Parent  uint64 `json:"parent"`
	Name    []byte `json:"name"`
	Inode   uint64 `json:"inode,omitempty"`
	Present bool   `json:"present"`
}

type persistentImageWhiteoutOp struct {
	Parent  uint64 `json:"parent"`
	Name    []byte `json:"name"`
	Present bool   `json:"present"`
}

func (s *persistentImageStore) ensurePending() *persistentImagePending {
	if s.pending == nil {
		s.pending = &persistentImagePending{
			nodes:     make(map[uint64]struct{}),
			deleted:   make(map[uint64]struct{}),
			dirents:   make(map[persistentDirentKey]persistentImageDirentOp),
			whiteouts: make(map[persistentDirentKey]persistentImageWhiteoutOp),
		}
	}
	return s.pending
}

func (s *persistentImageStore) markNode(id uint64) {
	if s == nil || id == 0 {
		return
	}
	pending := s.ensurePending()
	if _, deleted := pending.deleted[id]; deleted {
		return
	}
	pending.nodes[id] = struct{}{}
}

func (s *persistentImageStore) markDeleted(id uint64) {
	if s == nil || id == 0 {
		return
	}
	pending := s.ensurePending()
	delete(pending.nodes, id)
	pending.deleted[id] = struct{}{}
}

func (s *persistentImageStore) markDirent(parent uint64, name string, inode uint64, present bool) {
	if s == nil {
		return
	}
	key := persistentDirentKey{parent: parent, name: name}
	s.ensurePending().dirents[key] = persistentImageDirentOp{
		Parent: parent, Name: []byte(name), Inode: inode, Present: present,
	}
}

func (s *persistentImageStore) markWhiteout(parent uint64, name string, present bool) {
	if s == nil {
		return
	}
	key := persistentDirentKey{parent: parent, name: name}
	s.ensurePending().whiteouts[key] = persistentImageWhiteoutOp{
		Parent: parent, Name: []byte(name), Present: present,
	}
}

func (p *imageFS) markPersistentNodeLocked(node *imageNode) {
	if p != nil && p.persistent != nil && node != nil {
		p.persistentNodes[node.id] = struct{}{}
		p.persistent.markNode(node.id)
	}
}

func (p *imageFS) setPersistentDirentLocked(parent *imageNode, name string, inode uint64) {
	parent.entries[name] = inode
	if p.persistent != nil {
		p.persistent.markDirent(parent.id, name, inode, true)
	}
}

func (p *imageFS) deletePersistentDirentLocked(parent *imageNode, name string) {
	inode := parent.entries[name]
	delete(parent.entries, name)
	if p.persistent != nil {
		p.persistent.markDirent(parent.id, name, inode, false)
	}
}

func (p *imageFS) setPersistentWhiteoutLocked(parent *imageNode, name string) {
	if parent.whiteouts == nil {
		parent.whiteouts = map[string]bool{}
	}
	parent.whiteouts[name] = true
	if p.persistent != nil {
		p.persistent.markWhiteout(parent.id, name, true)
	}
}

func (p *imageFS) deletePersistentWhiteoutLocked(parent *imageNode, name string) {
	delete(parent.whiteouts, name)
	if p.persistent != nil {
		p.persistent.markWhiteout(parent.id, name, false)
	}
}

func (p *imageFS) persistentDeltaLocked() *persistentImageDelta {
	if p == nil || p.persistent == nil || p.persistent.pending == nil {
		return nil
	}
	pending := p.persistent.pending
	if len(pending.nodes) == 0 && len(pending.deleted) == 0 && len(pending.dirents) == 0 && len(pending.whiteouts) == 0 {
		return nil
	}
	delta := &persistentImageDelta{
		Version: persistentImageFormatVersion, NextNodeID: p.nextNodeID,
	}
	nodeIDs := make([]uint64, 0, len(pending.nodes))
	for id := range pending.nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Slice(nodeIDs, func(i, j int) bool { return nodeIDs[i] < nodeIDs[j] })
	for _, id := range nodeIDs {
		node := p.nodes[id]
		if node == nil {
			continue
		}
		saved := persistentNodeFromImageNode(node, p.pathForNode(id), p.persistentNodes)
		saved.Entries = nil
		saved.Whiteouts = nil
		delta.Nodes = append(delta.Nodes, saved)
	}
	for id := range pending.deleted {
		delta.Deleted = append(delta.Deleted, id)
	}
	sort.Slice(delta.Deleted, func(i, j int) bool { return delta.Deleted[i] < delta.Deleted[j] })
	for _, op := range pending.dirents {
		delta.Dirents = append(delta.Dirents, op)
	}
	sort.Slice(delta.Dirents, func(i, j int) bool {
		if delta.Dirents[i].Parent == delta.Dirents[j].Parent {
			return string(delta.Dirents[i].Name) < string(delta.Dirents[j].Name)
		}
		return delta.Dirents[i].Parent < delta.Dirents[j].Parent
	})
	for _, op := range pending.whiteouts {
		delta.Whiteouts = append(delta.Whiteouts, op)
	}
	sort.Slice(delta.Whiteouts, func(i, j int) bool {
		if delta.Whiteouts[i].Parent == delta.Whiteouts[j].Parent {
			return string(delta.Whiteouts[i].Name) < string(delta.Whiteouts[j].Name)
		}
		return delta.Whiteouts[i].Parent < delta.Whiteouts[j].Parent
	})
	return delta
}

func (p *imageFS) persistentFileDeltaLocked(nodeID uint64) *persistentImageDelta {
	if p == nil || p.persistent == nil {
		return nil
	}
	node := p.imageNodeLocked(nodeID)
	if node == nil || node.isDir() {
		return nil
	}
	saved := persistentNodeFromImageNode(node, p.pathForNode(nodeID), p.persistentNodes)
	// A file fsync covers inode data and metadata, not directory entries or
	// link/rename operations. Existing durable namespace fields are retained
	// when this delta is applied.
	saved.Entries = nil
	saved.Whiteouts = nil
	delta := &persistentImageDelta{
		Version: persistentImageFormatVersion, NextNodeID: p.nextNodeID,
		Nodes: []persistentImageNode{saved},
	}
	_, existedDurably := p.persistent.durableNode(nodeID)
	if existedDurably {
		// fsync(2) makes the inode durable. Renames and new hard links for an
		// already-durable inode still require fsync on the containing directory.
		delta.Nodes[0].PreserveNamespace = true
		return delta
	}
	pending := p.persistent.pending
	if pending == nil {
		delta.Nodes[0].PreserveNamespace = true
		return delta
	}
	parents := make(map[uint64]struct{})
	selectedKeys := make(map[persistentDirentKey]struct{})
	for key, op := range pending.dirents {
		if op.Inode != nodeID {
			continue
		}
		delta.Dirents = append(delta.Dirents, op)
		parents[op.Parent] = struct{}{}
		selectedKeys[key] = struct{}{}
	}
	if len(delta.Dirents) == 0 {
		delta.Nodes[0].PreserveNamespace = true
		return delta
	}
	for key, op := range pending.whiteouts {
		if _, selected := selectedKeys[key]; selected {
			delta.Whiteouts = append(delta.Whiteouts, op)
		}
	}
	for parentID := range parents {
		parent := p.imageNodeLocked(parentID)
		if parent == nil {
			continue
		}
		parentSaved := persistentNodeFromImageNode(parent, p.pathForNode(parentID), p.persistentNodes)
		parentSaved.Entries = nil
		parentSaved.Whiteouts = nil
		delta.Nodes = append(delta.Nodes, parentSaved)
	}
	if _, deleted := pending.deleted[nodeID]; deleted {
		delta.Deleted = append(delta.Deleted, nodeID)
	}
	sort.Slice(delta.Nodes, func(i, j int) bool { return delta.Nodes[i].ID < delta.Nodes[j].ID })
	sort.Slice(delta.Dirents, func(i, j int) bool {
		if delta.Dirents[i].Parent == delta.Dirents[j].Parent {
			return string(delta.Dirents[i].Name) < string(delta.Dirents[j].Name)
		}
		return delta.Dirents[i].Parent < delta.Dirents[j].Parent
	})
	return delta
}

func (p *imageFS) persistentNamespaceDeltaLocked() *persistentImageDelta {
	delta := p.persistentDeltaLocked()
	if delta == nil {
		return nil
	}
	for i := range delta.Nodes {
		node := &delta.Nodes[i]
		if _, dirty := p.persistent.dirtyData[node.ID]; !dirty || node.Kind != "file" {
			continue
		}
		if durable, ok := p.persistent.durableNode(node.ID); ok {
			preservePersistentNodeData(node, durable)
			continue
		}
		// Directory durability must be able to publish a newly-created name
		// without claiming that un-fsynced file contents survived a crash.
		// A copied-up lower file falls back to its immutable lower contents; a
		// brand-new file becomes an empty durable inode until its own fsync.
		if len(node.LowerPath) != 0 {
			node.Size = node.LowerSize
			node.CopiedUp = false
			node.Independent = false
		} else {
			node.Size = 0
			node.LowerSize = 0
			node.Independent = true
		}
		node.Data = nil
	}
	return delta
}

func preservePersistentNodeData(target *persistentImageNode, durable persistentImageNode) {
	target.Size = durable.Size
	target.Data = append([]persistentImageExtent(nil), durable.Data...)
	target.CopiedUp = durable.CopiedUp
	target.Independent = durable.Independent
	target.LowerSize = durable.LowerSize
	target.LowerPath = append([]byte(nil), durable.LowerPath...)
}

func (s *persistentImageStore) durableNode(nodeID uint64) (persistentImageNode, bool) {
	if s == nil || s.durableState == nil {
		return persistentImageNode{}, false
	}
	for _, node := range s.durableState.Nodes {
		if node.ID == nodeID {
			return node, true
		}
	}
	return persistentImageNode{}, false
}

func (s *persistentImageStore) clearFilePending(nodeID uint64, committedCreation bool) {
	if s == nil || s.pending == nil {
		return
	}
	if committedCreation {
		for key, op := range s.pending.dirents {
			if op.Inode == nodeID {
				delete(s.pending.dirents, key)
				delete(s.pending.whiteouts, key)
			}
		}
		delete(s.pending.deleted, nodeID)
	}
	keepNodeForNamespace := false
	if !committedCreation {
		_, keepNodeForNamespace = s.pending.deleted[nodeID]
		if !keepNodeForNamespace {
			for _, op := range s.pending.dirents {
				if op.Inode == nodeID {
					keepNodeForNamespace = true
					break
				}
			}
		}
	}
	if !keepNodeForNamespace {
		delete(s.pending.nodes, nodeID)
	}
	if len(s.pending.nodes) == 0 && len(s.pending.deleted) == 0 && len(s.pending.dirents) == 0 && len(s.pending.whiteouts) == 0 {
		s.pending = nil
	}
}

func (s *persistentImageStore) clearNamespacePending() {
	if s == nil || s.pending == nil {
		return
	}
	dirty := make(map[uint64]struct{}, len(s.dirtyData))
	for id := range s.dirtyData {
		if _, deleted := s.pending.deleted[id]; !deleted {
			dirty[id] = struct{}{}
		}
	}
	s.pending = nil
	if len(dirty) != 0 {
		s.pending = &persistentImagePending{
			nodes: dirty, deleted: make(map[uint64]struct{}),
			dirents:   make(map[persistentDirentKey]persistentImageDirentOp),
			whiteouts: make(map[persistentDirentKey]persistentImageWhiteoutOp),
		}
	}
}

func (s *persistentImageStore) clearPending() {
	s.pending = nil
}

func (s *persistentImageStore) walPath() string {
	return filepath.Join(s.dir, "metadata.wal")
}

func (s *persistentImageStore) appendDelta(delta *persistentImageDelta, durable bool) error {
	if delta == nil {
		if !durable {
			return nil
		}
		// A durability barrier is a real WAL record. Earlier non-durable bytes
		// may happen to reach disk during a crash, but recovery only accepts
		// them when a data-ordered committed record follows.
		delta = &persistentImageDelta{Version: persistentImageFormatVersion}
	}
	delta.Version = persistentImageFormatVersion
	delta.Sequence = s.sequence + 1
	delta.Committed = durable
	var nextDurable *persistentImageState
	if durable {
		nextDurable = clonePersistentImageState(s.durableState)
		if nextDurable == nil {
			nextDurable = &persistentImageState{Version: persistentImageFormatVersion, LowerID: s.lowerID}
		}
		if err := applyPersistentImageDelta(nextDurable, delta); err != nil {
			return fmt.Errorf("apply persistent metadata delta: %w", err)
		}
	}
	if err := runPersistentImageFaultTestHook("before-wal-append"); err != nil {
		return err
	}
	payload, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("encode persistent metadata delta: %w", err)
	}
	if len(payload) > persistentImageMaxMetadata {
		return fmt.Errorf("persistent metadata delta is %d bytes", len(payload))
	}
	var header [persistentImageHeaderSize]byte
	copy(header[:8], persistentImageWALMagic[:])
	binary.LittleEndian.PutUint32(header[8:12], persistentImageFormatVersion)
	binary.LittleEndian.PutUint64(header[16:24], delta.Sequence)
	binary.LittleEndian.PutUint64(header[24:32], uint64(len(payload)))
	binary.LittleEndian.PutUint32(header[32:36], crc32.ChecksumIEEE(payload))
	file, err := os.OpenFile(s.walPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open persistent metadata WAL: %w", err)
	}
	if _, err := file.Write(header[:]); err != nil {
		_ = file.Close()
		return fmt.Errorf("append persistent metadata WAL header: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("append persistent metadata WAL payload: %w", err)
	}
	runPersistentImageSaveTestHook("wal-written")
	if durable {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync persistent metadata WAL: %w", err)
		}
	}
	runPersistentImageSaveTestHook("wal-synced")
	if err := file.Close(); err != nil {
		return fmt.Errorf("close persistent metadata WAL: %w", err)
	}
	runPersistentImageSaveTestHook("wal-closed")
	s.sequence = delta.Sequence
	if durable {
		s.durableSequence = s.sequence
		s.durableState = nextDurable
	}
	return nil
}

func (s *persistentImageStore) replayWAL(state *persistentImageState) error {
	file, err := os.OpenFile(s.walPath(), os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open persistent metadata WAL: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat persistent metadata WAL: %w", err)
	}
	type walRecord struct {
		offset int64
		delta  persistentImageDelta
	}
	var records []walRecord
	discardOffset := info.Size()
	discardReason := ""
	for offset := int64(0); offset < info.Size(); {
		var header [persistentImageHeaderSize]byte
		if info.Size()-offset < int64(len(header)) {
			discardOffset, discardReason = offset, "truncated-header"
			break
		}
		if _, err := file.ReadAt(header[:], offset); err != nil {
			return fmt.Errorf("read persistent metadata WAL header: %w", err)
		}
		if string(header[:8]) != string(persistentImageWALMagic[:]) {
			discardOffset, discardReason = offset, "invalid-magic"
			break
		}
		length := binary.LittleEndian.Uint64(header[24:32])
		if length > persistentImageMaxMetadata {
			discardOffset, discardReason = offset, "invalid-length"
			break
		}
		recordEnd := offset + int64(len(header)) + int64(length)
		if recordEnd < offset || recordEnd > info.Size() {
			discardOffset, discardReason = offset, "truncated-payload"
			break
		}
		payload := make([]byte, int(length))
		if _, err := file.ReadAt(payload, offset+int64(len(header))); err != nil {
			return fmt.Errorf("read persistent metadata WAL payload: %w", err)
		}
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[32:36]) {
			discardOffset, discardReason = offset, "checksum"
			break
		}
		var delta persistentImageDelta
		if err := json.Unmarshal(payload, &delta); err != nil {
			discardOffset, discardReason = offset, "invalid-payload"
			break
		}
		if delta.Version != persistentImageFormatVersion || delta.Sequence != binary.LittleEndian.Uint64(header[16:24]) {
			discardOffset, discardReason = offset, "invalid-record"
			break
		}
		records = append(records, walRecord{offset: offset, delta: delta})
		offset = recordEnd
	}

	firstPostSnapshot := -1
	lastCommitted := -1
	for index := range records {
		if records[index].delta.Sequence <= state.Sequence {
			continue
		}
		if firstPostSnapshot == -1 {
			firstPostSnapshot = index
		}
		if records[index].delta.Committed {
			lastCommitted = index
		}
	}

	if firstPostSnapshot != -1 && lastCommitted < firstPostSnapshot {
		if records[firstPostSnapshot].offset < discardOffset {
			discardOffset = records[firstPostSnapshot].offset
			discardReason = "uncommitted"
		}
	} else if lastCommitted >= firstPostSnapshot && lastCommitted+1 < len(records) {
		if records[lastCommitted+1].offset < discardOffset {
			discardOffset = records[lastCommitted+1].offset
			discardReason = "uncommitted"
		}
	}

	if firstPostSnapshot != -1 && lastCommitted >= firstPostSnapshot {
		candidate := clonePersistentImageState(state)
		groupOffset := records[firstPostSnapshot].offset
		for index := firstPostSnapshot; index <= lastCommitted; index++ {
			delta := &records[index].delta
			if delta.Sequence != candidate.Sequence+1 {
				if groupOffset < discardOffset {
					discardOffset = groupOffset
					discardReason = "sequence"
				}
				break
			}
			if err := applyPersistentImageDelta(candidate, delta); err != nil {
				if groupOffset < discardOffset {
					discardOffset = groupOffset
					discardReason = "invalid-delta"
				}
				break
			}
			if delta.Committed {
				*state = *candidate
				s.sequence = delta.Sequence
				if index < lastCommitted {
					candidate = clonePersistentImageState(state)
					groupOffset = records[index+1].offset
				}
			}
		}
	}
	if discardOffset < info.Size() {
		return s.quarantineWALTail(file, discardOffset, info.Size(), discardReason)
	}
	return nil
}

func (s *persistentImageStore) quarantineWALTail(file *os.File, offset, size int64, reason string) error {
	if offset >= size {
		return nil
	}
	name := fmt.Sprintf("wal-torn-%020d-%s-%d", s.sequence+1, reason, time.Now().UnixNano())
	path := filepath.Join(s.dir, "staging", name)
	quarantine, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("preserve invalid persistent metadata WAL tail: %w", err)
	}
	_, copyErr := io.Copy(quarantine, io.NewSectionReader(file, offset, size-offset))
	syncErr := quarantine.Sync()
	closeErr := quarantine.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("preserve invalid persistent metadata WAL tail: %w", err)
	}
	if err := syncPersistentDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("publish invalid persistent metadata WAL tail: %w", err)
	}
	if err := file.Truncate(offset); err != nil {
		return fmt.Errorf("discard quarantined persistent metadata WAL tail: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync recovered persistent metadata WAL: %w", err)
	}
	status := "discarded-torn-wal-tail"
	if reason == "uncommitted" {
		status = "discarded-uncommitted-wal-tail"
	}
	s.recovery = persistentImageRecovery{
		Status:         status,
		QuarantinePath: path,
		DiscardedBytes: uint64(size - offset),
	}
	return nil
}

func applyPersistentImageDelta(state *persistentImageState, delta *persistentImageDelta) error {
	nodes := make(map[uint64]persistentImageNode, len(state.Nodes)+len(delta.Nodes))
	for _, node := range state.Nodes {
		nodes[node.ID] = node
	}
	for _, update := range delta.Nodes {
		if old, ok := nodes[update.ID]; ok {
			if update.PreserveNamespace {
				update.Parent = old.Parent
				update.Name = append([]byte(nil), old.Name...)
				update.GuestPath = append([]byte(nil), old.GuestPath...)
				update.NLink = old.NLink
			}
			update.Entries = old.Entries
			update.Whiteouts = old.Whiteouts
		}
		update.PreserveNamespace = false
		nodes[update.ID] = update
	}
	for _, id := range delta.Deleted {
		delete(nodes, id)
	}
	for _, op := range delta.Dirents {
		parent, ok := nodes[op.Parent]
		if !ok {
			return fmt.Errorf("directory entry parent %d is missing", op.Parent)
		}
		entries := make(map[string]uint64, len(parent.Entries)+1)
		for _, entry := range parent.Entries {
			entries[string(entry.Name)] = entry.Inode
		}
		name := string(op.Name)
		if op.Present {
			entries[name] = op.Inode
		} else {
			delete(entries, name)
		}
		parent.Entries = parent.Entries[:0]
		for name, inode := range entries {
			parent.Entries = append(parent.Entries, persistentImageDirent{Name: []byte(name), Inode: inode})
		}
		sort.Slice(parent.Entries, func(i, j int) bool { return string(parent.Entries[i].Name) < string(parent.Entries[j].Name) })
		nodes[parent.ID] = parent
	}
	for _, op := range delta.Whiteouts {
		parent, ok := nodes[op.Parent]
		if !ok {
			return fmt.Errorf("whiteout parent %d is missing", op.Parent)
		}
		whiteouts := make(map[string]bool, len(parent.Whiteouts)+1)
		for _, name := range parent.Whiteouts {
			whiteouts[string(name)] = true
		}
		name := string(op.Name)
		if op.Present {
			whiteouts[name] = true
		} else {
			delete(whiteouts, name)
		}
		parent.Whiteouts = parent.Whiteouts[:0]
		for name := range whiteouts {
			parent.Whiteouts = append(parent.Whiteouts, []byte(name))
		}
		sort.Slice(parent.Whiteouts, func(i, j int) bool { return string(parent.Whiteouts[i]) < string(parent.Whiteouts[j]) })
		nodes[parent.ID] = parent
	}
	state.Nodes = state.Nodes[:0]
	for _, node := range nodes {
		state.Nodes = append(state.Nodes, node)
	}
	sort.Slice(state.Nodes, func(i, j int) bool { return state.Nodes[i].ID < state.Nodes[j].ID })
	state.NextNodeID = max(state.NextNodeID, delta.NextNodeID)
	state.Sequence = delta.Sequence
	return nil
}

func clonePersistentImageState(source *persistentImageState) *persistentImageState {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.RemovedNodes = append([]uint64(nil), source.RemovedNodes...)
	cloned.Nodes = make([]persistentImageNode, len(source.Nodes))
	for index := range source.Nodes {
		node := source.Nodes[index]
		node.Name = append([]byte(nil), node.Name...)
		node.SymlinkTarget = append([]byte(nil), node.SymlinkTarget...)
		node.LowerPath = append([]byte(nil), node.LowerPath...)
		node.Entries = append([]persistentImageDirent(nil), node.Entries...)
		for entry := range node.Entries {
			node.Entries[entry].Name = append([]byte(nil), node.Entries[entry].Name...)
		}
		node.Whiteouts = append([][]byte(nil), node.Whiteouts...)
		for whiteout := range node.Whiteouts {
			node.Whiteouts[whiteout] = append([]byte(nil), node.Whiteouts[whiteout]...)
		}
		node.Xattrs = append([]persistentImageXattr(nil), node.Xattrs...)
		for xattr := range node.Xattrs {
			node.Xattrs[xattr].Name = append([]byte(nil), node.Xattrs[xattr].Name...)
			node.Xattrs[xattr].Value = append([]byte(nil), node.Xattrs[xattr].Value...)
		}
		node.Data = append([]persistentImageExtent(nil), node.Data...)
		cloned.Nodes[index] = node
	}
	return &cloned
}

func (s *persistentImageStore) resetWAL(durable bool) error {
	file, err := os.OpenFile(s.walPath(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if durable {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if durable {
		err = errors.Join(err, syncPersistentDirectory(s.dir))
	}
	return err
}

func (p *imageFS) flushPersistentLocked(durable bool) error {
	if p == nil || p.persistent == nil {
		return nil
	}
	if durable {
		// The committed record below makes all preceding WAL records
		// recoverable. Sync every data file changed by those records first.
		if err := p.persistent.syncDirtyData(); err != nil {
			p.persistent.lastErr = err
			return err
		}
	}
	delta := p.persistentDeltaLocked()
	if err := p.persistent.appendDelta(delta, durable); err != nil {
		p.persistent.lastErr = err
		return err
	}
	if delta != nil {
		for _, node := range delta.Nodes {
			p.persistent.refreshPhysicalUsage(node.ID)
		}
	}
	p.persistent.clearPending()
	if durable {
		if err := p.persistent.reclaimDurableData(); err != nil {
			p.persistent.lastErr = err
			return err
		}
	}
	return nil
}

func (p *imageFS) commitPersistentMutationLocked() int32 {
	// Namespace operations have already succeeded in memory. Keep their WAL
	// changes coalesced until Flush, Fsync, or Close; an append failure here
	// would otherwise report that the operation failed while leaving its live
	// mutation visible.
	return 0
}

func (p *imageFS) checkpointPersistentLocked(durable bool) error {
	if p == nil || p.persistent == nil {
		return nil
	}
	if err := p.flushPersistentLocked(durable); err != nil {
		return err
	}
	if err := p.savePersistentLocked(durable); err != nil {
		return err
	}
	if err := p.persistent.resetWAL(durable); err != nil {
		return err
	}
	return nil
}

func (p *imageFS) maybeCheckpointPersistentLocked() error {
	if p == nil || p.persistent == nil || persistentImageCheckpointWALBytes <= 0 {
		return nil
	}
	info, err := os.Stat(p.persistent.walPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() < persistentImageCheckpointWALBytes {
		return nil
	}
	state := clonePersistentImageState(p.persistent.durableState)
	if state == nil {
		return nil
	}
	// A selective fsync may leave unrelated live mutations dirty. Compact only
	// the already-durable state; checkpointing must never promote those live
	// mutations merely because the WAL crossed its size threshold.
	if err := p.persistent.save(state, true); err != nil {
		p.persistent.lastErr = err
		return err
	}
	if err := p.persistent.resetWAL(true); err != nil {
		p.persistent.lastErr = err
		return err
	}
	return nil
}
