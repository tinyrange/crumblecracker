package virtio

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

type mountedLifecycleBackend struct {
	FSBackend
	current, highWater, physical uint64
	metadata, metadataHighWater  uint64
	usageErr                     error
	closes                       int
	closeErr                     error
}

type retryCloseBackend struct {
	FSBackend
	closes int
}

type blockingCloseBackend struct {
	FSBackend
	started chan struct{}
	release chan struct{}
	closes  int
}

type blockingBeginCloseBackend struct {
	FSBackend
	started chan struct{}
	release chan struct{}
	closes  int
}

type nonComparableFSBackend struct {
	FSBackend
	state []string
}

type countingReadDirBackend struct {
	FSBackend
	reads int
}

func (b *countingReadDirBackend) ReadDir(nodeID uint64, fh uint64, off uint64, maxBytes uint32) ([]byte, int32) {
	b.reads++
	return b.FSBackend.ReadDir(nodeID, fh, off, maxBytes)
}

func (b nonComparableFSBackend) Create(parent uint64, name string, flags uint32, mode uint32, uid uint32, gid uint32) (uint64, uint64, FuseAttr, int32) {
	return b.FSBackend.(fsCreateBackend).Create(parent, name, flags, mode, uid, gid)
}

func (b nonComparableFSBackend) Link(nodeID uint64, newParent uint64, newName string) (uint64, FuseAttr, int32) {
	return b.FSBackend.(fsLinkBackend).Link(nodeID, newParent, newName)
}

func (b nonComparableFSBackend) Rename(parent uint64, name string, newParent uint64, newName string, flags uint32) int32 {
	return b.FSBackend.(fsRenameBackend).Rename(parent, name, newParent, newName, flags)
}

func (b nonComparableFSBackend) Unlink(parent uint64, name string) int32 {
	return b.FSBackend.(fsUnlinkBackend).Unlink(parent, name)
}

func (b *retryCloseBackend) Close() error {
	b.closes++
	if b.closes == 1 {
		return &CloseIncompleteError{Resource: "test backend", Timeout: time.Millisecond}
	}
	return nil
}

func (b *blockingCloseBackend) Close() error {
	b.closes++
	close(b.started)
	<-b.release
	return nil
}

func (b *blockingBeginCloseBackend) BeginClose() {
	close(b.started)
	<-b.release
}

func (b *blockingBeginCloseBackend) Close() error {
	b.closes++
	return nil
}

func (b *mountedLifecycleBackend) BackingUsage() (uint64, uint64, uint64, error) {
	return b.current, b.highWater, b.physical, b.usageErr
}

func (b *mountedLifecycleBackend) BackingMetadataUsage() (uint64, uint64) {
	return b.metadata, b.metadataHighWater
}

func (b *mountedLifecycleBackend) Close() error {
	b.closes++
	return b.closeErr
}

func TestMountedFSForwardsBackingLifecycleOncePerBackend(t *testing.T) {
	rootErr := errors.New("root reclaim degraded")
	closeErr := errors.New("share close failed")
	root := &mountedLifecycleBackend{FSBackend: imageBackend(t, nil), current: 10, highWater: 20, metadata: 3, metadataHighWater: 8, usageErr: rootErr}
	share := &mountedLifecycleBackend{FSBackend: imageBackend(t, nil), current: 30, highWater: 40, metadata: 5, metadataHighWater: 9, closeErr: closeErr}
	fsys := NewMountedFS(root, []ShareMount{
		{GuestPath: "/one", Backend: share},
		{GuestPath: "/two", Backend: share},
	}).(*mountedFS)

	current, highWater, _, err := fsys.BackingUsage()
	if current != 40 || highWater != 40 || !errors.Is(err, rootErr) {
		t.Fatalf("backing usage = %d, %d, %v", current, highWater, err)
	}
	if current, highWater := fsys.BackingMetadataUsage(); current != 8 || highWater != 9 {
		t.Fatalf("metadata usage = %d, %d", current, highWater)
	}
	if err := fsys.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v", err)
	}
	if err := fsys.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("second close error = %v", err)
	}
	if root.closes != 1 || share.closes != 1 {
		t.Fatalf("close counts root=%d share=%d, want one each", root.closes, share.closes)
	}
}

func TestMountedFSRetriesOnlyIncompleteClose(t *testing.T) {
	backend := &retryCloseBackend{FSBackend: imageBackend(t, nil)}
	fsys := NewMountedFS(backend, nil).(*mountedFS)
	var incomplete *CloseIncompleteError
	if err := fsys.Close(); !errors.As(err, &incomplete) {
		t.Fatalf("first close = %v, want incomplete ownership", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("completed close replay: %v", err)
	}
	if backend.closes != 2 {
		t.Fatalf("backend close calls = %d, want one retry then cached success", backend.closes)
	}
}

func TestVirtioFSCloseBoundsWorkerWaitAndRetriesBackend(t *testing.T) {
	backend := &retryCloseBackend{FSBackend: imageBackend(t, nil)}
	fsys := NewFS(0, 0, 0, "test", backend)
	fsys.workerWG.Add(1)
	var incomplete *CloseIncompleteError
	if err := fsys.closeWithin(20 * time.Millisecond); !errors.As(err, &incomplete) {
		t.Fatalf("close with live worker = %v, want incomplete ownership", err)
	}
	if backend.closes != 0 {
		t.Fatal("backend closed while a device worker could still be using it")
	}
	fsys.workerWG.Done()
	if err := fsys.Close(); !errors.As(err, &incomplete) {
		t.Fatalf("first backend close = %v, want retryable incomplete result", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("backend close retry: %v", err)
	}
	if backend.closes != 2 {
		t.Fatalf("backend close calls = %d, want two", backend.closes)
	}
}

func TestMountedFSCloseBoundsAllBackendsWithOneDeadline(t *testing.T) {
	root := &blockingCloseBackend{FSBackend: imageBackend(t, nil), started: make(chan struct{}), release: make(chan struct{})}
	share := &blockingCloseBackend{FSBackend: imageBackend(t, nil), started: make(chan struct{}), release: make(chan struct{})}
	fsys := NewMountedFS(root, []ShareMount{{GuestPath: "/share", Backend: share}}).(*mountedFS)
	var incomplete *CloseIncompleteError
	if err := fsys.closeWithin(20 * time.Millisecond); !errors.As(err, &incomplete) {
		t.Fatalf("close with blocked backends = %v, want incomplete ownership", err)
	}
	for name, started := range map[string]<-chan struct{}{"root": root.started, "share": share.started} {
		select {
		case <-started:
		default:
			t.Fatalf("%s backend was not closed concurrently", name)
		}
	}
	close(root.release)
	close(share.release)
	if err := fsys.Close(); err != nil {
		t.Fatalf("resume retained backend closes: %v", err)
	}
	if root.closes != 1 || share.closes != 1 {
		t.Fatalf("retained backend close calls root=%d share=%d, want one each", root.closes, share.closes)
	}
}

func TestVirtioFSCloseRetainsTimedOutBackendAttempt(t *testing.T) {
	backend := &blockingCloseBackend{FSBackend: imageBackend(t, nil), started: make(chan struct{}), release: make(chan struct{})}
	fsys := NewFS(0, 0, 0, "test", backend)
	var incomplete *CloseIncompleteError
	if err := fsys.closeWithin(20 * time.Millisecond); !errors.As(err, &incomplete) {
		t.Fatalf("close with blocked backend = %v, want incomplete ownership", err)
	}
	select {
	case <-backend.started:
	default:
		t.Fatal("backend close was not started")
	}
	close(backend.release)
	if err := fsys.Close(); err != nil {
		t.Fatalf("resume retained backend close: %v", err)
	}
	if backend.closes != 1 {
		t.Fatalf("backend close calls = %d, want retained single attempt", backend.closes)
	}
}

func TestVirtioFSCloseBoundsBackendShutdownSignal(t *testing.T) {
	backend := &blockingBeginCloseBackend{FSBackend: imageBackend(t, nil), started: make(chan struct{}), release: make(chan struct{})}
	fsys := NewFS(0, 0, 0, "test", backend)
	var incomplete *CloseIncompleteError
	if err := fsys.closeWithin(20 * time.Millisecond); !errors.As(err, &incomplete) {
		t.Fatalf("close with blocked shutdown signal = %v, want incomplete ownership", err)
	}
	select {
	case <-backend.started:
	default:
		t.Fatal("backend shutdown signal was not started")
	}
	if backend.closes != 0 {
		t.Fatal("backend close started before its shutdown signal returned")
	}
	close(backend.release)
	if err := fsys.Close(); err != nil {
		t.Fatalf("resume close after shutdown signal: %v", err)
	}
	if backend.closes != 1 {
		t.Fatalf("backend close calls = %d, want one", backend.closes)
	}
}

func TestMountedFSUsesStableRouteIdentityForNonComparableBackend(t *testing.T) {
	backend := nonComparableFSBackend{FSBackend: imageBackend(t, nil), state: []string{"valid"}}
	fsys := NewMountedFS(backend, nil).(*mountedFS)
	nodeID, fh, _, errno := fsys.Create(1, "source", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o644, 0, 0)
	if errno != 0 {
		t.Fatalf("create: errno %d", errno)
	}
	if _, _, errno := fsys.Link(nodeID, 1, "linked"); errno != 0 {
		t.Fatalf("same-route hard link: errno %d", errno)
	}
	if errno := fsys.Rename(1, "linked", 1, "renamed", 0); errno != 0 {
		t.Fatalf("same-route rename: errno %d", errno)
	}
	if errno := fsys.Unlink(1, "source"); errno != 0 {
		t.Fatalf("unlink open node: errno %d", errno)
	}
	if fsys.node(nodeID) == nil {
		t.Fatal("open node was discarded after unlink")
	}
	fsys.Release(nodeID, fh)
}

func TestMountedFSAddShareSnapshotsAndConflicts(t *testing.T) {
	rootBackend := imageBackend(t, map[string]string{"/root.txt": "root"})
	shareBackend := imageBackend(t, map[string]string{"/share.txt": "share"})
	otherBackend := imageBackend(t, map[string]string{"/share.txt": "other"})

	fsys := NewMountedFS(rootBackend, nil).(*mountedFS)
	if err := fsys.AddShare(ShareMount{
		GuestPath: "/mnt/share",
		Backend:   shareBackend,
		Writable:  true,
		CacheMode: fsCacheNormal,
	}); err != nil {
		t.Fatalf("add share: %v", err)
	}
	if err := fsys.AddShare(ShareMount{
		GuestPath: "mnt/share",
		Backend:   shareBackend,
		Writable:  true,
		CacheMode: fsCacheNormal,
	}); err != nil {
		t.Fatalf("re-add identical share: %v", err)
	}
	if err := fsys.AddShare(ShareMount{
		GuestPath: "/mnt/share",
		Backend:   otherBackend,
		Writable:  true,
		CacheMode: fsCacheNormal,
	}); err == nil {
		t.Fatalf("conflicting share error = %v", err)
	}

	rootSnap, err := fsys.RootSnapshot()
	if err != nil {
		t.Fatalf("root snapshot: %v", err)
	}
	if got := readSnapshotFile(t, rootSnap, "/root.txt"); got != "root" {
		t.Fatalf("root snapshot file = %q", got)
	}

	shareSnap, err := fsys.RootSnapshotAt("/mnt/share")
	if err != nil {
		t.Fatalf("share snapshot: %v", err)
	}
	if got := readSnapshotFile(t, shareSnap, "/share.txt"); got != "share" {
		t.Fatalf("share snapshot file = %q", got)
	}

	if _, err := fsys.RootSnapshotAt("/missing"); err == nil {
		t.Fatalf("missing mount snapshot error = %v", err)
	}
}

func TestMountedFSRejectsConflictingShareBatchWithoutMutation(t *testing.T) {
	root := imageBackend(t, nil)
	one := imageBackend(t, map[string]string{"/one": "one"})
	two := imageBackend(t, map[string]string{"/two": "two"})
	fsys := NewMountedFS(root, nil).(*mountedFS)
	err := fsys.AddShares([]ShareMount{
		{GuestPath: "/data", Backend: one},
		{GuestPath: "/data", Backend: two},
	})
	if err == nil {
		t.Fatal("conflicting share batch succeeded")
	}
	if len(fsys.mounts) != 0 {
		t.Fatalf("failed share batch left mounts: %+v", fsys.mounts)
	}
}

func TestMountedFSLookupRoutesSyntheticMountPaths(t *testing.T) {
	fsys := NewMountedFS(
		imageBackend(t, map[string]string{"/root.txt": "root"}),
		[]ShareMount{{
			GuestPath: "/mnt/share",
			Backend:   imageBackend(t, map[string]string{"/nested/file.txt": "share data"}),
			CacheMode: "unknown",
		}},
	).(*mountedFS)

	mntID, attr, errno := fsys.Lookup(1, "mnt")
	if errno != 0 {
		t.Fatalf("lookup synthetic /mnt errno = %d", errno)
	}
	if attr.Mode&uint32(dirTypeDir) == 0 && attr.Size != 0 {
		t.Fatalf("synthetic /mnt attr looks wrong: %+v", attr)
	}
	shareID, _, errno := fsys.Lookup(mntID, "share")
	if errno != 0 {
		t.Fatalf("lookup /mnt/share errno = %d", errno)
	}
	nestedID, _, errno := fsys.Lookup(shareID, "nested")
	if errno != 0 {
		t.Fatalf("lookup share dir errno = %d", errno)
	}
	fileID, _, errno := fsys.Lookup(nestedID, "file.txt")
	if errno != 0 {
		t.Fatalf("lookup share file errno = %d", errno)
	}
	fh, errno := fsys.Open(fileID, 0)
	if errno != 0 {
		t.Fatalf("open share file errno = %d", errno)
	}
	defer fsys.Release(fileID, fh)
	data, errno := fsys.Read(fileID, fh, 0, 128)
	if errno != 0 {
		t.Fatalf("read share file errno = %d", errno)
	}
	if string(data) != "share data" {
		t.Fatalf("share file data = %q", data)
	}

	if got := fsys.CachePolicy(shareID).Mode; got != fsCacheStrict {
		t.Fatalf("share cache mode = %q, want normalized strict mode", got)
	}
}

func TestMountedFSPreservesTrailingSpaceNames(t *testing.T) {
	root := imageBackend(t, nil)
	fsys := NewMountedFS(root, nil).(*mountedFS)
	var create fsCreateCallerBackend = fsys
	var write fsWriteCallerBackend = fsys

	created := make(map[string]uint64)
	for name, contents := range map[string]string{"collision": "A", "collision ": "B"} {
		nodeID, fh, _, errno := create.CreateForCaller(1, name, linuxOWRONLY|linuxOCREAT|linuxOEXCL, 0o644, 0, 0)
		if errno != 0 {
			t.Fatalf("create %q errno = %d", name, errno)
		}
		if _, errno := write.WriteForCaller(nodeID, fh, 0, []byte(contents), 0, 0, 0); errno != 0 {
			t.Fatalf("write %q errno = %d", name, errno)
		}
		fsys.Release(nodeID, fh)
		created[name] = nodeID
	}
	if created["collision"] == created["collision "] {
		t.Fatalf("byte-distinct names share node %d", created["collision"])
	}
	for name, want := range map[string]string{"collision": "A", "collision ": "B"} {
		nodeID, _, errno := fsys.Lookup(1, name)
		if errno != 0 {
			t.Fatalf("lookup %q errno = %d", name, errno)
		}
		fh, errno := fsys.Open(nodeID, linuxORDONLY)
		if errno != 0 {
			t.Fatalf("open %q errno = %d", name, errno)
		}
		data, errno := fsys.Read(nodeID, fh, 0, 16)
		fsys.Release(nodeID, fh)
		if errno != 0 || string(data) != want {
			t.Fatalf("read %q = %q, errno %d; want %q", name, data, errno, want)
		}
	}

	paths := fsys.SnapshotNodePaths()
	restored := NewMountedFS(root, nil).(*mountedFS)
	if err := restored.RestoreNodePaths(paths); err != nil {
		t.Fatalf("restore node paths: %v", err)
	}
	plainID, _, plainErrno := restored.Lookup(1, "collision")
	spacedID, _, spacedErrno := restored.Lookup(1, "collision ")
	if plainErrno != 0 || spacedErrno != 0 || plainID == spacedID {
		t.Fatalf("restored names = plain(%d,%d) spaced(%d,%d)", plainID, plainErrno, spacedID, spacedErrno)
	}
}

func TestMountedFSPassthroughHardlinkReportsSameInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows passthrough inode reporting does not expose stable hardlink inode identity")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	fsys := NewMountedFS(NewPassthroughFS(root, nil), nil).(*mountedFS)

	aID, aAttr, errno := fsys.Lookup(1, "a")
	if errno != 0 {
		t.Fatalf("lookup a errno = %d", errno)
	}
	bID, bAttr, errno := fsys.Link(aID, 1, "b")
	if errno != 0 {
		t.Fatalf("link errno = %d", errno)
	}
	aAttrAfter, errno := fsys.GetAttr(aID)
	if errno != 0 {
		t.Fatalf("getattr a errno = %d", errno)
	}
	bAttrAfter, errno := fsys.GetAttr(bID)
	if errno != 0 {
		t.Fatalf("getattr b errno = %d", errno)
	}
	if aAttr.Ino != bAttr.Ino || aAttrAfter.Ino != bAttrAfter.Ino {
		t.Fatalf("hardlink inodes before=(%d,%d) after=(%d,%d), want same", aAttr.Ino, bAttr.Ino, aAttrAfter.Ino, bAttrAfter.Ino)
	}
	if aAttrAfter.NLink < 2 || bAttrAfter.NLink < 2 {
		t.Fatalf("hardlink nlink a=%d b=%d, want at least 2", aAttrAfter.NLink, bAttrAfter.NLink)
	}
}

func TestImageFSHardlinkReportsSameInode(t *testing.T) {
	fsys := imageBackend(t, map[string]string{"/a": "data"})
	aID, aAttr, errno := fsys.Lookup(1, "a")
	if errno != 0 {
		t.Fatalf("lookup a errno = %d", errno)
	}
	bID, bAttr, errno := fsys.(fsLinkBackend).Link(aID, 1, "b")
	if errno != 0 {
		t.Fatalf("link errno = %d", errno)
	}
	aAttrAfter, errno := fsys.GetAttr(aID)
	if errno != 0 {
		t.Fatalf("getattr a errno = %d", errno)
	}
	bAttrAfter, errno := fsys.GetAttr(bID)
	if errno != 0 {
		t.Fatalf("getattr b errno = %d", errno)
	}
	if aAttr.Ino != bAttr.Ino || aAttrAfter.Ino != bAttrAfter.Ino {
		t.Fatalf("hardlink inodes before=(%d,%d) after=(%d,%d), want same", aAttr.Ino, bAttr.Ino, aAttrAfter.Ino, bAttrAfter.Ino)
	}
	if aID != bID {
		t.Fatalf("hardlink node IDs = %d and %d, want one FUSE inode", aID, bID)
	}
	if aAttrAfter.NLink != 2 || bAttrAfter.NLink != 2 {
		t.Fatalf("hardlink nlink a=%d b=%d, want 2", aAttrAfter.NLink, bAttrAfter.NLink)
	}
	if errno := fsys.(fsUnlinkBackend).Unlink(1, "a"); errno != 0 {
		t.Fatalf("unlink first hardlink errno = %d", errno)
	}
	remainingID, remainingAttr, errno := fsys.Lookup(1, "b")
	if errno != 0 || remainingID != bID || remainingAttr.NLink != 1 {
		t.Fatalf("remaining hardlink = node %d nlink %d errno %d, want node %d nlink 1", remainingID, remainingAttr.NLink, errno, bID)
	}
}

func TestMountedImageFSHardlinkUsesOneFUSENode(t *testing.T) {
	fsys := NewMountedFS(imageBackend(t, map[string]string{"/a": "data"}), nil).(*mountedFS)
	aID, _, errno := fsys.Lookup(1, "a")
	if errno != 0 {
		t.Fatalf("lookup a errno = %d", errno)
	}
	bID, linked, errno := fsys.Link(aID, 1, "b")
	if errno != 0 {
		t.Fatalf("link b errno = %d", errno)
	}
	if bID != aID || linked.NLink != 2 {
		t.Fatalf("linked alias = node %d nlink %d, want node %d nlink 2", bID, linked.NLink, aID)
	}
	lookedUpID, lookedUp, errno := fsys.Lookup(1, "b")
	if errno != 0 || lookedUpID != aID || lookedUp.NLink != 2 {
		t.Fatalf("lookup alias = node %d nlink %d errno %d, want node %d nlink 2", lookedUpID, lookedUp.NLink, errno, aID)
	}
	if errno := fsys.Unlink(1, "a"); errno != 0 {
		t.Fatalf("unlink a errno = %d", errno)
	}
	remainingID, remaining, errno := fsys.Lookup(1, "b")
	if errno != 0 || remainingID != aID || remaining.NLink != 1 {
		t.Fatalf("remaining alias = node %d nlink %d errno %d, want node %d nlink 1", remainingID, remaining.NLink, errno, aID)
	}
	cID, relinked, errno := fsys.Link(remainingID, 1, "c")
	if errno != 0 || cID != aID || relinked.NLink != 2 {
		t.Fatalf("relink remaining alias = node %d nlink %d errno %d, want node %d nlink 2", cID, relinked.NLink, errno, aID)
	}
	restored := NewMountedFS(fsys.root, nil).(*mountedFS)
	if err := restored.RestoreNodePaths(fsys.SnapshotNodePaths()); err != nil {
		t.Fatalf("restore hard-link node paths: %v", err)
	}
	bRestored, _, bErrno := restored.Lookup(1, "b")
	cRestored, _, cErrno := restored.Lookup(1, "c")
	if bErrno != 0 || cErrno != 0 || bRestored != cRestored {
		t.Fatalf("restored aliases = b(%d,%d) c(%d,%d), want one FUSE node", bRestored, bErrno, cRestored, cErrno)
	}
}

func TestMountedFSOpenDirectorySurvivesRemoval(t *testing.T) {
	root := imageBackend(t, nil)
	backendNodeID, _, errno := root.(fsMkdirBackend).Mkdir(1, "open-directory", 0o700, 0, 0)
	if errno != 0 {
		t.Fatalf("mkdir backend directory: errno %d", errno)
	}
	fsys := NewMountedFS(root, nil).(*mountedFS)
	nodeID, _, errno := fsys.Lookup(1, "open-directory")
	if errno != 0 {
		t.Fatalf("lookup mounted directory: errno %d", errno)
	}
	fh, errno := fsys.OpenDir(nodeID, 0)
	if errno != 0 {
		t.Fatalf("open mounted directory: errno %d", errno)
	}
	if errno := fsys.RmDir(1, "open-directory"); errno != 0 {
		t.Fatalf("remove mounted directory: errno %d", errno)
	}
	if fsys.node(nodeID) == nil {
		t.Fatal("mounted node was dropped while its directory handle was open")
	}
	if _, errno := root.GetAttr(backendNodeID); errno != 0 {
		t.Fatalf("backend node was dropped while its directory handle was open: errno %d", errno)
	}
	if _, errno := fsys.GetAttr(nodeID); errno != 0 {
		t.Fatalf("getattr open removed directory: errno %d", errno)
	}
	if _, errno := fsys.ReadDir(nodeID, fh, 0, 4096); errno != 0 {
		t.Fatalf("readdir open removed directory: errno %d", errno)
	}
	fsys.ReleaseDir(nodeID, fh)
	if _, errno := fsys.GetAttr(nodeID); errno != -linuxENOENT {
		t.Fatalf("getattr released removed directory: errno %d, want %d", errno, -linuxENOENT)
	}
}

func TestMountedFSDirectoryHandlePagesOneSnapshot(t *testing.T) {
	root := imageBackend(t, nil)
	for i := range 1000 {
		nodeID, fh, _, errno := root.(fsCreateBackend).Create(1, "entry-"+strconv.Itoa(i), linuxOWRONLY|linuxOCREAT, 0o644, 0, 0)
		if errno != 0 {
			t.Fatalf("create entry %d: errno %d", i, errno)
		}
		root.Release(nodeID, fh)
	}
	counting := &countingReadDirBackend{FSBackend: root}
	fsys := NewMountedFS(counting, nil).(*mountedFS)
	fh, errno := fsys.OpenDir(1, 0)
	if errno != 0 {
		t.Fatalf("open directory: errno %d", errno)
	}
	defer fsys.ReleaseDir(1, fh)
	offset := uint64(0)
	entries := 0
	for {
		page, errno := fsys.ReadDir(1, fh, offset, 128)
		if errno != 0 {
			t.Fatalf("read directory at offset %d: errno %d", offset, errno)
		}
		if len(page) == 0 {
			break
		}
		for len(page) != 0 {
			nameBytes := int(readLE32(page[16:20]))
			recordBytes := align8(fuseDirentBaseSize + nameBytes)
			offset = readLE64(page[8:16])
			entries++
			page = page[recordBytes:]
		}
	}
	if entries != 1002 {
		t.Fatalf("directory entries = %d, want 1002", entries)
	}
	if counting.reads != 2 {
		t.Fatalf("backend directory snapshot used %d reads, want one data page and one EOF read", counting.reads)
	}
}

func TestMountedFSOpenDirectorySurvivesRuntimeMountReplacement(t *testing.T) {
	root := imageBackend(t, nil)
	if _, _, errno := root.(fsMkdirBackend).Mkdir(1, "data", 0o700, 0, 0); errno != 0 {
		t.Fatalf("mkdir backend directory: errno %d", errno)
	}
	fsys := NewMountedFS(root, nil).(*mountedFS)
	nodeID, _, errno := fsys.Lookup(1, "data")
	if errno != 0 {
		t.Fatalf("lookup old directory: errno %d", errno)
	}
	fh, errno := fsys.OpenDir(nodeID, 0)
	if errno != 0 {
		t.Fatalf("open old directory: errno %d", errno)
	}
	if err := fsys.AddShare(ShareMount{GuestPath: "/data", Backend: imageBackend(t, map[string]string{"/new": "mounted"})}); err != nil {
		t.Fatal(err)
	}
	if _, errno := fsys.ReadDir(nodeID, fh, 0, 4096); errno != 0 {
		t.Fatalf("old open directory after mount replacement: errno %d", errno)
	}
	newID, _, errno := fsys.Lookup(1, "data")
	if errno != 0 || newID == nodeID {
		t.Fatalf("replacement lookup node=%d old=%d errno=%d", newID, nodeID, errno)
	}
	fsys.ReleaseDir(nodeID, fh)
}

func BenchmarkImageFSGentooLikeTinyFiles(b *testing.B) {
	const (
		fileCount = 1024
		fileSize  = 128
	)
	payload := make([]byte, fileSize)
	names := make([]string, fileCount)
	for i := range names {
		names[i] = "repo-file-" + strconv.Itoa(i)
	}

	b.ReportAllocs()
	b.SetBytes(fileCount * fileSize)
	b.ReportMetric(fileCount, "files/op")
	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		fsys := NewImageFS(imagefs.NewOverlay(nil).Root(), "")
		create := fsys.(fsCreateCallerBackend)
		write := fsys.(fsWriteCallerBackend)

		for _, name := range names {
			nodeID, fh, _, errno := create.CreateForCaller(1, name, linuxOWRONLY|linuxOCREAT|linuxOEXCL, 0o644, 0, 0)
			if errno != 0 {
				b.Fatalf("create %s errno = %d", name, errno)
			}
			if _, errno := write.WriteForCaller(nodeID, fh, 0, payload, 0, 0, 0); errno != 0 {
				b.Fatalf("write %s errno = %d", name, errno)
			}
			fsys.Release(nodeID, fh)
		}
	}
}

func BenchmarkImageFSSameBytesSingleFile(b *testing.B) {
	const (
		fileCount = 1024
		fileSize  = 128
	)
	payload := make([]byte, fileCount*fileSize)

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ReportMetric(1, "files/op")

	for n := 0; n < b.N; n++ {
		fsys := NewImageFS(imagefs.NewOverlay(nil).Root(), "")
		nodeID, fh, _, errno := fsys.(fsCreateCallerBackend).CreateForCaller(1, "repo-pack", linuxOWRONLY|linuxOCREAT|linuxOEXCL, 0o644, 0, 0)
		if errno != 0 {
			b.Fatalf("create repo-pack errno = %d", errno)
		}
		if _, errno := fsys.(fsWriteCallerBackend).WriteForCaller(nodeID, fh, 0, payload, 0, 0, 0); errno != 0 {
			b.Fatalf("write repo-pack errno = %d", errno)
		}
		fsys.Release(nodeID, fh)
	}
}

func imageBackend(t *testing.T, files map[string]string) FSBackend {
	t.Helper()
	overlay := imagefs.NewOverlay(nil)
	for guestPath, contents := range files {
		if err := overlay.AddFile(guestPath, 0o644, []byte(contents)); err != nil {
			t.Fatalf("add %s: %v", guestPath, err)
		}
	}
	return NewImageFS(overlay.Root(), "")
}

func readSnapshotFile(t *testing.T, root imagefs.Directory, guestPath string) string {
	t.Helper()
	entry, err := imagefs.LookupPath(root, guestPath)
	if err != nil {
		t.Fatalf("lookup snapshot %s: %v", guestPath, err)
	}
	if entry.File == nil {
		t.Fatalf("snapshot %s is not a file", guestPath)
	}
	size, _ := entry.File.Stat()
	data, err := entry.File.ReadAt(0, uint32(size))
	if err != nil {
		t.Fatalf("read snapshot %s: %v", guestPath, err)
	}
	return string(data)
}
