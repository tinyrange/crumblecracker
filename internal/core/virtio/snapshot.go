package virtio

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

func (p *imageFS) RootSnapshot() (imagefs.Directory, error) {
	return p.RootSnapshotContext(context.Background())
}

func (p *imageFS) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	if p == nil {
		return nil, fmt.Errorf("image filesystem is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.materializeSnapshotTreeContext(ctx, 1); err != nil {
		return nil, err
	}
	for {
		p.mu.Lock()
		root := p.imageNodeLocked(1)
		if root == nil || !root.isDir() {
			p.mu.Unlock()
			return nil, fmt.Errorf("image filesystem root is missing")
		}
		pending := p.firstUnmaterializedSnapshotDirLocked(1)
		if pending == 0 {
			out, err := p.snapshotDirLocked(ctx, root, p.dataStore)
			p.mu.Unlock()
			return out, err
		}
		p.mu.Unlock()
		if err := p.materializeSnapshotTreeContext(ctx, pending); err != nil {
			return nil, err
		}
	}
}

func (p *imageFS) materializeSnapshotTreeContext(ctx context.Context, rootID uint64) error {
	queue := []uint64{rootID}
	seen := make(map[uint64]struct{})
	for len(queue) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		nodeID := queue[0]
		queue = queue[1:]
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		if err := p.materializeDirEntriesContext(ctx, nodeID); err != nil {
			return fmt.Errorf("materialize snapshot directory: %w", err)
		}
		p.mu.Lock()
		node := p.imageNodeLocked(nodeID)
		if node == nil {
			p.mu.Unlock()
			continue
		}
		for _, childID := range node.entries {
			if child := p.imageNodeLocked(childID); child != nil && child.isDir() {
				queue = append(queue, childID)
			}
		}
		p.mu.Unlock()
	}
	return nil
}

func (p *imageFS) firstUnmaterializedSnapshotDirLocked(rootID uint64) uint64 {
	queue := []uint64{rootID}
	seen := make(map[uint64]struct{})
	for len(queue) != 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		node := p.imageNodeLocked(nodeID)
		if node == nil || !node.isDir() {
			continue
		}
		if node.abstractDir != nil && !node.entriesDone {
			return nodeID
		}
		for _, childID := range node.entries {
			queue = append(queue, childID)
		}
	}
	return 0
}

func (p *imageFS) snapshotDirLocked(ctx context.Context, node *imageNode, store *imageDataStore) (*snapshotDir, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	attr := p.attr(node)
	out := &snapshotDir{
		mode:    linuxModeToGo(attr.Mode),
		uid:     attr.UID,
		gid:     attr.GID,
		rdev:    attr.RDev,
		modTime: unixAttrModTime(attr),
		entries: map[string]imagefs.Entry{},
	}
	names := make([]string, 0, len(node.entries))
	for name := range node.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := p.imageNodeLocked(node.entries[name])
		if child == nil {
			continue
		}
		entry, err := p.snapshotEntryLocked(ctx, child, store)
		if err != nil {
			releaseSnapshotDir(out)
			return nil, err
		}
		out.entries[name] = entry
	}
	return out, nil
}

func (p *imageFS) snapshotEntryLocked(ctx context.Context, node *imageNode, store *imageDataStore) (imagefs.Entry, error) {
	if err := ctx.Err(); err != nil {
		return imagefs.Entry{}, err
	}
	attr := p.attr(node)
	switch {
	case node.isDir():
		dir, err := p.snapshotDirLocked(ctx, node, store)
		if err != nil {
			return imagefs.Entry{}, err
		}
		return imagefs.Entry{Dir: dir}, nil
	case node.isSymlink():
		target := node.symlinkTarget
		if node.abstractLink != nil {
			target = node.abstractLink.Target()
		}
		return imagefs.Entry{Symlink: &snapshotSymlink{
			mode:    linuxModeToGo(attr.Mode),
			uid:     attr.UID,
			gid:     attr.GID,
			rdev:    attr.RDev,
			target:  target,
			modTime: unixAttrModTime(attr),
		}}, nil
	default:
		var source imagefs.File
		var data snapshotSparseData
		if node.abstractFile != nil {
			source = node.abstractFile
		} else {
			source = node.lowerFile
		}
		if len(node.data.extents) != 0 {
			data = snapshotSparseData{store: store}
			for _, extent := range node.data.extents {
				for pageOffset := uint64(0); pageOffset < extent.count; pageOffset++ {
					if err := ctx.Err(); err != nil {
						data.data.release(store)
						return imagefs.Entry{}, err
					}
					snapshotLocation := extent.location + pageOffset
					if err := store.retainPage(snapshotLocation); err != nil {
						data.data.release(store)
						return imagefs.Entry{}, fmt.Errorf("snapshot file %s: %w", p.pathForNode(node.id), err)
					}
					data.data.insert(extent.page+pageOffset, snapshotLocation)
				}
			}
		}
		file := &snapshotFile{
			mode:   linuxModeToGo(attr.Mode),
			uid:    attr.UID,
			gid:    attr.GID,
			rdev:   attr.RDev,
			size:   attr.Size,
			data:   data,
			source: source,
			sourceSize: func() uint64 {
				if node.abstractFile != nil {
					return attr.Size
				}
				return node.lowerSize
			}(),
			modTime: unixAttrModTime(attr),
		}
		if data.store != nil {
			data.store.retain()
			runtime.SetFinalizer(file, (*snapshotFile).releaseStore)
		}
		return imagefs.Entry{File: file}, nil
	}
}

func unixAttrModTime(attr FuseAttr) time.Time {
	if attr.MTimeSec == 0 && attr.MTimeNsec == 0 {
		return time.Unix(0, 0)
	}
	return time.Unix(int64(attr.MTimeSec), int64(attr.MTimeNsec))
}

type snapshotDir struct {
	mode      fs.FileMode
	uid       uint32
	gid       uint32
	rdev      uint32
	modTime   time.Time
	entries   map[string]imagefs.Entry
	closeOnce sync.Once
}

func (d *snapshotDir) Close() error {
	if d != nil {
		d.closeOnce.Do(func() { releaseSnapshotDirEntries(d) })
	}
	return nil
}

func (d *snapshotDir) Stat() fs.FileMode       { return d.mode }
func (d *snapshotDir) ModTime() time.Time      { return d.modTime }
func (d *snapshotDir) Owner() (uint32, uint32) { return d.uid, d.gid }
func (d *snapshotDir) RDev() uint32            { return d.rdev }
func (d *snapshotDir) ReadDir() ([]imagefs.DirEnt, error) {
	names := make([]string, 0, len(d.entries))
	for name := range d.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]imagefs.DirEnt, 0, len(names))
	for _, name := range names {
		entry := d.entries[name]
		var mode fs.FileMode
		switch {
		case entry.Dir != nil:
			mode = fs.ModeDir | entry.Dir.Stat()
		case entry.Symlink != nil:
			mode = fs.ModeSymlink | entry.Symlink.Stat()
		case entry.File != nil:
			_, mode = entry.File.Stat()
		}
		out = append(out, imagefs.DirEnt{Name: name, Mode: mode})
	}
	return out, nil
}
func (d *snapshotDir) Lookup(name string) (imagefs.Entry, error) {
	entry, ok := d.entries[name]
	if !ok {
		return imagefs.Entry{}, os.ErrNotExist
	}
	return entry, nil
}

type snapshotFile struct {
	mode       fs.FileMode
	uid        uint32
	gid        uint32
	rdev       uint32
	size       uint64
	data       snapshotSparseData
	source     imagefs.File
	sourceSize uint64
	modTime    time.Time
}

type snapshotSparseData struct {
	data  sparseImageData
	store *imageDataStore
}

func (d snapshotSparseData) readAt(dst []byte, off uint64) error {
	if d.store == nil {
		return nil
	}
	return d.data.readAt(d.store, dst, off)
}

func (f *snapshotFile) releaseStore() {
	if f == nil || f.data.store == nil {
		return
	}
	f.data.data.release(f.data.store)
	_ = f.data.store.close()
	f.data.store = nil
}

func (f *snapshotFile) Stat() (uint64, fs.FileMode) { return f.size, f.mode }
func (f *snapshotFile) ModTime() time.Time          { return f.modTime }
func (f *snapshotFile) Owner() (uint32, uint32)     { return f.uid, f.gid }
func (f *snapshotFile) RDev() uint32                { return f.rdev }
func (f *snapshotFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	if off >= f.size || size == 0 {
		return nil, nil
	}
	end := off + uint64(size)
	if end > f.size {
		end = f.size
	}
	data := make([]byte, end-off)
	if f.source != nil && off < f.sourceSize {
		sourceEnd := min(end, f.sourceSize)
		lower, err := f.source.ReadAt(off, uint32(sourceEnd-off))
		if err != nil {
			return nil, err
		}
		if uint64(len(lower)) != sourceEnd-off {
			return nil, io.ErrUnexpectedEOF
		}
		copy(data, lower)
	}
	if err := f.data.readAt(data, off); err != nil {
		return nil, err
	}
	return data, nil
}

func releaseSnapshotDir(dir *snapshotDir) {
	if dir == nil {
		return
	}
	_ = dir.Close()
}

func releaseSnapshotDirEntries(dir *snapshotDir) {
	for _, entry := range dir.entries {
		switch {
		case entry.Dir != nil:
			if child, ok := entry.Dir.(*snapshotDir); ok {
				_ = child.Close()
			}
		case entry.File != nil:
			if file, ok := entry.File.(*snapshotFile); ok {
				runtime.SetFinalizer(file, nil)
				file.releaseStore()
			}
		}
	}
}

type snapshotSymlink struct {
	mode    fs.FileMode
	uid     uint32
	gid     uint32
	rdev    uint32
	target  string
	modTime time.Time
}

func (l *snapshotSymlink) Stat() fs.FileMode       { return l.mode }
func (l *snapshotSymlink) ModTime() time.Time      { return l.modTime }
func (l *snapshotSymlink) Target() string          { return l.target }
func (l *snapshotSymlink) Owner() (uint32, uint32) { return l.uid, l.gid }
func (l *snapshotSymlink) RDev() uint32            { return l.rdev }
