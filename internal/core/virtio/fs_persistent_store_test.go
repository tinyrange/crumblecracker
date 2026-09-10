package virtio

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

func TestPersistentImageFSRestoresCOWNamespaceAndSparseData(t *testing.T) {
	lower := imagefs.NewOverlay(nil)
	if err := lower.AddFile("/config", 0o644, []byte("base")); err != nil {
		t.Fatal(err)
	}
	if err := lower.AddFile("/removed", 0o644, []byte("lower")); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, lower.Root(), storeDir)

	configID := lookupPersistentTest(t, backend, 1, "config")
	configFH, errno := backend.Open(configID, linuxORDWR)
	if errno != 0 {
		t.Fatalf("open lower config: errno %d", errno)
	}
	if written, errno := backend.Write(configID, configFH, 1, []byte("X"), 0); errno != 0 || written != 1 {
		t.Fatalf("write lower config = %d, errno %d", written, errno)
	}
	backend.Release(configID, configFH)

	fileID, fileFH, _, errno := backend.Create(1, "result", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o640, 1000, 1000)
	if errno != 0 {
		t.Fatalf("create result: errno %d", errno)
	}
	const sparseOffset = 8 << 20
	if written, errno := backend.Write(fileID, fileFH, sparseOffset, []byte("tail"), 0); errno != 0 || written != 4 {
		t.Fatalf("write sparse result = %d, errno %d", written, errno)
	}
	if errno := backend.SetXattr(fileID, "user.test", []byte("value"), 0); errno != 0 {
		t.Fatalf("set result xattr: errno %d", errno)
	}
	if _, _, errno := backend.Link(fileID, 1, "result-link"); errno != 0 {
		t.Fatalf("link result: errno %d", errno)
	}
	backend.Release(fileID, fileFH)

	lookupPersistentTest(t, backend, 1, "removed")
	if errno := backend.Unlink(1, "removed"); errno != 0 {
		t.Fatalf("unlink lower entry: errno %d", errno)
	}
	if errno := backend.Fsync(fileID, 0, 0); errno != 0 {
		t.Fatalf("fsync persistent home: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close persistent home: %v", err)
	}

	dataInfo, err := os.Stat(backend.persistentDataPathForTest(fileID, storeDir))
	if err != nil {
		t.Fatalf("stat inode data: %v", err)
	}
	if dataInfo.Size() != sparseOffset+4 {
		t.Fatalf("inode data size = %d, want %d", dataInfo.Size(), sparseOffset+4)
	}

	backend = openPersistentImageFSTest(t, lower.Root(), storeDir)
	defer backend.Close()
	configID = lookupPersistentTest(t, backend, 1, "config")
	if got := readPersistentTest(t, backend, configID, 0, 4); string(got) != "bXse" {
		t.Fatalf("restored config = %q, want %q", got, "bXse")
	}
	fileID = lookupPersistentTest(t, backend, 1, "result")
	linkID := lookupPersistentTest(t, backend, 1, "result-link")
	if linkID != fileID {
		t.Fatalf("hard-link inode = %d, want %d", linkID, fileID)
	}
	if got := readPersistentTest(t, backend, fileID, sparseOffset, 4); string(got) != "tail" {
		t.Fatalf("restored sparse tail = %q, want %q", got, "tail")
	}
	if got, errno := backend.GetXattr(fileID, "user.test"); errno != 0 || !bytes.Equal(got, []byte("value")) {
		t.Fatalf("restored xattr = %q, errno %d", got, errno)
	}
	if _, _, errno := backend.Lookup(1, "removed"); errno != -linuxENOENT {
		t.Fatalf("restored lower whiteout lookup errno = %d, want %d", errno, -linuxENOENT)
	}

	entries, err := os.ReadDir(filepath.Join(storeDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "config" || entry.Name() == "result" || entry.Name() == "result-link" {
			t.Fatalf("guest filename leaked into data store: %q", entry.Name())
		}
	}
}

func TestPersistentImageFSRejectsConcurrentWriter(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	first := openPersistentImageFSTest(t, lower, storeDir)
	defer first.Close()
	_, err := NewPersistentImageFS(lower, PersistentImageFSOptions{StoreDir: storeDir, LowerID: "test"})
	if !errors.Is(err, ErrPersistentStoreInUse) {
		t.Fatalf("second writer error = %v, want ErrPersistentStoreInUse", err)
	}
}

func TestPersistentImageFSUntouchedLowerFilesFollowImageUpdate(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lowerV1 := imagefs.NewOverlay(nil)
	if err := lowerV1.AddFile("/image-setting", 0o644, []byte("version-one")); err != nil {
		t.Fatal(err)
	}
	fsys, err := NewPersistentImageFS(lowerV1.Root(), PersistentImageFSOptions{
		StoreDir: storeDir,
		Name:     "research",
		Mount:    "/home/scientist",
		LowerID:  "sha256:one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.(interface{ Close() error }).Close(); err != nil {
		t.Fatal(err)
	}

	lowerV2 := imagefs.NewOverlay(nil)
	if err := lowerV2.AddFile("/image-setting", 0o644, []byte("version-two")); err != nil {
		t.Fatal(err)
	}
	fsys, err = NewPersistentImageFS(lowerV2.Root(), PersistentImageFSOptions{
		StoreDir: storeDir,
		Name:     "research",
		Mount:    "/home/scientist",
		LowerID:  "sha256:two",
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := fsys.(*imageFS)
	defer backend.Close()
	nodeID := lookupPersistentTest(t, backend, 1, "image-setting")
	if got := readPersistentTest(t, backend, nodeID, 0, uint32(len("version-two"))); string(got) != "version-two" {
		t.Fatalf("updated lower file = %q", got)
	}
	statuses := backend.PersistentFSStatus()
	if len(statuses) != 1 || statuses[0].LowerID != "sha256:two" || statuses[0].PreviousLowerID != "sha256:one" {
		t.Fatalf("lower image transition status = %+v", statuses)
	}
}

func TestPersistentImageFSImageUpdateKeepsOnlyUpperChanges(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lowerV1 := imagefs.NewOverlay(nil)
	for name, value := range map[string]string{
		"/a-untouched": "untouched-v1",
		"/m-metadata":  "metadata-v1",
		"/z-copied":    "copied-v1",
	} {
		if err := lowerV1.AddFile(name, 0o644, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	backend := openPersistentImageFSTest(t, lowerV1.Root(), storeDir)
	untouchedID := lookupPersistentTest(t, backend, 1, "a-untouched")
	if got := readPersistentTest(t, backend, untouchedID, 0, 12); string(got) != "untouched-v1" {
		t.Fatalf("initial untouched data = %q", got)
	}
	metadataID := lookupPersistentTest(t, backend, 1, "m-metadata")
	if _, errno := backend.SetAttr(metadataID, fattrMode, 0, 0, 0o600, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("chmod metadata-only lower file: errno %d", errno)
	}
	if errno := backend.Fsync(metadataID, 0, 0); errno != 0 {
		t.Fatalf("fsync metadata-only lower file: errno %d", errno)
	}
	copiedID := lookupPersistentTest(t, backend, 1, "z-copied")
	copiedFH, errno := backend.Open(copiedID, linuxORDWR)
	if errno != 0 {
		t.Fatalf("open copied lower file: errno %d", errno)
	}
	if written, errno := backend.Write(copiedID, copiedFH, 0, []byte("USER"), 0); errno != 0 || written != 4 {
		t.Fatalf("write copied lower file = %d, errno %d", written, errno)
	}
	if errno := backend.Fsync(copiedID, copiedFH, 0); errno != 0 {
		t.Fatalf("fsync copied lower file: errno %d", errno)
	}
	backend.Release(copiedID, copiedFH)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	lowerV2 := imagefs.NewOverlay(nil)
	for name, value := range map[string]string{
		"/00-new":      "new-in-v2",
		"/a-untouched": "untouched-v2",
		"/m-metadata":  "metadata-v2",
		"/z-copied":    "replaced-v2",
	} {
		if err := lowerV2.AddFile(name, 0o644, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	backend = openPersistentImageFSTest(t, lowerV2.Root(), storeDir)
	defer backend.Close()
	newID := lookupPersistentTest(t, backend, 1, "00-new")
	if got := readPersistentTest(t, backend, newID, 0, 9); string(got) != "new-in-v2" {
		t.Fatalf("new image file = %q", got)
	}
	untouchedID = lookupPersistentTest(t, backend, 1, "a-untouched")
	if got := readPersistentTest(t, backend, untouchedID, 0, 12); string(got) != "untouched-v2" {
		t.Fatalf("updated untouched data = %q", got)
	}
	metadataID = lookupPersistentTest(t, backend, 1, "m-metadata")
	if got := readPersistentTest(t, backend, metadataID, 0, 11); string(got) != "metadata-v2" {
		t.Fatalf("metadata-only file did not follow image = %q", got)
	}
	if _, attr, errno := backend.Lookup(1, "m-metadata"); errno != 0 || attr.Mode&linuxPermMask != 0o600 {
		t.Fatalf("metadata-only mode = %#o, errno %d", attr.Mode&linuxPermMask, errno)
	}
	copiedID = lookupPersistentTest(t, backend, 1, "z-copied")
	if got := readPersistentTest(t, backend, copiedID, 0, 9); string(got) != "USERed-v1" {
		t.Fatalf("copied-up file followed image replacement = %q", got)
	}
}

func TestPersistentImageFSMissingMetadataOnlyLowerIsIsolated(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lowerV1 := imagefs.NewOverlay(nil)
	if err := lowerV1.AddFile("/missing-later", 0o644, []byte("lower")); err != nil {
		t.Fatal(err)
	}
	if err := lowerV1.AddFile("/healthy", 0o644, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	backend := openPersistentImageFSTest(t, lowerV1.Root(), storeDir)
	missingID := lookupPersistentTest(t, backend, 1, "missing-later")
	if _, errno := backend.SetAttr(missingID, fattrMode, 0, 0, 0o600, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("chmod lower file: errno %d", errno)
	}
	if errno := backend.Fsync(missingID, 0, 0); errno != 0 {
		t.Fatalf("fsync lower metadata: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	lowerV2 := imagefs.NewOverlay(nil)
	if err := lowerV2.AddFile("/healthy", 0o644, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	backend = openPersistentImageFSTest(t, lowerV2.Root(), storeDir)
	defer backend.Close()
	healthyID := lookupPersistentTest(t, backend, 1, "healthy")
	if got := readPersistentTest(t, backend, healthyID, 0, 2); string(got) != "v2" {
		t.Fatalf("healthy lower file = %q", got)
	}
	missingID = lookupPersistentTest(t, backend, 1, "missing-later")
	fh, errno := backend.Open(missingID, linuxORDONLY)
	if errno != 0 {
		t.Fatalf("open missing lower metadata inode: errno %d", errno)
	}
	if _, errno := backend.Read(missingID, fh, 0, 5); errno != -linuxEIO {
		t.Fatalf("read missing lower metadata inode: errno %d", errno)
	}
	backend.Release(missingID, fh)
	statuses := backend.PersistentFSStatus()
	if len(statuses) != 1 || statuses[0].LastError == "" {
		t.Fatalf("missing lower status = %+v", statuses)
	}
}

func TestPersistentImageFSRenamedLowerDirectorySurvivesImageRemoval(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil)
	if err := lower.AddDir("/profile", fs.ModeDir|0o700); err != nil {
		t.Fatal(err)
	}
	if err := lower.AddDir("/profile/nested", fs.ModeDir|0o700); err != nil {
		t.Fatal(err)
	}
	if err := lower.AddFile("/profile/nested/settings", 0o600, []byte("durable-settings")); err != nil {
		t.Fatal(err)
	}
	backend := openPersistentImageFSTest(t, lower.Root(), storeDir)
	lookupPersistentTest(t, backend, 1, "profile")
	if errno := backend.Rename(1, "profile", 1, "renamed-profile", 0); errno != 0 {
		t.Fatalf("rename lower directory: errno %d", errno)
	}
	if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
		t.Fatalf("fsync renamed directory parent: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	defer backend.Close()
	renamedID := lookupPersistentTest(t, backend, 1, "renamed-profile")
	nestedID := lookupPersistentTest(t, backend, renamedID, "nested")
	settingsID := lookupPersistentTest(t, backend, nestedID, "settings")
	if got := readPersistentTest(t, backend, settingsID, 0, uint32(len("durable-settings"))); string(got) != "durable-settings" {
		t.Fatalf("renamed lower directory data = %q", got)
	}
	if _, _, errno := backend.Lookup(1, "profile"); errno != -linuxENOENT {
		t.Fatalf("old lower directory reappeared: errno %d", errno)
	}
}

func TestPersistentImageFSTruncateAndGrowDoesNotRevealDiscardedBytes(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	nodeID, fh, _, errno := backend.Create(1, "state", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create state: errno %d", errno)
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("secret"), 0); errno != 0 || written != 6 {
		t.Fatalf("write state = %d, errno %d", written, errno)
	}
	if _, errno := backend.SetAttr(nodeID, fattrSize, 0, 2, 0, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("shrink state: errno %d", errno)
	}
	if _, errno := backend.SetAttr(nodeID, fattrSize, 0, 6, 0, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatalf("grow state: errno %d", errno)
	}
	if got := readPersistentTest(t, backend, nodeID, 0, 6); !bytes.Equal(got, []byte{'s', 'e', 0, 0, 0, 0}) {
		t.Fatalf("grown state exposed discarded bytes: %q", got)
	}
	if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
		t.Fatalf("fsync state: errno %d", errno)
	}
	backend.Release(nodeID, fh)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, lower, storeDir)
	defer backend.Close()
	nodeID = lookupPersistentTest(t, backend, 1, "state")
	if got := readPersistentTest(t, backend, nodeID, 0, 6); !bytes.Equal(got, []byte{'s', 'e', 0, 0, 0, 0}) {
		t.Fatalf("reopened state exposed discarded bytes: %q", got)
	}
}

func TestPersistentImageFSPreservesCaseAndTrailingSpaceNames(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	names := []string{"sample", "sample ", "Sample", string([]byte{'n', 'a', 'm', 'e', 0xff})}
	for i, name := range names {
		nodeID, fh, _, errno := backend.Create(1, name, linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
		if errno != 0 {
			t.Fatalf("create %q: errno %d", name, errno)
		}
		payload := []byte{byte('0' + i)}
		if written, errno := backend.Write(nodeID, fh, 0, payload, 0); errno != 0 || written != 1 {
			t.Fatalf("write %q = %d, errno %d", name, written, errno)
		}
		backend.Release(nodeID, fh)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend = openPersistentImageFSTest(t, lower, storeDir)
	defer backend.Close()
	ids := make(map[uint64]struct{}, len(names))
	for i, name := range names {
		nodeID := lookupPersistentTest(t, backend, 1, name)
		ids[nodeID] = struct{}{}
		if got := readPersistentTest(t, backend, nodeID, 0, 1); !bytes.Equal(got, []byte{byte('0' + i)}) {
			t.Fatalf("contents of %q = %q", name, got)
		}
	}
	if len(ids) != len(names) {
		t.Fatalf("distinct names resolved to %d inodes, want %d", len(ids), len(names))
	}
}

func TestPersistentImageFSRenameVariantsSurviveReopen(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	ids := make(map[string]uint64)
	for _, item := range []struct {
		name, contents string
	}{{"left", "L"}, {"right", "R"}} {
		nodeID, fh, _, errno := backend.Create(1, item.name, linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
		if errno != 0 {
			t.Fatalf("create %s: errno %d", item.name, errno)
		}
		if written, errno := backend.Write(nodeID, fh, 0, []byte(item.contents), 0); errno != 0 || written != 1 {
			t.Fatalf("write %s = %d, errno %d", item.name, written, errno)
		}
		backend.Release(nodeID, fh)
		ids[item.name] = nodeID
	}
	if errno := backend.Rename(1, "left", 1, "right", linuxRenameExchange); errno != 0 {
		t.Fatalf("exchange left and right: errno %d", errno)
	}
	if errno := backend.Rename(1, "right", 1, "final", linuxRenameNoReplace); errno != 0 {
		t.Fatalf("noreplace into absent destination: errno %d", errno)
	}
	if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
		t.Fatalf("fsync renamed directory: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, lower, storeDir)
	defer backend.Close()
	leftID := lookupPersistentTest(t, backend, 1, "left")
	finalID := lookupPersistentTest(t, backend, 1, "final")
	if leftID != ids["right"] || finalID != ids["left"] {
		t.Fatalf("renamed inode identities = left:%d final:%d, want %d/%d", leftID, finalID, ids["right"], ids["left"])
	}
	if got := readPersistentTest(t, backend, leftID, 0, 1); string(got) != "R" {
		t.Fatalf("left contents = %q", got)
	}
	if got := readPersistentTest(t, backend, finalID, 0, 1); string(got) != "L" {
		t.Fatalf("final contents = %q", got)
	}
}

func TestPersistentImageFSReopenAfterEveryMutationSequence(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	model := make(map[string]string)
	deleted := make(map[string]struct{})
	for step := 0; step < 80; step++ {
		names := sortedPersistentTestNames(model)
		switch {
		case len(names) < 4 || step%4 == 0 && len(names) < 12:
			name := fmt.Sprintf("file-%03d ", step)
			value := fmt.Sprintf("created-%03d", step)
			nodeID, fh, _, errno := backend.Create(1, name, linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 1000, 1000)
			if errno != 0 {
				t.Fatalf("step %d create %q: errno %d", step, name, errno)
			}
			if written, errno := backend.Write(nodeID, fh, 0, []byte(value), 0); errno != 0 || written != uint32(len(value)) {
				t.Fatalf("step %d write new %q = %d, errno %d", step, name, written, errno)
			}
			if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
				t.Fatalf("step %d fsync new %q: errno %d", step, name, errno)
			}
			backend.Release(nodeID, fh)
			model[name] = value
		case step%4 == 1:
			name := names[step%len(names)]
			value := fmt.Sprintf("updated-%03d", step)
			nodeID := lookupPersistentTest(t, backend, 1, name)
			fh, errno := backend.Open(nodeID, linuxORDWR|linuxOTRUNC)
			if errno != 0 {
				t.Fatalf("step %d open %q for update: errno %d", step, name, errno)
			}
			if written, errno := backend.Write(nodeID, fh, 0, []byte(value), 0); errno != 0 || written != uint32(len(value)) {
				t.Fatalf("step %d update %q = %d, errno %d", step, name, written, errno)
			}
			if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
				t.Fatalf("step %d fsync %q: errno %d", step, name, errno)
			}
			backend.Release(nodeID, fh)
			model[name] = value
		case step%4 == 2:
			oldName := names[step%len(names)]
			newName := fmt.Sprintf("renamed-%03d", step)
			if errno := backend.Rename(1, oldName, 1, newName, linuxRenameNoReplace); errno != 0 {
				t.Fatalf("step %d rename %q: errno %d", step, oldName, errno)
			}
			model[newName] = model[oldName]
			delete(model, oldName)
			deleted[oldName] = struct{}{}
		default:
			name := names[step%len(names)]
			if errno := backend.Unlink(1, name); errno != 0 {
				t.Fatalf("step %d unlink %q: errno %d", step, name, errno)
			}
			delete(model, name)
			deleted[name] = struct{}{}
		}

		if err := backend.Close(); err != nil {
			t.Fatalf("step %d close: %v", step, err)
		}
		backend = openPersistentImageFSTest(t, lower, storeDir)
		for name, want := range model {
			nodeID := lookupPersistentTest(t, backend, 1, name)
			if got := readPersistentTest(t, backend, nodeID, 0, uint32(len(want))); string(got) != want {
				t.Fatalf("step %d contents %q = %q, want %q", step, name, got, want)
			}
		}
		for name := range deleted {
			if _, stillPresent := model[name]; stillPresent {
				continue
			}
			if _, _, errno := backend.Lookup(1, name); errno != -linuxENOENT {
				t.Fatalf("step %d deleted %q lookup errno = %d", step, name, errno)
			}
		}
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func sortedPersistentTestNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestPersistentImageFSOpenUnlinkedFileSurvivesUntilRelease(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	nodeID, fh, _, errno := backend.Create(1, "temporary", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create temporary: errno %d", errno)
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("still-open"), 0); errno != 0 || written != 10 {
		t.Fatalf("write temporary = %d, errno %d", written, errno)
	}
	if errno := backend.Unlink(1, "temporary"); errno != 0 {
		t.Fatalf("unlink temporary: errno %d", errno)
	}
	if got := readPersistentHandleTest(t, backend, nodeID, fh, 0, 10); string(got) != "still-open" {
		t.Fatalf("open-unlinked contents = %q", got)
	}
	if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
		t.Fatalf("fsync parent: errno %d", errno)
	}
	backend.Release(nodeID, fh)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	defer backend.Close()
	if _, _, errno := backend.Lookup(1, "temporary"); errno != -linuxENOENT {
		t.Fatalf("unlinked file returned after reopen: errno %d", errno)
	}
}

func TestPersistentImageFSCloseOmitsOpenReplacedFile(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	replacedID, replacedFH, _, errno := backend.Create(1, "state", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create state: errno %d", errno)
	}
	if written, errno := backend.Write(replacedID, replacedFH, 0, []byte("old"), 0); errno != 0 || written != 3 {
		t.Fatalf("write old state = %d, errno %d", written, errno)
	}
	replacementID, replacementFH, _, errno := backend.Create(1, "state.tmp", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create replacement: errno %d", errno)
	}
	if written, errno := backend.Write(replacementID, replacementFH, 0, []byte("new"), 0); errno != 0 || written != 3 {
		t.Fatalf("write replacement = %d, errno %d", written, errno)
	}
	backend.Release(replacementID, replacementFH)
	if errno := backend.Rename(1, "state.tmp", 1, "state", 0); errno != 0 {
		t.Fatalf("replace state: errno %d", errno)
	}
	if got := readPersistentHandleTest(t, backend, replacedID, replacedFH, 0, 3); string(got) != "old" {
		t.Fatalf("open replaced contents = %q", got)
	}
	// Deliberately close with the replaced inode still open. A clean checkpoint
	// must preserve the namespace, not a handle that cannot survive restart.
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	defer backend.Close()
	nodeID, _, errno := backend.Lookup(1, "state")
	if errno != 0 {
		t.Fatalf("lookup replacement: errno %d", errno)
	}
	if got := readPersistentTest(t, backend, nodeID, 0, 3); string(got) != "new" {
		t.Fatalf("replacement contents = %q", got)
	}
}

func TestPersistentImageFSRestoreRepairsDanglingDirectoryEntry(t *testing.T) {
	storeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(storeDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := newImageFS(imagefs.NewOverlay(nil).Root(), storeDir, 0, 0, false).(*imageFS)
	backend.persistent = &persistentImageStore{
		dir:           storeDir,
		dataUsage:     make(map[uint64]uint64),
		dataPhysical:  make(map[uint64]uint64),
		pendingTrim:   make(map[uint64]uint64),
		pendingRemove: make(map[uint64]struct{}),
	}
	backend.persistentNodes = make(map[uint64]struct{})
	err := backend.restorePersistentState(&persistentImageState{
		NextNodeID: 3,
		Nodes: []persistentImageNode{{
			ID:          1,
			Parent:      1,
			Name:        []byte("/"),
			Kind:        "dir",
			Mode:        uint32(fs.ModeDir | 0o755),
			NLink:       1,
			EntriesDone: true,
			Entries: []persistentImageDirent{{
				Name:  []byte("missing"),
				Inode: 2,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("restore dangling checkpoint: %v", err)
	}
	root := backend.nodes[1]
	if _, exists := root.entries["missing"]; exists {
		t.Fatal("dangling directory entry survived recovery")
	}
	if !root.whiteouts["missing"] {
		t.Fatal("recovered name was not hidden from the lower filesystem")
	}
	if backend.persistent.recovery.Status != "recovered-dangling-directory-entry" {
		t.Fatalf("recovery status = %q", backend.persistent.recovery.Status)
	}
}

func TestPersistentImageFSRestoreDiscardsOrphanedDirectoryTree(t *testing.T) {
	storeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(storeDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := newImageFS(imagefs.NewOverlay(nil).Root(), storeDir, 0, 0, false).(*imageFS)
	backend.persistent = &persistentImageStore{
		dir:           storeDir,
		dataUsage:     make(map[uint64]uint64),
		dataPhysical:  make(map[uint64]uint64),
		pendingTrim:   make(map[uint64]uint64),
		pendingRemove: make(map[uint64]struct{}),
	}
	backend.persistentNodes = make(map[uint64]struct{})
	directory := func(id, parent uint64, name, guestPath string, entries ...persistentImageDirent) persistentImageNode {
		return persistentImageNode{
			ID: id, Parent: parent, Name: []byte(name), GuestPath: []byte(guestPath),
			Kind: "dir", Mode: uint32(fs.ModeDir | 0o755), NLink: 1, EntriesDone: true, Entries: entries,
		}
	}
	file := func(id, parent uint64, name, guestPath string) persistentImageNode {
		return persistentImageNode{
			ID: id, Parent: parent, Name: []byte(name), GuestPath: []byte(guestPath),
			Kind: "file", Mode: 0o600, NLink: 1,
		}
	}
	err := backend.restorePersistentState(&persistentImageState{
		NextNodeID: 6,
		Nodes: []persistentImageNode{
			directory(1, 1, "/", "/", persistentImageDirent{Name: []byte("tree"), Inode: 2}),
			directory(2, 1, "tree", "/tree", persistentImageDirent{Name: []byte("value"), Inode: 3}),
			file(3, 2, "value", "/tree/value"),
			directory(4, 1, "tree", "/tree", persistentImageDirent{Name: []byte("value"), Inode: 5}),
			file(5, 4, "value", "/tree/value"),
		},
	})
	if err != nil {
		t.Fatalf("restore checkpoint with orphaned tree: %v", err)
	}
	if _, exists := backend.nodes[4]; exists {
		t.Fatal("orphaned directory survived recovery")
	}
	if _, exists := backend.nodes[5]; exists {
		t.Fatal("orphaned file survived recovery")
	}
	if nodeID, _, errno := backend.Lookup(2, "value"); errno != 0 || nodeID != 3 {
		t.Fatalf("lookup reachable file = inode %d, errno %d", nodeID, errno)
	}
}

func TestPersistentImageFSTornWALTailIsPreservedAndRemovedFromReplay(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	if _, _, _, errno := backend.Create(1, "safe", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0); errno != 0 {
		t.Fatalf("create safe: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(storeDir, "metadata.wal")
	file, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("torn-journal-tail")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, lower, storeDir)
	lookupPersistentTest(t, backend, 1, "safe")
	if backend.persistent.recovery.DiscardedBytes != uint64(len("torn-journal-tail")) {
		t.Fatalf("discarded WAL bytes = %d", backend.persistent.recovery.DiscardedBytes)
	}
	if _, err := os.Stat(backend.persistent.recovery.QuarantinePath); err != nil {
		t.Fatalf("stat quarantined WAL tail: %v", err)
	}
	if info, err := os.Stat(walPath); err != nil || info.Size() != 0 {
		t.Fatalf("recovered WAL size = %v, err %v", info, err)
	}
	if _, _, _, errno := backend.Create(1, "after-recovery", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0); errno != 0 {
		t.Fatalf("create after recovery: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend = openPersistentImageFSTest(t, lower, storeDir)
	defer backend.Close()
	lookupPersistentTest(t, backend, 1, "after-recovery")
	if statuses := backend.PersistentFSStatus(); len(statuses) != 1 || statuses[0].RecoveryStatus != "" || statuses[0].DiscardedBytes != 0 {
		t.Fatalf("completed recovery repeated on next attachment: %+v", statuses)
	}
}

func TestPersistentImageFSCheckpointsGrowingWALWithoutRewritingData(t *testing.T) {
	previousThreshold := persistentImageCheckpointWALBytes
	persistentImageCheckpointWALBytes = 1
	t.Cleanup(func() { persistentImageCheckpointWALBytes = previousThreshold })
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	nodeID, fh, _, errno := backend.Create(1, "state", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create state: errno %d", errno)
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("stable-data"), 0); errno != 0 || written != 11 {
		t.Fatalf("write state = %d, errno %d", written, errno)
	}
	dataPath := backend.persistent.dataPath(nodeID)
	before, err := os.Stat(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
		t.Fatalf("fsync state: errno %d", errno)
	}
	wal, err := os.Stat(backend.persistent.walPath())
	if err != nil {
		t.Fatal(err)
	}
	if wal.Size() != 0 {
		t.Fatalf("WAL size after checkpoint = %d", wal.Size())
	}
	after, err := os.Stat(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() {
		t.Fatal("metadata checkpoint replaced or rewrote inode data")
	}
	backend.Release(nodeID, fh)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentImageFSCopiesLowerOnceAndDoesNotVersionRepeatedWrites(t *testing.T) {
	lower := imagefs.NewOverlay(nil)
	const lowerSize = 16 << 20
	if err := lower.AddFile("/large", 0o600, bytes.Repeat([]byte{0x7f}, lowerSize)); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, lower.Root(), storeDir)
	nodeID := lookupPersistentTest(t, backend, 1, "large")
	fh, errno := backend.Open(nodeID, linuxORDWR)
	if errno != 0 {
		t.Fatalf("open large lower file: errno %d", errno)
	}
	const offset = 8<<20 + 17
	var firstPhysical uint64
	var err error
	for i := 0; i < 64; i++ {
		if written, errno := backend.Write(nodeID, fh, offset, []byte{byte(i)}, 0); errno != 0 || written != 1 {
			t.Fatalf("overwrite %d = %d, errno %d", i, written, errno)
		}
		if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
			t.Fatalf("fsync overwrite %d: errno %d", i, errno)
		}
		if i == 0 {
			_, _, firstPhysical, err = backend.BackingUsage()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	current, _, physical, err := backend.BackingUsage()
	if err != nil {
		t.Fatal(err)
	}
	if current != lowerSize {
		t.Fatalf("upper allocated bytes = %d, want copied lower size %d", current, lowerSize)
	}
	if physical < lowerSize || physical > lowerSize+(1<<20) {
		t.Fatalf("physical bytes for 64 one-byte overwrites = %d", physical)
	}
	if physical != firstPhysical {
		t.Fatalf("repeated overwrites grew physical storage from %d to %d", firstPhysical, physical)
	}
	dataFiles := 0
	err = filepath.WalkDir(filepath.Join(storeDir, "data"), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			dataFiles++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dataFiles != 1 {
		t.Fatalf("inode data files = %d, want 1", dataFiles)
	}
	backend.Release(nodeID, fh)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentImageFSCopiedLowerSurvivesImageRemoval(t *testing.T) {
	const size = 3*imageDataPageSize + 17
	original := bytes.Repeat([]byte("lower-data-"), int(size)/len("lower-data-")+1)
	original = original[:size]
	lower := imagefs.NewOverlay(nil)
	if err := lower.AddFile("/settings", 0o600, original); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, lower.Root(), storeDir)
	nodeID := lookupPersistentTest(t, backend, 1, "settings")
	fh, errno := backend.Open(nodeID, linuxORDWR)
	if errno != 0 {
		t.Fatalf("open settings: errno %d", errno)
	}
	const changedOffset = imageDataPageSize + 19
	if written, errno := backend.Write(nodeID, fh, changedOffset, []byte("USER"), 0); errno != 0 || written != 4 {
		t.Fatalf("write settings = %d, errno %d", written, errno)
	}
	if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
		t.Fatalf("fsync settings: errno %d", errno)
	}
	backend.Release(nodeID, fh)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	expected := append([]byte(nil), original...)
	copy(expected[changedOffset:], "USER")
	emptyLower := imagefs.NewOverlay(nil).Root()
	backend = openPersistentImageFSTest(t, emptyLower, storeDir)
	nodeID = lookupPersistentTest(t, backend, 1, "settings")
	fh, errno = backend.Open(nodeID, linuxORDONLY)
	if errno != 0 {
		t.Fatalf("open copied settings after image removal: errno %d", errno)
	}
	got, errno := backend.Read(nodeID, fh, 0, uint32(len(expected)))
	if errno != 0 {
		t.Fatalf("read copied settings after image removal: errno %d", errno)
	}
	if !bytes.Equal(got, expected) {
		t.Fatal("copied lower file changed when the image file disappeared")
	}
	backend.Release(nodeID, fh)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentImageFSMissingMetadataNeverCreatesEmptyHome(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	if _, _, _, errno := backend.Create(1, "important", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0); errno != 0 {
		t.Fatalf("create important: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(storeDir, "metadata.0"))
	_ = os.Remove(filepath.Join(storeDir, "metadata.1"))
	if _, err := NewPersistentImageFS(lower, PersistentImageFSOptions{StoreDir: storeDir, LowerID: "test"}); err == nil {
		t.Fatal("store with missing metadata reopened as an empty filesystem")
	}
}

func TestPersistentImageFSMissingUpperDataReturnsIOError(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	nodeID, fh, _, errno := backend.Create(1, "important", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create important: errno %d", errno)
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("payload"), 0); errno != 0 || written != 7 {
		t.Fatalf("write important = %d, errno %d", written, errno)
	}
	if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
		t.Fatalf("fsync important: errno %d", errno)
	}
	backend.Release(nodeID, fh)
	dataPath := backend.persistent.dataPath(nodeID)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, lower, storeDir)
	defer backend.Close()
	nodeID = lookupPersistentTest(t, backend, 1, "important")
	fh, errno = backend.Open(nodeID, linuxORDONLY)
	if errno != 0 {
		t.Fatalf("open missing-data inode: errno %d", errno)
	}
	defer backend.Release(nodeID, fh)
	if _, errno := backend.Read(nodeID, fh, 0, 7); errno != -linuxEIO {
		t.Fatalf("read missing-data inode: errno %d, want %d", errno, -linuxEIO)
	}
}

func TestPersistentImageFSENOSPCPreservesPendingDataAndMetadata(t *testing.T) {
	t.Cleanup(func() { persistentImageFaultTestHook = nil })
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	nodeID, fh, _, errno := backend.Create(1, "state", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create state: errno %d", errno)
	}
	persistentImageFaultTestHook = func(stage string) error {
		if stage == "before-data-write" {
			return syscall.ENOSPC
		}
		return nil
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("durable"), 0); errno != -linuxENOSPC || written != 0 {
		t.Fatalf("write under ENOSPC = %d, errno %d", written, errno)
	}
	persistentImageFaultTestHook = nil
	if written, errno := backend.Write(nodeID, fh, 0, []byte("durable"), 0); errno != 0 || written != 7 {
		t.Fatalf("retry write = %d, errno %d", written, errno)
	}
	persistentImageFaultTestHook = func(stage string) error {
		if stage == "before-wal-append" {
			return syscall.ENOSPC
		}
		return nil
	}
	if errno := backend.Fsync(nodeID, fh, 0); errno != -linuxENOSPC {
		t.Fatalf("fsync under ENOSPC: errno %d", errno)
	}
	persistentImageFaultTestHook = nil
	if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
		t.Fatalf("retry fsync: errno %d", errno)
	}
	backend.Release(nodeID, fh)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, lower, storeDir)
	defer backend.Close()
	nodeID = lookupPersistentTest(t, backend, 1, "state")
	if got := readPersistentTest(t, backend, nodeID, 0, 7); string(got) != "durable" {
		t.Fatalf("state after ENOSPC retries = %q", got)
	}
}

func TestPersistentImageFSMetadataENOSPCDoesNotReportFalseOperationFailure(t *testing.T) {
	t.Cleanup(func() { persistentImageFaultTestHook = nil })
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	persistentImageFaultTestHook = func(stage string) error {
		if stage == "before-wal-append" {
			return syscall.ENOSPC
		}
		return nil
	}
	dirID, _, errno := backend.Mkdir(1, "research", 0o700, 1000, 1000)
	if errno != 0 {
		t.Fatalf("mkdir under pending WAL ENOSPC: errno %d", errno)
	}
	if lookupPersistentTest(t, backend, 1, "research") != dirID {
		t.Fatal("successful mkdir was not visible")
	}
	if errno := backend.FsyncDir(dirID, 0, 0); errno != -linuxENOSPC {
		t.Fatalf("fsync metadata under ENOSPC: errno %d", errno)
	}
	if lookupPersistentTest(t, backend, 1, "research") != dirID {
		t.Fatal("failed metadata flush rolled back or hid live state")
	}
	persistentImageFaultTestHook = nil
	if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
		t.Fatalf("retry metadata fsync: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	lookupPersistentTest(t, backend, 1, "research")
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentImageFSCheckpointENOSPCLeavesRecoverableWAL(t *testing.T) {
	t.Cleanup(func() { persistentImageFaultTestHook = nil })
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	nodeID, fh, _, errno := backend.Create(1, "state", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create state: errno %d", errno)
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("safe"), 0); errno != 0 || written != 4 {
		t.Fatalf("write state = %d, errno %d", written, errno)
	}
	if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
		t.Fatalf("fsync state: errno %d", errno)
	}
	backend.Release(nodeID, fh)
	persistentImageFaultTestHook = func(stage string) error {
		if stage == "before-metadata-publish" {
			return syscall.ENOSPC
		}
		return nil
	}
	if err := backend.Close(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("close under checkpoint ENOSPC = %v", err)
	}
	persistentImageFaultTestHook = nil

	backend = openPersistentImageFSTest(t, lower, storeDir)
	defer backend.Close()
	nodeID = lookupPersistentTest(t, backend, 1, "state")
	if got := readPersistentTest(t, backend, nodeID, 0, 4); string(got) != "safe" {
		t.Fatalf("state recovered from WAL = %q", got)
	}
	if statuses := backend.PersistentFSStatus(); len(statuses) != 1 || statuses[0].RecoveryStatus != "recovered-incomplete-close" || statuses[0].LastError != "" {
		t.Fatalf("incomplete close recovery status = %+v", statuses)
	}
}

func TestPersistentImageMetadataFallsBackToPreviousSlot(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	lower := imagefs.NewOverlay(nil).Root()
	backend := openPersistentImageFSTest(t, lower, storeDir)
	firstID, _, _, errno := backend.Create(1, "first", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create first: errno %d", errno)
	}
	if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
		t.Fatalf("persist first: errno %d", errno)
	}
	backend.mu.Lock()
	err := backend.checkpointPersistentLocked(true)
	backend.mu.Unlock()
	if err != nil {
		t.Fatalf("checkpoint first: %v", err)
	}
	if firstID == 0 {
		t.Fatal("first inode is zero")
	}
	if _, _, _, errno := backend.Create(1, "second", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0); errno != 0 {
		t.Fatalf("create second: errno %d", errno)
	}
	if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
		t.Fatalf("persist second: errno %d", errno)
	}
	backend.mu.Lock()
	err = backend.checkpointPersistentLocked(true)
	backend.mu.Unlock()
	if err != nil {
		t.Fatalf("checkpoint second: %v", err)
	}
	newest := backend.persistent.metadataPath(backend.persistent.metadataSlot)
	if err := backend.persistent.close(); err != nil {
		t.Fatal(err)
	}
	backend.persistent = nil
	if err := backend.dataStore.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newest, []byte("torn"), 0o600); err != nil {
		t.Fatal(err)
	}

	recovered := openPersistentImageFSTest(t, lower, storeDir)
	defer recovered.Close()
	lookupPersistentTest(t, recovered, 1, "first")
	if _, _, errno := recovered.Lookup(1, "second"); errno != -linuxENOENT {
		t.Fatalf("newest torn transaction was not discarded: errno %d", errno)
	}
}

func TestPersistentImageDeletionHardCrashStages(t *testing.T) {
	for _, stage := range []string{"wal-written", "wal-closed", "data-moved-to-trash", "trash-removed"} {
		t.Run(stage, func(t *testing.T) {
			storeDir := filepath.Join(t.TempDir(), "home")
			lower := imagefs.NewOverlay(nil).Root()
			backend := openPersistentImageFSTest(t, lower, storeDir)
			nodeID, fh, _, errno := backend.Create(1, "important", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
			if errno != 0 {
				t.Fatalf("create important: errno %d", errno)
			}
			if written, errno := backend.Write(nodeID, fh, 0, []byte("safe"), 0); errno != 0 || written != 4 {
				t.Fatalf("write important = %d, errno %d", written, errno)
			}
			if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
				t.Fatalf("fsync important: errno %d", errno)
			}
			backend.Release(nodeID, fh)
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestPersistentImageFSHardCrashHelper$")
			cmd.Env = append(os.Environ(),
				"CC_PERSISTENT_IMAGE_CRASH_HELPER=1",
				"CC_PERSISTENT_IMAGE_CRASH_OPERATION=delete",
				"CC_PERSISTENT_IMAGE_CRASH_STORE="+storeDir,
				"CC_PERSISTENT_IMAGE_CRASH_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
				t.Fatalf("crash helper = %v, output:\n%s", err, output)
			}

			recovered := openPersistentImageFSTest(t, lower, storeDir)
			if importantID, _, errno := recovered.Lookup(1, "important"); errno == 0 {
				if got := readPersistentTest(t, recovered, importantID, 0, 4); string(got) != "safe" {
					t.Fatalf("retained important data = %q", got)
				}
			} else if errno != -linuxENOENT {
				t.Fatalf("lookup important after crash: errno %d", errno)
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPersistentImageMetadataHardCrashStages(t *testing.T) {
	for _, stage := range []string{
		"wal-written",
		"wal-synced",
		"metadata-written",
		"metadata-synced",
		"metadata-published",
		"directory-synced",
	} {
		t.Run(stage, func(t *testing.T) {
			storeDir := filepath.Join(t.TempDir(), "home")
			lower := imagefs.NewOverlay(nil).Root()
			baseline := openPersistentImageFSTest(t, lower, storeDir)
			nodeID, fh, _, errno := baseline.Create(1, "baseline", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
			if errno != 0 {
				t.Fatalf("create baseline: errno %d", errno)
			}
			if written, errno := baseline.Write(nodeID, fh, 0, []byte("safe"), 0); errno != 0 || written != 4 {
				t.Fatalf("write baseline = %d, errno %d", written, errno)
			}
			if errno := baseline.Fsync(nodeID, fh, 0); errno != 0 {
				t.Fatalf("fsync baseline: errno %d", errno)
			}
			baseline.Release(nodeID, fh)
			if err := baseline.Close(); err != nil {
				t.Fatalf("close baseline: %v", err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestPersistentImageFSHardCrashHelper$")
			cmd.Env = append(os.Environ(),
				"CC_PERSISTENT_IMAGE_CRASH_HELPER=1",
				"CC_PERSISTENT_IMAGE_CRASH_STORE="+storeDir,
				"CC_PERSISTENT_IMAGE_CRASH_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
				t.Fatalf("crash helper = %v, output:\n%s", err, output)
			}

			recovered := openPersistentImageFSTest(t, lower, storeDir)
			baselineID := lookupPersistentTest(t, recovered, 1, "baseline")
			if got := readPersistentTest(t, recovered, baselineID, 0, 4); string(got) != "safe" {
				t.Fatalf("baseline after crash = %q", got)
			}
			if afterID, _, errno := recovered.Lookup(1, "after-crash"); errno == 0 {
				if got := readPersistentTest(t, recovered, afterID, 0, uint32(len("complete"))); len(got) != 0 && string(got) != "complete" {
					t.Fatalf("file after crash is partial: %q", got)
				}
			} else if errno != -linuxENOENT {
				t.Fatalf("lookup optional crash transaction: errno %d", errno)
			}
			if err := recovered.Close(); err != nil {
				t.Fatalf("close recovered store: %v", err)
			}
		})
	}
}

func TestPersistentImageFSDiscardsCompleteUncommittedWALAfterCrash(t *testing.T) {
	for _, stage := range []string{"wal-written", "wal-closed"} {
		t.Run(stage, func(t *testing.T) {
			storeDir := filepath.Join(t.TempDir(), "home")
			backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
			if _, _, _, errno := backend.Create(1, "durable", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0); errno != 0 {
				t.Fatalf("create durable baseline: errno %d", errno)
			}
			if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
				t.Fatalf("fsync durable baseline: errno %d", errno)
			}
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestPersistentImageFSHardCrashHelper$")
			cmd.Env = append(os.Environ(),
				"CC_PERSISTENT_IMAGE_CRASH_HELPER=1",
				"CC_PERSISTENT_IMAGE_CRASH_OPERATION=uncommitted",
				"CC_PERSISTENT_IMAGE_CRASH_STORE="+storeDir,
				"CC_PERSISTENT_IMAGE_CRASH_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
				t.Fatalf("crash helper = %v, output:\n%s", err, output)
			}

			recovered := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
			lookupPersistentTest(t, recovered, 1, "durable")
			if _, _, errno := recovered.Lookup(1, "not-durable"); errno != -linuxENOENT {
				t.Fatalf("uncommitted directory survived crash: errno %d", errno)
			}
			statuses := recovered.PersistentFSStatus()
			if len(statuses) != 1 || statuses[0].RecoveryStatus != "discarded-uncommitted-wal-tail" {
				t.Fatalf("uncommitted recovery status = %+v", statuses)
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPersistentImageFSFileFsyncLeavesUnrelatedDataDirty(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	firstID, firstFH, _, errno := backend.Create(1, "first", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create first: errno %d", errno)
	}
	secondID, secondFH, _, errno := backend.Create(1, "second", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create second: errno %d", errno)
	}
	if written, errno := backend.Write(firstID, firstFH, 0, []byte("first-data"), 0); errno != 0 || written != 10 {
		t.Fatalf("write first = %d, errno %d", written, errno)
	}
	if written, errno := backend.Write(secondID, secondFH, 0, []byte("second-data"), 0); errno != 0 || written != 11 {
		t.Fatalf("write second = %d, errno %d", written, errno)
	}
	if errno := backend.Fsync(firstID, firstFH, 0); errno != 0 {
		t.Fatalf("fsync first: errno %d", errno)
	}
	if _, dirty := backend.persistent.dirtyData[firstID]; dirty {
		t.Fatalf("fsynced inode remained dirty: %v", backend.persistent.dirtyData)
	}
	if _, dirty := backend.persistent.dirtyData[secondID]; !dirty {
		t.Fatalf("unrelated inode was made durable: %v", backend.persistent.dirtyData)
	}
	if errno := backend.SyncFS(); errno != 0 {
		t.Fatalf("syncfs: errno %d", errno)
	}
	if len(backend.persistent.dirtyData) != 0 {
		t.Fatalf("syncfs retained dirty data: %v", backend.persistent.dirtyData)
	}
	backend.Release(firstID, firstFH)
	backend.Release(secondID, secondFH)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	firstID = lookupPersistentTest(t, backend, 1, "first")
	secondID = lookupPersistentTest(t, backend, 1, "second")
	if got := readPersistentTest(t, backend, firstID, 0, 10); string(got) != "first-data" {
		t.Fatalf("first data after barrier = %q", got)
	}
	if got := readPersistentTest(t, backend, secondID, 0, 11); string(got) != "second-data" {
		t.Fatalf("second data after barrier = %q", got)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentImageFSFileFsyncDoesNotCommitUnrelatedDataAfterCrash(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPersistentImageFSHardCrashHelper$")
	cmd.Env = append(os.Environ(),
		"CC_PERSISTENT_IMAGE_CRASH_HELPER=1",
		"CC_PERSISTENT_IMAGE_CRASH_OPERATION=selective-fsync",
		"CC_PERSISTENT_IMAGE_CRASH_STORE="+storeDir,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
		t.Fatalf("crash helper = %v, output:\n%s", err, output)
	}

	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	defer backend.Close()
	firstID := lookupPersistentTest(t, backend, 1, "first")
	if got := readPersistentTest(t, backend, firstID, 0, uint32(len("first-data"))); string(got) != "first-data" {
		t.Fatalf("fsynced data after crash = %q", got)
	}
	if _, _, errno := backend.Lookup(1, "second"); errno != -linuxENOENT {
		t.Fatalf("un-fsynced file survived crash: errno %d", errno)
	}
}

func TestPersistentImageFSReadsRemainResponsiveDuringHostDataSync(t *testing.T) {
	t.Cleanup(func() { persistentImageFaultTestHook = nil })
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), filepath.Join(t.TempDir(), "home"))
	defer backend.Close()
	nodeID, fh, _, errno := backend.Create(1, "state", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create state: errno %d", errno)
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("data"), 0); errno != 0 || written != 4 {
		t.Fatalf("write state: written=%d errno=%d", written, errno)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	persistentImageFaultTestHook = func(stage string) error {
		if stage == "before-data-sync" {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	fsyncDone := make(chan int32, 1)
	go func() { fsyncDone <- backend.Fsync(nodeID, fh, 0) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fsync did not enter host data sync")
	}
	lookupDone := make(chan int32, 1)
	go func() {
		lookupID, _, errno := backend.Lookup(1, "state")
		if errno != 0 {
			lookupDone <- errno
			return
		}
		readFH, errno := backend.Open(lookupID, linuxORDONLY)
		if errno != 0 {
			lookupDone <- errno
			return
		}
		_, errno = backend.Read(lookupID, readFH, 0, 4)
		backend.Release(lookupID, readFH)
		lookupDone <- errno
	}()
	select {
	case errno := <-lookupDone:
		if errno != 0 {
			t.Fatalf("lookup during fsync: errno %d", errno)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("read-only open blocked behind host data sync")
	}
	close(release)
	if errno := <-fsyncDone; errno != 0 {
		t.Fatalf("fsync after release: errno %d", errno)
	}
}

func TestPersistentImageFSCloseDoesNotResyncDurableHome(t *testing.T) {
	t.Cleanup(func() { persistentImageFaultTestHook = nil })
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), filepath.Join(t.TempDir(), "home"))
	nodeID, fh, _, errno := backend.Create(1, "state", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create state: errno %d", errno)
	}
	if written, errno := backend.Write(nodeID, fh, 0, []byte("durable"), 0); errno != 0 || written != 7 {
		t.Fatalf("write state: written=%d errno=%d", written, errno)
	}
	if errno := backend.SyncFS(); errno != 0 {
		t.Fatalf("sync baseline: errno %d", errno)
	}
	backend.Release(nodeID, fh)
	persistentImageFaultTestHook = func(stage string) error {
		if stage == "before-data-sync" {
			return errors.New("durable inode was synchronized again")
		}
		return nil
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentImageFSCloseSynchronizesDirtyFilesConcurrently(t *testing.T) {
	t.Cleanup(func() { persistentImageFaultTestHook = nil })
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), filepath.Join(t.TempDir(), "home"))
	for _, name := range []string{"first", "second"} {
		nodeID, fh, _, errno := backend.Create(1, name, linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
		if errno != 0 {
			t.Fatalf("create %s: errno %d", name, errno)
		}
		if written, errno := backend.Write(nodeID, fh, 0, []byte(name), 0); errno != 0 || written != uint32(len(name)) {
			t.Fatalf("write %s: written=%d errno=%d", name, written, errno)
		}
		backend.Release(nodeID, fh)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	persistentImageFaultTestHook = func(stage string) error {
		if stage == "before-data-sync" {
			entered <- struct{}{}
			<-release
		}
		return nil
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- backend.closeWithin(time.Second) }()
	for count := 0; count < 2; count++ {
		select {
		case <-entered:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("independent dirty files were synchronized serially")
		}
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func TestPersistentImageFSFileAndDirectoryFsyncHaveSeparateDurability(t *testing.T) {
	for _, test := range []struct {
		name    string
		syncDir bool
	}{
		{name: "file-only"},
		{name: "file-and-directory", syncDir: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			storeDir := filepath.Join(t.TempDir(), "home")
			cmd := exec.Command(os.Args[0], "-test.run=^TestPersistentImageFSHardCrashHelper$")
			cmd.Env = append(os.Environ(),
				"CC_PERSISTENT_IMAGE_CRASH_HELPER=1",
				"CC_PERSISTENT_IMAGE_CRASH_OPERATION=selective-namespace",
				"CC_PERSISTENT_IMAGE_CRASH_STORE="+storeDir,
				fmt.Sprintf("CC_PERSISTENT_IMAGE_CRASH_SYNC_DIR=%t", test.syncDir),
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
				t.Fatalf("crash helper = %v, output:\n%s", err, output)
			}
			backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
			defer backend.Close()
			firstID := lookupPersistentTest(t, backend, 1, "first")
			if got := readPersistentTest(t, backend, firstID, 0, 7); string(got) != "initial" {
				t.Fatalf("baseline file after crash = %q", got)
			}
			durableName, missingName := "second", "moved"
			if test.syncDir {
				durableName, missingName = "moved", "second"
			}
			secondID := lookupPersistentTest(t, backend, 1, durableName)
			if got := readPersistentTest(t, backend, secondID, 0, 7); string(got) != "updated" {
				t.Fatalf("fsynced inode after crash = %q", got)
			}
			if _, _, errno := backend.Lookup(1, missingName); errno != -linuxENOENT {
				t.Fatalf("non-durable name %q survived crash: errno %d", missingName, errno)
			}
		})
	}
}

func TestPersistentImageFSPreservesUnreferencedCrashData(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := (&persistentImageStore{dir: storeDir}).dataPath(0xabc)
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("possibly useful crash data"), 0o600); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	statuses := backend.PersistentFSStatus()
	if len(statuses) != 1 || statuses[0].RecoveryStatus != "preserved-unreferenced-data" {
		t.Fatalf("unreferenced data recovery status = %+v", statuses)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced data remained live: %v", err)
	}
	entries, err := os.ReadDir(statuses[0].QuarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantined data files = %d, want 1", len(entries))
	}
	content, err := os.ReadFile(filepath.Join(statuses[0].QuarantinePath, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "possibly useful crash data" {
		t.Fatalf("quarantined data = %q", content)
	}
	// Simulate recovery metadata written by an older build. Completed recovery
	// diagnostics must not surface as an error on every later attachment.
	backend.persistent.lastErr = errors.New("preserved 1 unreferenced inode data files in " + statuses[0].QuarantinePath)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	defer backend.Close()
	statuses = backend.PersistentFSStatus()
	if len(statuses) != 1 || statuses[0].RecoveryStatus != "" || statuses[0].LastError != "" {
		t.Fatalf("completed recovery repeated on next attachment: %+v", statuses)
	}
}

func TestPersistentImageFSConcurrentIndependentWorkloadsSurviveRestart(t *testing.T) {
	const workers = 32
	storeDir := filepath.Join(t.TempDir(), "home")
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			dirName := fmt.Sprintf("worker-%02d", worker)
			dirID, _, errno := backend.Mkdir(1, dirName, 0o700, uint32(worker+1000), uint32(worker+1000))
			if errno != 0 {
				errs <- fmt.Errorf("mkdir %s: errno %d", dirName, errno)
				return
			}
			nodeID, fh, _, errno := backend.Create(dirID, "state.tmp", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
			if errno != 0 {
				errs <- fmt.Errorf("create %s: errno %d", dirName, errno)
				return
			}
			payload := bytes.Repeat([]byte{byte(worker + 1)}, 64<<10)
			for offset := 0; offset < len(payload); offset += 997 {
				end := min(len(payload), offset+997)
				written, errno := backend.Write(nodeID, fh, uint64(offset), payload[offset:end], 0)
				if errno != 0 || int(written) != end-offset {
					errs <- fmt.Errorf("write %s at %d = %d, errno %d", dirName, offset, written, errno)
					return
				}
			}
			if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
				errs <- fmt.Errorf("fsync %s: errno %d", dirName, errno)
				return
			}
			backend.Release(nodeID, fh)
			if _, _, errno := backend.Link(nodeID, dirID, "state-link"); errno != 0 {
				errs <- fmt.Errorf("link %s: errno %d", dirName, errno)
				return
			}
			if errno := backend.Rename(dirID, "state.tmp", dirID, "state", 0); errno != 0 {
				errs <- fmt.Errorf("rename %s: errno %d", dirName, errno)
				return
			}
			if errno := backend.FsyncDir(dirID, 0, 0); errno != 0 {
				errs <- fmt.Errorf("fsync directory %s: errno %d", dirName, errno)
			}
		}(worker)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		_ = backend.Close()
		return
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	backend = openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	defer backend.Close()
	for worker := 0; worker < workers; worker++ {
		dirName := fmt.Sprintf("worker-%02d", worker)
		dirID := lookupPersistentTest(t, backend, 1, dirName)
		nodeID := lookupPersistentTest(t, backend, dirID, "state")
		linkID := lookupPersistentTest(t, backend, dirID, "state-link")
		if nodeID != linkID {
			t.Fatalf("%s hard link inode = %d, want %d", dirName, linkID, nodeID)
		}
		expected := bytes.Repeat([]byte{byte(worker + 1)}, 64<<10)
		if got := readPersistentTest(t, backend, nodeID, 0, uint32(len(expected))); !bytes.Equal(got, expected) {
			t.Fatalf("%s data changed after restart", dirName)
		}
	}
}

func TestPersistentImageFSHardCrashHelper(t *testing.T) {
	if os.Getenv("CC_PERSISTENT_IMAGE_CRASH_HELPER") != "1" {
		return
	}
	stage := os.Getenv("CC_PERSISTENT_IMAGE_CRASH_STAGE")
	storeDir := os.Getenv("CC_PERSISTENT_IMAGE_CRASH_STORE")
	backend := openPersistentImageFSTest(t, imagefs.NewOverlay(nil).Root(), storeDir)
	if os.Getenv("CC_PERSISTENT_IMAGE_CRASH_OPERATION") == "selective-namespace" {
		firstID, firstFH, _, errno := backend.Create(1, "first", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
		if errno != 0 {
			t.Fatalf("create first: errno %d", errno)
		}
		secondID, secondFH, _, errno := backend.Create(1, "second", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
		if errno != 0 {
			t.Fatalf("create second: errno %d", errno)
		}
		if written, errno := backend.Write(firstID, firstFH, 0, []byte("initial"), 0); errno != 0 || written != 7 {
			t.Fatalf("write initial: written=%d errno=%d", written, errno)
		}
		if errno := backend.SyncFS(); errno != 0 {
			t.Fatalf("sync baseline: errno %d", errno)
		}
		if errno := backend.Rename(1, "second", 1, "moved", 0); errno != 0 {
			t.Fatalf("rename second: errno %d", errno)
		}
		if written, errno := backend.Write(secondID, secondFH, 0, []byte("updated"), 0); errno != 0 || written != 7 {
			t.Fatalf("write updated: written=%d errno=%d", written, errno)
		}
		if errno := backend.Fsync(secondID, secondFH, 0); errno != 0 {
			t.Fatalf("fsync renamed inode: errno %d", errno)
		}
		if os.Getenv("CC_PERSISTENT_IMAGE_CRASH_SYNC_DIR") == "true" {
			if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
				t.Fatalf("fsync root directory: errno %d", errno)
			}
		}
		os.Exit(91)
	}
	if os.Getenv("CC_PERSISTENT_IMAGE_CRASH_OPERATION") == "selective-fsync" {
		firstID, firstFH, _, errno := backend.Create(1, "first", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
		if errno != 0 {
			t.Fatalf("create first: errno %d", errno)
		}
		secondID, secondFH, _, errno := backend.Create(1, "second", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
		if errno != 0 {
			t.Fatalf("create second: errno %d", errno)
		}
		if written, errno := backend.Write(firstID, firstFH, 0, []byte("first-data"), 0); errno != 0 || written != 10 {
			t.Fatalf("write first: written=%d errno=%d", written, errno)
		}
		if written, errno := backend.Write(secondID, secondFH, 0, []byte("second-data"), 0); errno != 0 || written != 11 {
			t.Fatalf("write second: written=%d errno=%d", written, errno)
		}
		if errno := backend.Fsync(firstID, firstFH, 0); errno != 0 {
			t.Fatalf("fsync first: errno %d", errno)
		}
		os.Exit(91)
	}
	if os.Getenv("CC_PERSISTENT_IMAGE_CRASH_OPERATION") == "uncommitted" {
		persistentImageSaveTestHook = func(current string) {
			if current == stage {
				os.Exit(91)
			}
		}
		if _, _, errno := backend.Mkdir(1, "not-durable", 0o700, 0, 0); errno != 0 {
			t.Fatalf("mkdir not-durable: errno %d", errno)
		}
		backend.mu.Lock()
		err := backend.flushPersistentLocked(false)
		backend.mu.Unlock()
		if err != nil {
			t.Fatalf("write legacy uncommitted WAL: %v", err)
		}
		t.Fatalf("crash stage %q was not reached", stage)
	}
	if os.Getenv("CC_PERSISTENT_IMAGE_CRASH_OPERATION") == "delete" {
		lookupPersistentTest(t, backend, 1, "important")
		persistentImageSaveTestHook = func(current string) {
			if current == stage {
				os.Exit(91)
			}
		}
		if errno := backend.Unlink(1, "important"); errno != 0 {
			t.Fatalf("unlink important: errno %d", errno)
		}
		if errno := backend.FsyncDir(1, 0, 0); errno != 0 {
			t.Fatalf("fsync parent: errno %d", errno)
		}
		t.Fatalf("crash stage %q was not reached", stage)
	}
	nodeID, fh, _, errno := backend.Create(1, "after-crash", linuxORDWR|linuxOCREAT|linuxOEXCL, 0o600, 0, 0)
	if errno != 0 {
		t.Fatalf("create crash file: errno %d", errno)
	}
	payload := []byte("complete")
	if written, errno := backend.Write(nodeID, fh, 0, payload, 0); errno != 0 || written != uint32(len(payload)) {
		t.Fatalf("write crash file = %d, errno %d", written, errno)
	}
	persistentImageSaveTestHook = func(current string) {
		if current == stage {
			os.Exit(91)
		}
	}
	if errno := backend.Fsync(nodeID, fh, 0); errno != 0 {
		t.Fatalf("fsync crash file: errno %d", errno)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close crash filesystem: %v", err)
	}
	t.Fatalf("crash stage %q was not reached", stage)
}

func openPersistentImageFSTest(t *testing.T, lower imagefs.Directory, storeDir string) *imageFS {
	t.Helper()
	fsys, err := NewPersistentImageFS(lower, PersistentImageFSOptions{
		StoreDir: storeDir,
		LowerID:  "test",
	})
	if err != nil {
		t.Fatalf("open persistent image filesystem: %v", err)
	}
	backend, ok := fsys.(*imageFS)
	if !ok {
		t.Fatalf("persistent backend type = %T", fsys)
	}
	return backend
}

func lookupPersistentTest(t *testing.T, backend *imageFS, parent uint64, name string) uint64 {
	t.Helper()
	nodeID, _, errno := backend.Lookup(parent, name)
	if errno != 0 {
		t.Fatalf("lookup %q: errno %d", name, errno)
	}
	return nodeID
}

func readPersistentTest(t *testing.T, backend *imageFS, nodeID, off uint64, size uint32) []byte {
	t.Helper()
	fh, errno := backend.Open(nodeID, linuxORDONLY)
	if errno != 0 {
		t.Fatalf("open node %d: errno %d", nodeID, errno)
	}
	defer backend.Release(nodeID, fh)
	data, errno := backend.Read(nodeID, fh, off, size)
	if errno != 0 {
		t.Fatalf("read node %d: errno %d", nodeID, errno)
	}
	return data
}

func readPersistentHandleTest(t *testing.T, backend *imageFS, nodeID, fh, off uint64, size uint32) []byte {
	t.Helper()
	data, errno := backend.Read(nodeID, fh, off, size)
	if errno != 0 {
		t.Fatalf("read node %d handle %d: errno %d", nodeID, fh, errno)
	}
	return data
}

func (p *imageFS) persistentDataPathForTest(nodeID uint64, storeDir string) string {
	if p.persistent != nil {
		return p.persistent.dataPath(nodeID)
	}
	id := persistentImageStore{dir: storeDir}
	return id.dataPath(nodeID)
}
