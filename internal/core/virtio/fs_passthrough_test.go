package virtio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
)

func TestPassthroughFSParentLookupAndReadDirUseParentNode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	fsys := NewPassthroughFS(root, nil)
	aID, _, errno := fsys.Lookup(1, "a")
	if errno != 0 {
		t.Fatalf("lookup a errno = %d", errno)
	}
	bID, _, errno := fsys.Lookup(aID, "b")
	if errno != 0 {
		t.Fatalf("lookup b errno = %d", errno)
	}
	parentID, _, errno := fsys.Lookup(bID, "..")
	if errno != 0 {
		t.Fatalf("lookup .. errno = %d", errno)
	}
	if parentID != aID {
		t.Fatalf("lookup .. node = %d, want %d", parentID, aID)
	}

	fh, errno := fsys.OpenDir(bID, 0)
	if errno != 0 {
		t.Fatalf("opendir b errno = %d", errno)
	}
	defer fsys.ReleaseDir(bID, fh)
	data, errno := fsys.ReadDir(bID, fh, 0, 4096)
	if errno != 0 {
		t.Fatalf("readdir b errno = %d", errno)
	}
	entries := parseTestFuseDirents(data)
	if got := entries["."]; got != bID {
		t.Fatalf("readdir . ino = %d, want %d", got, bID)
	}
	if got := entries[".."]; got != aID {
		t.Fatalf("readdir .. ino = %d, want %d", got, aID)
	}
}

func TestPassthroughFSReadDirRefreshesWhenEnumerationRestarts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "item.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	fsys := NewPassthroughFS(root, nil)
	renameFS, ok := fsys.(fsRenameBackend)
	if !ok {
		t.Fatal("passthrough filesystem does not support rename")
	}
	rootHandle, errno := fsys.OpenDir(1, 0)
	if errno != 0 {
		t.Fatalf("opendir root errno = %d", errno)
	}
	defer fsys.ReleaseDir(1, rootHandle)

	entries := readTestPassthroughDir(t, fsys, 1, rootHandle)
	if _, ok := entries["item.txt"]; !ok {
		t.Fatalf("initial directory entries = %v", entries)
	}

	folderID, _, errno := fsys.Lookup(1, "folder")
	if errno != 0 {
		t.Fatalf("lookup folder errno = %d", errno)
	}
	if errno := renameFS.Rename(1, "item.txt", folderID, "item.txt", 0); errno != 0 {
		t.Fatalf("move into folder errno = %d", errno)
	}
	entries = readTestPassthroughDir(t, fsys, 1, rootHandle)
	if _, ok := entries["item.txt"]; ok {
		t.Fatalf("entries after move still contain item.txt: %v", entries)
	}

	if errno := renameFS.Rename(folderID, "item.txt", 1, "item.txt", 0); errno != 0 {
		t.Fatalf("move back to root errno = %d", errno)
	}
	entries = readTestPassthroughDir(t, fsys, 1, rootHandle)
	if _, ok := entries["item.txt"]; !ok {
		t.Fatalf("entries after move back = %v", entries)
	}
}

func TestMountedPassthroughReadDirRefreshesWhenEnumerationRestarts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "item.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	fsys := NewMountedFS(imageBackend(t, nil), []ShareMount{{
		GuestPath: "/shared",
		Backend:   NewPassthroughFS(root, nil),
		Writable:  true,
		CacheMode: fsCacheStrict,
	}}).(*mountedFS)
	sharedID, _, errno := fsys.Lookup(1, "shared")
	if errno != 0 {
		t.Fatalf("lookup share errno = %d", errno)
	}
	folderID, _, errno := fsys.Lookup(sharedID, "folder")
	if errno != 0 {
		t.Fatalf("lookup folder errno = %d", errno)
	}
	handle, errno := fsys.OpenDir(sharedID, 0)
	if errno != 0 {
		t.Fatalf("opendir share errno = %d", errno)
	}
	defer fsys.ReleaseDir(sharedID, handle)

	entries := readTestPassthroughDir(t, fsys, sharedID, handle)
	if _, ok := entries["item.txt"]; !ok {
		t.Fatalf("initial mounted entries = %v", entries)
	}
	if errno := fsys.Rename(sharedID, "item.txt", folderID, "item.txt", 0); errno != 0 {
		t.Fatalf("mounted move into folder errno = %d", errno)
	}
	entries = readTestPassthroughDir(t, fsys, sharedID, handle)
	if _, ok := entries["item.txt"]; ok {
		t.Fatalf("mounted entries after move still contain item.txt: %v", entries)
	}
	if errno := fsys.Rename(folderID, "item.txt", sharedID, "item.txt", 0); errno != 0 {
		t.Fatalf("mounted move back errno = %d", errno)
	}
	entries = readTestPassthroughDir(t, fsys, sharedID, handle)
	if _, ok := entries["item.txt"]; !ok {
		t.Fatalf("mounted entries after move back = %v", entries)
	}
}

func TestPassthroughFSRenamePreservesCachedDescendantIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "source", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source", "nested", "item.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]fsmeta.Entry{
		"/source":                 {UID: 1000, GID: 1000},
		"/source/nested/item.txt": {UID: 1000, GID: 1000},
	}
	fsys := NewPassthroughFS(root, meta)
	renameFS := fsys.(fsRenameBackend)
	sourceID := lookupTestPassthroughNode(t, fsys, 1, "source")
	nestedID := lookupTestPassthroughNode(t, fsys, sourceID, "nested")
	itemID := lookupTestPassthroughNode(t, fsys, nestedID, "item.txt")

	if errno := renameFS.Rename(1, "source", 1, "moved", 0); errno != 0 {
		t.Fatalf("rename directory errno = %d", errno)
	}
	if got := readTestPassthroughFile(t, fsys, itemID); got != "original" {
		t.Fatalf("cached descendant after move = %q", got)
	}
	if _, ok := meta["/source/nested/item.txt"]; ok {
		t.Fatalf("metadata retained old descendant path: %v", meta)
	}
	if _, ok := meta["/moved/nested/item.txt"]; !ok {
		t.Fatalf("metadata did not follow moved descendant: %v", meta)
	}
	if err := os.MkdirAll(filepath.Join(root, "source", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source", "nested", "item.txt"), []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readTestPassthroughFile(t, fsys, itemID); got != "original" {
		t.Fatalf("cached descendant redirected into reused source path: %q", got)
	}
}

func TestPassthroughFSRenameDetachesOverwrittenTargetNode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := NewPassthroughFS(root, nil)
	renameFS := fsys.(fsRenameBackend)
	sourceID := lookupTestPassthroughNode(t, fsys, 1, "source.txt")
	targetID := lookupTestPassthroughNode(t, fsys, 1, "target.txt")
	if errno := renameFS.Rename(1, "source.txt", 1, "target.txt", 0); errno != 0 {
		t.Fatalf("overwrite target errno = %d", errno)
	}
	if _, errno := fsys.GetAttr(targetID); errno != -linuxENOENT {
		t.Fatalf("overwritten target node getattr errno = %d, want %d", errno, -linuxENOENT)
	}
	if got := readTestPassthroughFile(t, fsys, sourceID); got != "source" {
		t.Fatalf("renamed source data = %q", got)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errno := fsys.GetAttr(targetID); errno != -linuxENOENT {
		t.Fatalf("overwritten target node aliased a replacement: errno %d", errno)
	}
}

func TestPassthroughFSRenameNoReplacePreservesBothFiles(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "source.txt")
	newPath := filepath.Join(root, "target.txt")
	if err := os.WriteFile(oldPath, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := NewPassthroughFS(root, nil)
	if errno := fsys.(fsRenameBackend).Rename(1, "source.txt", 1, "target.txt", linuxRenameNoReplace); errno != -linuxEEXIST {
		t.Fatalf("noreplace errno = %d, want %d", errno, -linuxEEXIST)
	}
	if data, err := os.ReadFile(oldPath); err != nil || string(data) != "source" {
		t.Fatalf("source after failed noreplace = %q, %v", data, err)
	}
	if data, err := os.ReadFile(newPath); err != nil || string(data) != "target" {
		t.Fatalf("target after failed noreplace = %q, %v", data, err)
	}
}

func readTestPassthroughDir(t *testing.T, fsys FSBackend, nodeID, handle uint64) map[string]uint64 {
	t.Helper()
	data, errno := fsys.ReadDir(nodeID, handle, 0, 4096)
	if errno != 0 {
		t.Fatalf("readdir errno = %d", errno)
	}
	return parseTestFuseDirents(data)
}

func lookupTestPassthroughNode(t *testing.T, fsys FSBackend, parent uint64, name string) uint64 {
	t.Helper()
	nodeID, _, errno := fsys.Lookup(parent, name)
	if errno != 0 {
		t.Fatalf("lookup %q errno = %d", name, errno)
	}
	return nodeID
}

func readTestPassthroughFile(t *testing.T, fsys FSBackend, nodeID uint64) string {
	t.Helper()
	handle, errno := fsys.Open(nodeID, linuxORDONLY)
	if errno != 0 {
		t.Fatalf("open node %d errno = %d", nodeID, errno)
	}
	defer fsys.Release(nodeID, handle)
	data, errno := fsys.Read(nodeID, handle, 0, 4096)
	if errno != 0 {
		t.Fatalf("read node %d errno = %d", nodeID, errno)
	}
	return string(data)
}

func parseTestFuseDirents(data []byte) map[string]uint64 {
	out := map[string]uint64{}
	for off := 0; off+fuseDirentBaseSize <= len(data); {
		ino := binary.LittleEndian.Uint64(data[off:])
		nameLen := int(binary.LittleEndian.Uint32(data[off+16:]))
		recLen := align8(fuseDirentBaseSize + nameLen)
		if off+recLen > len(data) || off+fuseDirentBaseSize+nameLen > len(data) {
			break
		}
		name := string(data[off+fuseDirentBaseSize : off+fuseDirentBaseSize+nameLen])
		out[name] = ino
		off += recLen
	}
	return out
}

func TestPassthroughOpenHandlesAllowRenameAndUnlink(t *testing.T) {
	root := t.TempDir()
	first := NewPassthroughFS(root, nil).(*passthroughFS)
	node, fh, _, errno := first.Create(1, "file", linuxORDWR, 0600, 1000, 1000)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer first.Release(node, fh)
	if _, errno := first.Write(node, fh, 0, []byte("original"), 0); errno != 0 {
		t.Fatal(errno)
	}
	second := NewPassthroughFS(root, nil).(*passthroughFS)
	alias, _, errno := second.Lookup(1, "file")
	if errno != 0 {
		t.Fatal(errno)
	}
	other, errno := second.Open(alias, linuxORDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer second.Release(alias, other)
	if errno := first.Rename(1, "file", 1, "renamed", 0); errno != 0 {
		t.Fatalf("rename open file: %d", errno)
	}
	if errno := first.Unlink(1, "renamed"); errno != 0 {
		t.Fatalf("unlink open file: %d", errno)
	}
	for _, handle := range []struct {
		fs       *passthroughFS
		node, fh uint64
	}{{first, node, fh}, {second, alias, other}} {
		data, errno := handle.fs.Read(handle.node, handle.fh, 0, 20)
		if errno != 0 || string(data) != "original" {
			t.Fatalf("open file data %q errno %d", data, errno)
		}
	}
}
