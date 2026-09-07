package virtio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

type blockingSnapshotDirectory struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type retryLookupDirectory struct {
	mu       sync.Mutex
	failures int
}

type legacyBlockingSnapshotDirectory struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type namespaceTestDirectory struct {
	imagefs.Directory
	namespace *imagefs.Namespace
}

func (d *namespaceTestDirectory) Namespace() *imagefs.Namespace {
	return d.namespace
}

func (d *legacyBlockingSnapshotDirectory) Stat() fs.FileMode       { return fs.ModeDir | 0o755 }
func (d *legacyBlockingSnapshotDirectory) ModTime() time.Time      { return time.Unix(0, 0) }
func (d *legacyBlockingSnapshotDirectory) Owner() (uint32, uint32) { return 0, 0 }
func (d *legacyBlockingSnapshotDirectory) RDev() uint32            { return 0 }
func (d *legacyBlockingSnapshotDirectory) ReadDir() ([]imagefs.DirEnt, error) {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return nil, nil
}
func (d *legacyBlockingSnapshotDirectory) Lookup(string) (imagefs.Entry, error) {
	return imagefs.Entry{}, os.ErrNotExist
}

type nonComparableDirectory struct {
	state []string
	err   error
}

func (d nonComparableDirectory) Stat() fs.FileMode       { return fs.ModeDir | 0o755 }
func (d nonComparableDirectory) ModTime() time.Time      { return time.Unix(0, 0) }
func (d nonComparableDirectory) Owner() (uint32, uint32) { return 0, 0 }
func (d nonComparableDirectory) RDev() uint32            { return 0 }
func (d nonComparableDirectory) ReadDir() ([]imagefs.DirEnt, error) {
	if d.err != nil {
		return nil, d.err
	}
	return []imagefs.DirEnt{{Name: "entry", Mode: 0o644}}, nil
}
func (d nonComparableDirectory) Lookup(string) (imagefs.Entry, error) {
	if d.err != nil {
		return imagefs.Entry{}, d.err
	}
	return imagefs.Entry{File: &snapshotFile{mode: 0o644, modTime: time.Unix(0, 0)}}, nil
}

func (d *retryLookupDirectory) Stat() fs.FileMode       { return fs.ModeDir | 0o755 }
func (d *retryLookupDirectory) ModTime() time.Time      { return time.Unix(0, 0) }
func (d *retryLookupDirectory) Owner() (uint32, uint32) { return 0, 0 }
func (d *retryLookupDirectory) RDev() uint32            { return 0 }
func (d *retryLookupDirectory) ReadDir() ([]imagefs.DirEnt, error) {
	return []imagefs.DirEnt{{Name: "important", Mode: 0o644}}, nil
}
func (d *retryLookupDirectory) Lookup(string) (imagefs.Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failures != 0 {
		d.failures--
		return imagefs.Entry{}, errors.New("transient lower lookup failure")
	}
	return imagefs.Entry{File: &snapshotFile{mode: 0o644, modTime: time.Unix(0, 0)}}, nil
}

func (d *blockingSnapshotDirectory) Stat() fs.FileMode       { return fs.ModeDir | 0o755 }
func (d *blockingSnapshotDirectory) ModTime() time.Time      { return time.Unix(0, 0) }
func (d *blockingSnapshotDirectory) Owner() (uint32, uint32) { return 0, 0 }
func (d *blockingSnapshotDirectory) RDev() uint32            { return 0 }
func (d *blockingSnapshotDirectory) ReadDir() ([]imagefs.DirEnt, error) {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return nil, nil
}
func (d *blockingSnapshotDirectory) ReadDirContext(ctx context.Context) ([]imagefs.DirEnt, error) {
	d.once.Do(func() { close(d.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.release:
		return nil, nil
	}
}
func (d *blockingSnapshotDirectory) Lookup(string) (imagefs.Entry, error) {
	return imagefs.Entry{}, os.ErrNotExist
}
func (d *blockingSnapshotDirectory) LookupContext(ctx context.Context, name string) (imagefs.Entry, error) {
	if err := ctx.Err(); err != nil {
		return imagefs.Entry{}, err
	}
	return d.Lookup(name)
}

type copyUpProbeFile struct {
	calls [][2]uint64
}

func (f *copyUpProbeFile) Stat() (uint64, fs.FileMode) { return (uint64(4) << 30) + 7, 0o644 }
func (f *copyUpProbeFile) ModTime() time.Time          { return time.Unix(0, 0) }
func (f *copyUpProbeFile) Owner() (uint32, uint32)     { return 0, 0 }
func (f *copyUpProbeFile) RDev() uint32                { return 0 }
func (f *copyUpProbeFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	f.calls = append(f.calls, [2]uint64{off, uint64(size)})
	if len(f.calls) == 1 {
		return make([]byte, size), nil
	}
	return nil, errors.New("injected lower-layer read failure")
}

func newEmptyImageFS(t *testing.T) *imageFS {
	t.Helper()
	backend, ok := NewImageFS(imagefs.NewOverlay(nil).Root(), "").(*imageFS)
	if !ok {
		t.Fatal("NewImageFS did not return an image filesystem")
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func createImageFile(t *testing.T, backend *imageFS, name, contents string) (uint64, uint64) {
	t.Helper()
	nodeID, fh, _, errno := backend.Create(1, name, linuxORDWR|linuxOCREAT|linuxOEXCL, 0o644, 0, 0)
	if errno != 0 {
		t.Fatalf("create %q: errno %d", name, errno)
	}
	if contents != "" {
		if _, errno := backend.Write(nodeID, fh, 0, []byte(contents), 0); errno != 0 {
			t.Fatalf("write %q: errno %d", name, errno)
		}
	}
	return nodeID, fh
}

func readImageHandle(t *testing.T, backend *imageFS, nodeID, fh uint64) string {
	t.Helper()
	data, errno := backend.Read(nodeID, fh, 0, 4096)
	if errno != 0 {
		t.Fatalf("read node %d handle %d: errno %d", nodeID, fh, errno)
	}
	return string(data)
}

func TestImageFSSnapshotCancellationDoesNotHoldFilesystemLock(t *testing.T) {
	lower := &blockingSnapshotDirectory{started: make(chan struct{}), release: make(chan struct{})}
	backend, ok := NewImageFS(lower, "").(*imageFS)
	if !ok {
		t.Fatal("NewImageFS did not return an image filesystem")
	}
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := backend.RootSnapshotContext(ctx)
		done <- err
	}()
	select {
	case <-lower.started:
	case <-time.After(time.Second):
		t.Fatal("snapshot did not begin lower directory materialization")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("snapshot error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot cancellation waited for lower directory materialization")
	}
	if _, _, errno := backend.Mkdir(1, "after-cancel", 0o755, 0, 0); errno != 0 {
		t.Fatalf("filesystem mutation after canceled snapshot: errno %d", errno)
	}
	close(lower.release)
}

func TestImageFSSnapshotCancellationDoesNotCancelSharedMaterialization(t *testing.T) {
	lower := &blockingSnapshotDirectory{started: make(chan struct{}), release: make(chan struct{})}
	backend := NewImageFS(lower, "").(*imageFS)
	defer backend.Close()
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() {
		_, err := backend.RootSnapshotContext(firstCtx)
		first <- err
	}()
	<-lower.started
	go func() {
		_, err := backend.RootSnapshotContext(t.Context())
		second <- err
	}()
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error = %v", err)
	}
	select {
	case err := <-second:
		t.Fatalf("shared snapshot returned before lower operation completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(lower.release)
	if err := <-second; err != nil {
		t.Fatalf("live snapshot failed after another waiter canceled: %v", err)
	}
}

func TestImageFSCloseQuarantinesBlockingLegacyMaterialization(t *testing.T) {
	lower := &legacyBlockingSnapshotDirectory{started: make(chan struct{}), release: make(chan struct{})}
	backend := NewImageFS(lower, "").(*imageFS)
	snapshotDone := make(chan error, 1)
	go func() {
		_, err := backend.RootSnapshotContext(context.Background())
		snapshotDone <- err
	}()
	<-lower.started
	if err := backend.closeWithin(20 * time.Millisecond); err == nil {
		t.Fatal("close silently returned while a lower filesystem operation remained live")
	}
	backend.dataStore.mu.Lock()
	closed := backend.dataStore.closed
	backend.dataStore.mu.Unlock()
	if closed {
		t.Fatal("backing store closed while the owned lower operation was still live")
	}
	close(lower.release)
	if err := <-snapshotDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot after close = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close after lower operation exited: %v", err)
	}
}

func TestImageFSHandlesNonComparableLowerDirectoryAndPreservesErrors(t *testing.T) {
	backend := NewImageFS(nonComparableDirectory{state: []string{"valid"}}, "").(*imageFS)
	if _, _, errno := backend.Lookup(1, "entry"); errno != 0 {
		t.Fatalf("lookup through non-comparable directory: errno %d", errno)
	}
	if _, err := backend.RootSnapshotContext(t.Context()); err != nil {
		t.Fatalf("snapshot through non-comparable directory: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	denied := NewImageFS(nonComparableDirectory{state: []string{"valid"}, err: fs.ErrPermission}, "").(*imageFS)
	defer denied.Close()
	if _, _, errno := denied.Lookup(1, "entry"); errno != -linuxEPERM {
		t.Fatalf("permission lookup errno = %d, want %d", errno, -linuxEPERM)
	}
	if _, errno := denied.OpenDir(1, 0); errno != -linuxEPERM {
		t.Fatalf("permission readdir errno = %d, want %d", errno, -linuxEPERM)
	}
}

func TestImageFSSnapshotRetriesTransientLowerLookupFailure(t *testing.T) {
	lower := &retryLookupDirectory{failures: 1}
	backend := NewImageFS(lower, "").(*imageFS)
	defer backend.Close()
	if _, err := backend.RootSnapshotContext(t.Context()); err == nil {
		t.Fatal("snapshot silently omitted a lower entry after lookup failure")
	}
	snapshot, err := backend.RootSnapshotContext(t.Context())
	if err != nil {
		t.Fatalf("snapshot retry: %v", err)
	}
	if _, err := snapshot.Lookup("important"); err != nil {
		t.Fatalf("retried snapshot omitted lower entry: %v", err)
	}
}

func TestImageFSOpenHandleSurvivesUnlinkAndReplacement(t *testing.T) {
	backend := newEmptyImageFS(t)
	unlinkedID, unlinkedFH := createImageFile(t, backend, "unlinked", "open-unlinked")
	if errno := backend.Unlink(1, "unlinked"); errno != 0 {
		t.Fatalf("unlink: errno %d", errno)
	}
	if got := readImageHandle(t, backend, unlinkedID, unlinkedFH); got != "open-unlinked" {
		t.Fatalf("open unlinked contents = %q", got)
	}

	targetID, targetFH := createImageFile(t, backend, "target", "old-target")
	sourceID, sourceFH := createImageFile(t, backend, "source", "new-target")
	backend.Release(sourceID, sourceFH)
	if errno := backend.Rename(1, "source", 1, "target", 0); errno != 0 {
		t.Fatalf("replace target: errno %d", errno)
	}
	if got := readImageHandle(t, backend, targetID, targetFH); got != "old-target" {
		t.Fatalf("open replaced contents = %q", got)
	}
	replacementID, _, errno := backend.Lookup(1, "target")
	if errno != 0 {
		t.Fatalf("lookup replacement: errno %d", errno)
	}
	replacementFH, errno := backend.Open(replacementID, linuxORDONLY)
	if errno != 0 {
		t.Fatalf("open replacement: errno %d", errno)
	}
	if got := readImageHandle(t, backend, replacementID, replacementFH); got != "new-target" {
		t.Fatalf("replacement contents = %q", got)
	}
}

func TestImageFSCopyUpKeepsLargeLowerFileAsPageOverlay(t *testing.T) {
	backend := newEmptyImageFS(t)
	probe := &copyUpProbeFile{}
	node := &imageNode{abstractFile: probe}
	if errno := backend.copyUpFileLocked(node); errno != 0 {
		t.Fatalf("copy-up: errno %d", errno)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("metadata copy-up read the lower file: %#v", probe.calls)
	}
	if node.abstractFile != nil || node.lowerFile != probe || node.size != (uint64(4)<<30)+7 || len(node.data.extents) != 0 {
		t.Fatalf("lazy copy-up state: node=%+v", node)
	}
	if errno := backend.prepareImageOverlayWriteLocked(node, 1, 1); errno != 0 {
		t.Fatalf("prepare partial page: errno %d", errno)
	}
	if len(probe.calls) != 1 || probe.calls[0] != [2]uint64{0, imageDataPageSize} {
		t.Fatalf("partial write lower reads = %#v, want one page", probe.calls)
	}
	if current, _, _, _ := backend.dataStore.usage(); current != imageDataPageSize {
		t.Fatalf("partial write retained %d backing bytes, want one page", current)
	}
}

func TestImageFSPageOverlayPreservesLowerBytesAndSnapshots(t *testing.T) {
	want := make([]byte, 3*imageDataPageSize)
	for i := range want {
		want[i] = byte(i % 251)
	}
	backend := imageBackend(t, map[string]string{"/lower": string(want)}).(*imageFS)
	nodeID, _, errno := backend.Lookup(1, "lower")
	if errno != 0 {
		t.Fatalf("lookup lower: errno %d", errno)
	}
	fh, errno := backend.Open(nodeID, linuxORDWR)
	if errno != 0 {
		t.Fatalf("open lower: errno %d", errno)
	}
	want[imageDataPageSize+17] = 0xfe
	if _, errno := backend.Write(nodeID, fh, imageDataPageSize+17, []byte{0xfe}, 0); errno != 0 {
		t.Fatalf("write overlay: errno %d", errno)
	}
	got, errno := backend.Read(nodeID, fh, 0, uint32(len(want)))
	if errno != 0 || !bytes.Equal(got, want) {
		t.Fatalf("overlay read errno=%d equal=%v", errno, bytes.Equal(got, want))
	}
	if current, _, _, _ := backend.dataStore.usage(); current != imageDataPageSize {
		t.Fatalf("one-byte overlay allocated %d bytes, want one page", current)
	}
	snapshot, err := backend.RootSnapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	entry, err := snapshot.Lookup("lower")
	if err != nil {
		t.Fatalf("snapshot lookup: %v", err)
	}
	snapshotData, err := entry.File.ReadAt(0, uint32(len(want)))
	if err != nil || !bytes.Equal(snapshotData, want) {
		t.Fatalf("snapshot overlay read err=%v equal=%v", err, bytes.Equal(snapshotData, want))
	}
	if current, _, _, _ := backend.dataStore.usage(); current != imageDataPageSize {
		t.Fatalf("snapshot duplicated backing pages: %d", current)
	}
	want[imageDataPageSize+17] = 0xfd
	if _, errno := backend.Write(nodeID, fh, imageDataPageSize+17, []byte{0xfd}, 0); errno != 0 {
		t.Fatalf("write after snapshot: errno %d", errno)
	}
	if current, _, _, _ := backend.dataStore.usage(); current != 2*imageDataPageSize {
		t.Fatalf("post-snapshot COW backing = %d, want two versions of one page", current)
	}
	snapshotData, err = entry.File.ReadAt(0, uint32(len(want)))
	if err != nil || snapshotData[imageDataPageSize+17] != 0xfe {
		t.Fatalf("snapshot changed after live write: byte=%#x err=%v", snapshotData[imageDataPageSize+17], err)
	}
	closer, ok := snapshot.(interface{ Close() error })
	if !ok {
		t.Fatal("snapshot has no deterministic close operation")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if current, _, _, _ := backend.dataStore.usage(); current != imageDataPageSize {
		t.Fatalf("released snapshot retained old COW page: %d", current)
	}
}

func TestImageFSSequentialWritesUseCompactExtentsAndReportMetadata(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, fh := createImageFile(t, backend, "sequential", "")
	page := make([]byte, imageDataPageSize)
	for off := uint64(0); off < 8<<20; off += imageDataPageSize {
		if _, errno := backend.Write(nodeID, fh, off, page, 0); errno != 0 {
			t.Fatalf("write at %d: errno %d", off, errno)
		}
	}
	backend.mu.Lock()
	extents := append([]imageDataExtent(nil), backend.nodes[nodeID].data.extents...)
	backend.mu.Unlock()
	if len(extents) != 1 || extents[0].count != (8<<20)/imageDataPageSize {
		t.Fatalf("sequential page index = %+v, want one extent", extents)
	}
	metadata, highWater := backend.BackingMetadataUsage()
	if metadata == 0 || highWater < metadata {
		t.Fatalf("metadata usage = current %d high-water %d", metadata, highWater)
	}
}

func TestImageFSRenameExchangeAndSameInodeNoOp(t *testing.T) {
	backend := newEmptyImageFS(t)
	aID, aFH := createImageFile(t, backend, "a", "AAA")
	bID, bFH := createImageFile(t, backend, "b", "BBB")
	backend.Release(aID, aFH)
	backend.Release(bID, bFH)
	if errno := backend.Rename(1, "a", 1, "b", linuxRenameExchange); errno != 0 {
		t.Fatalf("exchange: errno %d", errno)
	}
	for name, want := range map[string]string{"a": "BBB", "b": "AAA"} {
		nodeID, _, errno := backend.Lookup(1, name)
		if errno != 0 {
			t.Fatalf("lookup %q: errno %d", name, errno)
		}
		fh, errno := backend.Open(nodeID, linuxORDONLY)
		if errno != 0 {
			t.Fatalf("open %q: errno %d", name, errno)
		}
		if got := readImageHandle(t, backend, nodeID, fh); got != want {
			t.Fatalf("%s contents = %q, want %q", name, got, want)
		}
		backend.Release(nodeID, fh)
	}

	linkID, _, errno := backend.Link(aID, 1, "a-link")
	if errno != 0 {
		t.Fatalf("link: errno %d", errno)
	}
	if errno := backend.Rename(1, "b", 1, "a-link", 0); errno != 0 {
		t.Fatalf("same-inode rename: errno %d", errno)
	}
	for _, name := range []string{"b", "a-link"} {
		gotID, attr, errno := backend.Lookup(1, name)
		if errno != 0 || gotID != linkID || attr.NLink != 2 {
			t.Fatalf("lookup %q = node %d nlink %d errno %d", name, gotID, attr.NLink, errno)
		}
	}
}

func TestImageFSMetadataAndSparseStorage(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, fh := createImageFile(t, backend, "sparse", "")
	backend.Release(nodeID, fh)
	wantTime := time.Unix(1_234_567_890, 123)
	if _, errno := backend.SetAttr(nodeID, fattrMTime, 0, 0, 0, 0, 0, time.Time{}, wantTime); errno != 0 {
		t.Fatalf("set mtime: errno %d", errno)
	}
	if _, errno := backend.SetAttr(nodeID, fattrMode, 0, 0, 0o600, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("chmod: errno %d", errno)
	}
	attr, errno := backend.GetAttr(nodeID)
	if errno != 0 || attr.MTimeSec != uint64(wantTime.Unix()) || attr.MTimeNsec != uint32(wantTime.Nanosecond()) {
		t.Fatalf("mtime after chmod = %d.%d errno %d", attr.MTimeSec, attr.MTimeNsec, errno)
	}

	const logicalSize = uint64(64 << 20)
	if _, errno := backend.SetAttr(nodeID, fattrSize, 0, logicalSize, 0, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("truncate sparse file: errno %d", errno)
	}
	attr, errno = backend.GetAttr(nodeID)
	if errno != 0 || attr.Size != logicalSize || attr.Blocks != 0 {
		t.Fatalf("sparse attr = size %d blocks %d errno %d", attr.Size, attr.Blocks, errno)
	}
	fh, errno = backend.Open(nodeID, linuxORDWR)
	if errno != 0 {
		t.Fatalf("open sparse file: errno %d", errno)
	}
	if _, errno := backend.Write(nodeID, fh, logicalSize-1, []byte{'x'}, 0); errno != 0 {
		t.Fatalf("write sparse tail: errno %d", errno)
	}
	attr, _ = backend.GetAttr(nodeID)
	if attr.Blocks != imageDataPageSize/512 {
		t.Fatalf("sparse blocks = %d, want %d", attr.Blocks, imageDataPageSize/512)
	}
}

func TestImageFSWrittenDataDoesNotAccumulateInHostHeap(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, fh := createImageFile(t, backend, "large", "")
	page := make([]byte, imageDataPageSize)
	for i := range page {
		page[i] = byte(i)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	const written = 32 << 20
	for off := 0; off < written; off += len(page) {
		if _, errno := backend.Write(nodeID, fh, uint64(off), page, 0); errno != 0 {
			t.Fatalf("write at %d: errno %d", off, errno)
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if heapGrowth > 8<<20 {
		t.Fatalf("writing %d bytes retained %d bytes of Go heap", written, heapGrowth)
	}
	if backend.dataStore.file == nil {
		t.Fatal("writable data was not placed in the backing store")
	}
	if info, err := backend.dataStore.file.Stat(); err != nil || info.Size() < written {
		t.Fatalf("backing store stat = %#v, %v", info, err)
	}
}

func TestImageFSDeletedDataReclaimsBackingStore(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, fh := createImageFile(t, backend, "temporary", "")
	page := make([]byte, imageDataPageSize)
	const written = 8 << 20
	for off := 0; off < written; off += len(page) {
		if _, errno := backend.Write(nodeID, fh, uint64(off), page, 0); errno != 0 {
			t.Fatalf("write at %d: errno %d", off, errno)
		}
	}
	current, highWater, _, err := backend.BackingUsage()
	if err != nil || current != written || highWater != written {
		t.Fatalf("usage after write = current %d high-water %d error %v", current, highWater, err)
	}
	if errno := backend.Unlink(1, "temporary"); errno != 0 {
		t.Fatalf("unlink: errno %d", errno)
	}
	// POSIX keeps the unlinked file alive while its handle is open.
	if current, _, _, _ := backend.BackingUsage(); current != written {
		t.Fatalf("usage with open unlinked handle = %d, want %d", current, written)
	}
	backend.Release(nodeID, fh)
	current, highWater, _, err = backend.BackingUsage()
	if err != nil || current != 0 || highWater != written {
		t.Fatalf("usage after final release = current %d high-water %d error %v", current, highWater, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		info, statErr := backend.dataStore.file.Stat()
		if statErr == nil && info.Size() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backing store after asynchronous release = %#v, %v", info, statErr)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestImageFSDirectoryAndFileLinkCounts(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, fh := createImageFile(t, backend, "one", "data")
	backend.Release(nodeID, fh)
	if _, _, errno := backend.Link(nodeID, 1, "two"); errno != 0 {
		t.Fatalf("link: errno %d", errno)
	}
	for _, name := range []string{"one", "two"} {
		_, attr, errno := backend.Lookup(1, name)
		if errno != 0 || attr.NLink != 2 {
			t.Fatalf("%s nlink = %d errno %d", name, attr.NLink, errno)
		}
	}
	if errno := backend.Unlink(1, "one"); errno != 0 {
		t.Fatalf("unlink one: errno %d", errno)
	}
	_, attr, errno := backend.Lookup(1, "two")
	if errno != 0 || attr.NLink != 1 {
		t.Fatalf("remaining nlink = %d errno %d", attr.NLink, errno)
	}
	rootAttr, errno := backend.GetAttr(1)
	if errno != 0 || rootAttr.NLink != 2 {
		t.Fatalf("root with regular files nlink = %d errno %d", rootAttr.NLink, errno)
	}
	if _, _, errno := backend.Mkdir(1, "dir", 0o755, 0, 0); errno != 0 {
		t.Fatalf("mkdir: errno %d", errno)
	}
	rootAttr, _ = backend.GetAttr(1)
	if rootAttr.NLink != 3 {
		t.Fatalf("root with subdirectory nlink = %d", rootAttr.NLink)
	}
}

func TestImageFSOpenDirectorySurvivesRemoval(t *testing.T) {
	backend := newEmptyImageFS(t)
	dirID, _, errno := backend.Mkdir(1, "removed", 0o755, 0, 0)
	if errno != 0 {
		t.Fatalf("mkdir: errno %d", errno)
	}
	fh, errno := backend.OpenDir(dirID, 0)
	if errno != 0 {
		t.Fatalf("opendir: errno %d", errno)
	}
	if errno := backend.RmDir(1, "removed"); errno != 0 {
		t.Fatalf("rmdir: errno %d", errno)
	}
	if _, errno := backend.GetAttr(dirID); errno != 0 {
		t.Fatalf("getattr through open directory: errno %d", errno)
	}
	if entries, errno := backend.ReadDir(dirID, fh, 0, 4096); errno != 0 || len(entries) == 0 {
		t.Fatalf("readdir through removed directory: bytes=%d errno=%d", len(entries), errno)
	}
	backend.ReleaseDir(dirID, fh)
	if _, errno := backend.GetAttr(dirID); errno != -linuxENOENT {
		t.Fatalf("getattr after released directory: errno %d, want %d", errno, -linuxENOENT)
	}
}

func TestImageFSReadsLargeDirectoryWithoutCopyingItPerPage(t *testing.T) {
	backend := newEmptyImageFS(t)
	const files = 10000
	for i := range files {
		nodeID, fh, _, errno := backend.Create(1, fmt.Sprintf("entry-%05d", i), linuxOWRONLY|linuxOCREAT, 0o644, 0, 0)
		if errno != 0 {
			t.Fatalf("create entry %d: errno %d", i, errno)
		}
		backend.Release(nodeID, fh)
	}
	fh, errno := backend.OpenDir(1, 0)
	if errno != 0 {
		t.Fatalf("open large directory: errno %d", errno)
	}
	defer backend.ReleaseDir(1, fh)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	offset := uint64(0)
	entries := 0
	for {
		page, errno := backend.ReadDir(1, fh, offset, 128)
		if errno != 0 {
			t.Fatalf("read directory at offset %d: errno %d", offset, errno)
		}
		if len(page) == 0 {
			break
		}
		for cursor := 0; cursor < len(page); {
			if len(page)-cursor < fuseDirentBaseSize {
				t.Fatalf("short directory record at offset %d", offset)
			}
			nameBytes := int(binary.LittleEndian.Uint32(page[cursor+16 : cursor+20]))
			recordBytes := align8(fuseDirentBaseSize + nameBytes)
			if recordBytes > len(page)-cursor {
				t.Fatalf("directory record exceeds page at offset %d", offset)
			}
			offset = binary.LittleEndian.Uint64(page[cursor+8 : cursor+16])
			entries++
			cursor += recordBytes
		}
	}
	runtime.ReadMemStats(&after)
	if entries != files+2 {
		t.Fatalf("large directory entries = %d, want %d", entries, files+2)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("paged large-directory read allocated %d bytes", allocated)
	}
}

func TestImageFSRootSurvivesDirectoryHandleRelease(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, fileHandle := createImageFile(t, backend, "still-here", "data")
	backend.Release(nodeID, fileHandle)
	fh, errno := backend.OpenDir(1, 0)
	if errno != 0 {
		t.Fatalf("open root: errno %d", errno)
	}
	backend.ReleaseDir(1, fh)
	if _, errno := backend.GetAttr(1); errno != 0 {
		t.Fatalf("getattr root after release: errno %d", errno)
	}
	if _, _, errno := backend.Lookup(1, "still-here"); errno != 0 {
		t.Fatalf("lookup after root release: errno %d", errno)
	}
}

func TestImageFSSetgidInheritanceAndPrivilegeBitClearing(t *testing.T) {
	backend := newEmptyImageFS(t)
	dirID, _, errno := backend.Mkdir(1, "shared", 0o2775, 0, 42)
	if errno != 0 {
		t.Fatalf("mkdir shared: errno %d", errno)
	}
	childDir, childAttr, errno := backend.Mkdir(dirID, "child", 0o775, 1000, 1000)
	if errno != 0 || childAttr.GID != 42 || childAttr.Mode&0o2000 == 0 {
		t.Fatalf("child directory attr = %+v errno %d", childAttr, errno)
	}
	_, _ = childDir, childAttr
	fileID, fh, fileAttr, errno := backend.Create(dirID, "tool", linuxORDWR|linuxOCREAT, 0o6755, 1000, 1000)
	if errno != 0 || fileAttr.GID != 42 {
		t.Fatalf("inherited file attr = %+v errno %d", fileAttr, errno)
	}
	if _, errno := backend.WriteForCaller(fileID, fh, 0, []byte("x"), 0, 1000, 42); errno != 0 {
		t.Fatalf("write privileged file: errno %d", errno)
	}
	fileAttr, _ = backend.GetAttr(fileID)
	if fileAttr.Mode&0o6000 != 0 {
		t.Fatalf("privilege bits after non-root write = %#o", fileAttr.Mode)
	}
	if _, errno := backend.SetAttr(fileID, fattrMode, 0, 0, 0o6755, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("restore privilege bits: errno %d", errno)
	}
	if _, errno := backend.SetAttr(fileID, fattrGID, 0, 0, 0, 0, 77, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("chgrp privileged file: errno %d", errno)
	}
	fileAttr, _ = backend.GetAttr(fileID)
	if fileAttr.GID != 77 || fileAttr.Mode&0o6000 != 0 {
		t.Fatalf("attr after chgrp = %+v", fileAttr)
	}
}

func TestImageFSXattrsAndSparseSeek(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, fh := createImageFile(t, backend, "data", "")
	if errno := backend.SetXattr(nodeID, "user.test", []byte{0, 1, 2, 0xff}, 0); errno != 0 {
		t.Fatalf("setxattr: errno %d", errno)
	}
	value, errno := backend.GetXattr(nodeID, "user.test")
	if errno != 0 || string(value) != string([]byte{0, 1, 2, 0xff}) {
		t.Fatalf("getxattr = %v errno %d", value, errno)
	}
	names, errno := backend.ListXattr(nodeID)
	if errno != 0 || string(names) != "user.test\x00" {
		t.Fatalf("listxattr = %q errno %d", names, errno)
	}
	if errno := backend.RemoveXattr(nodeID, "user.test"); errno != 0 {
		t.Fatalf("removexattr: errno %d", errno)
	}
	if _, errno := backend.GetXattr(nodeID, "user.test"); errno != -linuxENODATA {
		t.Fatalf("get removed xattr errno = %d, want %d", errno, -linuxENODATA)
	}
	if _, errno := backend.Write(nodeID, fh, 2*imageDataPageSize, []byte("tail"), 0); errno != 0 {
		t.Fatalf("write sparse extent: errno %d", errno)
	}
	if got, errno := backend.Lseek(nodeID, fh, 0, 3); errno != 0 || got != 2*imageDataPageSize {
		t.Fatalf("SEEK_DATA = %d errno %d", got, errno)
	}
	if got, errno := backend.Lseek(nodeID, fh, 2*imageDataPageSize, 4); errno != 0 || got != 2*imageDataPageSize+4 {
		t.Fatalf("SEEK_HOLE = %d errno %d", got, errno)
	}
}

func TestImageFSBoundsAggregateExtendedAttributeMemory(t *testing.T) {
	backend := newEmptyImageFS(t)
	nodeID, _ := createImageFile(t, backend, "xattrs", "")
	value := make([]byte, 60<<10)
	accepted := 0
	for i := 0; i < 128; i++ {
		errno := backend.SetXattr(nodeID, fmt.Sprintf("user.value-%d", i), value, 0)
		if errno == -linuxENOSPC {
			break
		}
		if errno != 0 {
			t.Fatalf("set xattr %d: errno %d", i, errno)
		}
		accepted++
	}
	if accepted == 0 || accepted >= 128 {
		t.Fatalf("aggregate xattr budget accepted %d entries", accepted)
	}
	if errno := backend.RemoveXattr(nodeID, "user.value-0"); errno != 0 {
		t.Fatalf("remove xattr: errno %d", errno)
	}
	if errno := backend.SetXattr(nodeID, "user.replacement", value, 0); errno != 0 {
		t.Fatalf("released xattr budget was not reusable: errno %d", errno)
	}
}

func TestSparseImageAllocatedBytesUsesCompactExtents(t *testing.T) {
	const pages = uint64(1 << 28)
	data := sparseImageData{extents: []imageDataExtent{{page: 0, count: pages}}}
	if got, want := data.allocatedBytes(pages*imageDataPageSize-17), pages*imageDataPageSize-17; got != want {
		t.Fatalf("allocated bytes = %d, want %d", got, want)
	}
	data.extents = []imageDataExtent{{page: 7, count: 3}}
	if got, want := data.allocatedBytes(9*imageDataPageSize+23), 2*imageDataPageSize+23; got != want {
		t.Fatalf("partial allocated bytes = %d, want %d", got, want)
	}
}

func TestImageFSMetadataHighWaterSurvivesUnpolledChurnAndCompacts(t *testing.T) {
	backend := newEmptyImageFS(t)
	before, _ := backend.BackingMetadataUsage()
	for i := 0; i < 128; i++ {
		name := fmt.Sprintf("entry-%03d", i)
		nodeID, fh := createImageFile(t, backend, name, "")
		backend.Release(nodeID, fh)
	}
	for i := 0; i < 128; i++ {
		if errno := backend.Unlink(1, fmt.Sprintf("entry-%03d", i)); errno != 0 {
			t.Fatalf("unlink %d: errno %d", i, errno)
		}
	}
	after, highWater := backend.BackingMetadataUsage()
	if highWater <= after || highWater <= before {
		t.Fatalf("metadata churn current=%d high-water=%d before=%d", after, highWater, before)
	}
	if after > before+16<<10 {
		t.Fatalf("metadata maps did not compact after churn: before=%d after=%d", before, after)
	}
}

func TestImageFSHandleMapsReleaseBurstCapacity(t *testing.T) {
	backend := newEmptyImageFS(t)
	type opened struct{ node, handle uint64 }
	handles := make([]opened, 128)
	for i := range handles {
		node, handle := createImageFile(t, backend, fmt.Sprintf("open-%03d", i), "")
		handles[i] = opened{node: node, handle: handle}
	}
	for _, opened := range handles {
		backend.Release(opened.node, opened.handle)
	}
	backend.mu.Lock()
	retained := backend.retainedHandles
	active := len(backend.handles)
	backend.mu.Unlock()
	if active != 0 || retained != 0 {
		t.Fatalf("released handles active=%d retained=%d", active, retained)
	}
}

func TestImageFSNamespaceIsSharedWhileGuestChangesRemainPrivate(t *testing.T) {
	overlay := imagefs.NewOverlay(nil)
	if err := overlay.AddDir("/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := overlay.AddFile("/etc/config", 0o644, []byte("shared")); err != nil {
		t.Fatal(err)
	}
	namespace, err := imagefs.BuildNamespace(overlay.Root())
	if err != nil {
		t.Fatal(err)
	}
	root := &namespaceTestDirectory{Directory: overlay.Root(), namespace: namespace}
	first := NewImageFS(root, "").(*imageFS)
	second := NewImageFS(root, "").(*imageFS)
	if len(first.baseNodes) != len(namespace.Nodes) || first.baseNodes[1] != second.baseNodes[1] {
		t.Fatalf("namespace nodes were not shared: first=%d second=%d", len(first.baseNodes), len(second.baseNodes))
	}
	etcFirst, _, errno := first.Lookup(1, "etc")
	if errno != 0 {
		t.Fatalf("first lookup etc: errno %d", errno)
	}
	configFirst, _, errno := first.Lookup(etcFirst, "config")
	if errno != 0 {
		t.Fatalf("first lookup config: errno %d", errno)
	}
	if _, errno := first.SetAttr(configFirst, fattrMode, 0, 0, 0o600, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("chmod first config: errno %d", errno)
	}
	if errno := first.Unlink(etcFirst, "config"); errno != 0 {
		t.Fatalf("unlink first config: errno %d", errno)
	}
	if _, _, errno := first.Lookup(etcFirst, "config"); errno != -linuxENOENT {
		t.Fatalf("first config after unlink: errno %d", errno)
	}

	etcSecond, _, errno := second.Lookup(1, "etc")
	if errno != 0 {
		t.Fatalf("second lookup etc: errno %d", errno)
	}
	configSecond, attr, errno := second.Lookup(etcSecond, "config")
	if errno != 0 {
		t.Fatalf("second lookup config: errno %d", errno)
	}
	if configSecond != configFirst || attr.Mode&linuxPermMask != 0o644 {
		t.Fatalf("second config node=%d mode=%#o, want node=%d mode=0644", configSecond, attr.Mode, configFirst)
	}
}

func TestImageFSLargeImmutableNamespaceDoesNotMaterializePerVMNodes(t *testing.T) {
	const files = 10000
	overlay := imagefs.NewOverlay(nil)
	for i := range files {
		if err := overlay.AddFile(fmt.Sprintf("/entry-%05d", i), 0o644, nil); err != nil {
			t.Fatalf("add entry %d: %v", i, err)
		}
	}
	namespace, err := imagefs.BuildNamespace(overlay.Root())
	if err != nil {
		t.Fatal(err)
	}
	backend := NewImageFS(&namespaceTestDirectory{Directory: overlay.Root(), namespace: namespace}, "").(*imageFS)
	if len(backend.nodes) != 0 {
		t.Fatalf("new backend has %d private nodes", len(backend.nodes))
	}
	nodeID, _, errno := backend.Lookup(1, "entry-09999")
	if errno != 0 {
		t.Fatalf("lookup indexed entry: errno %d", errno)
	}
	fh, errno := backend.OpenDir(1, 0)
	if errno != 0 {
		t.Fatalf("open indexed root: errno %d", errno)
	}
	defer backend.ReleaseDir(1, fh)
	if page, errno := backend.ReadDir(1, fh, 0, 4096); errno != 0 || len(page) == 0 {
		t.Fatalf("read indexed root: bytes=%d errno=%d", len(page), errno)
	}
	if len(backend.nodes) != 0 {
		t.Fatalf("lookup and readdir created %d private nodes", len(backend.nodes))
	}
	if nodeID == 0 {
		t.Fatal("indexed lookup returned the root node")
	}
}

func BenchmarkImageFSLargeImmutableDirectoryOpen(b *testing.B) {
	const files = 30000
	overlay := imagefs.NewOverlay(nil)
	for i := range files {
		if err := overlay.AddFile(fmt.Sprintf("/entry-%05d", i), 0o644, nil); err != nil {
			b.Fatal(err)
		}
	}
	namespace, err := imagefs.BuildNamespace(overlay.Root())
	if err != nil {
		b.Fatal(err)
	}
	indexed := &namespaceTestDirectory{Directory: overlay.Root(), namespace: namespace}
	for _, benchmark := range []struct {
		name string
		root imagefs.Directory
	}{
		{name: "indexed", root: indexed},
		{name: "lazy", root: overlay.Root()},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				backend := NewImageFS(benchmark.root, "").(*imageFS)
				fh, errno := backend.OpenDir(1, 0)
				if errno != 0 {
					b.Fatalf("opendir: errno %d", errno)
				}
				backend.ReleaseDir(1, fh)
				if err := backend.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
