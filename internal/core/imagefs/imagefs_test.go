package imagefs

import (
	"archive/tar"
	"bytes"
	"context"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOverlayMergesBaseAndOverrides(t *testing.T) {
	baseDir := t.TempDir()
	mustWriteFile(t, filepath.Join(baseDir, "base.txt"), "base")
	mustMkdir(t, filepath.Join(baseDir, "etc"))
	mustWriteFile(t, filepath.Join(baseDir, "etc", "config"), "base-config")

	overlay := NewOverlay(NewHostFS(baseDir, nil))
	if err := overlay.AddFile("/etc/config", 0o644, []byte("overlay-config")); err != nil {
		t.Fatalf("override file: %v", err)
	}
	if err := overlay.AddDir("/opt/bin", fs.ModeDir|0o755); err != nil {
		t.Fatalf("add dir: %v", err)
	}
	if err := overlay.AddFile("/opt/bin/tool", 0o755, []byte("#!/bin/sh\n")); err != nil {
		t.Fatalf("add file: %v", err)
	}

	root := overlay.Root()
	entries, err := root.ReadDir()
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if got := direntNames(entries); got != "base.txt,etc,opt" {
		t.Fatalf("root entries = %q", got)
	}
	if got := readFile(t, root, "/etc/config"); got != "overlay-config" {
		t.Fatalf("overlay file = %q", got)
	}
	if got := readFile(t, root, "/base.txt"); got != "base" {
		t.Fatalf("base file = %q", got)
	}
}

func TestResolveCommandUsesPathAndExecutableBits(t *testing.T) {
	overlay := NewOverlay(nil)
	if err := overlay.AddDir("/bin", fs.ModeDir|0o755); err != nil {
		t.Fatalf("add bin: %v", err)
	}
	if err := overlay.AddFile("/bin/run", 0o755, []byte("run")); err != nil {
		t.Fatalf("add executable: %v", err)
	}
	if err := overlay.AddFile("/bin/not-exec", 0o644, []byte("nope")); err != nil {
		t.Fatalf("add non-executable: %v", err)
	}
	if err := overlay.AddDir("/sbin", fs.ModeDir|0o755); err != nil {
		t.Fatalf("add sbin: %v", err)
	}

	root := overlay.Root()
	cmd, err := ResolveCommand(root, []string{"run", "--flag"}, []string{"PATH=/missing:/bin"})
	if err != nil {
		t.Fatalf("resolve command: %v", err)
	}
	if strings.Join(cmd, " ") != "/bin/run --flag" {
		t.Fatalf("resolved command = %#v", cmd)
	}
	if _, err := ResolveCommand(root, []string{"not-exec"}, []string{"PATH=/bin"}); err == nil {
		t.Fatalf("non-executable command error = %v", err)
	}
	if _, err := ResolveCommand(root, []string{"/sbin"}, nil); err == nil {
		t.Fatalf("directory command error = %v", err)
	}
}

func TestResolvePathFollowsSymlinks(t *testing.T) {
	overlay := NewOverlay(nil)
	if err := overlay.AddDir("/bin", fs.ModeDir|0o755); err != nil {
		t.Fatalf("add bin: %v", err)
	}
	if err := overlay.AddFile("/bin/target", 0o755, []byte("target")); err != nil {
		t.Fatalf("add target: %v", err)
	}
	if err := overlay.AddSymlink("/bin/link", "../bin/target"); err != nil {
		t.Fatalf("add symlink: %v", err)
	}

	resolved, entry, err := ResolvePath(overlay.Root(), "/bin/link")
	if err != nil {
		t.Fatalf("resolve symlink: %v", err)
	}
	if resolved != "/bin/target" {
		t.Fatalf("resolved path = %q, want /bin/target", resolved)
	}
	if entry.File == nil {
		t.Fatalf("resolved symlink did not yield a file")
	}
}

func TestResolvePathFollowsIntermediateSymlinkDirectories(t *testing.T) {
	overlay := NewOverlay(nil)
	if err := overlay.AddDir("/usr/bin", fs.ModeDir|0o755); err != nil {
		t.Fatalf("add usr bin: %v", err)
	}
	if err := overlay.AddSymlink("/bin", "usr/bin"); err != nil {
		t.Fatalf("add bin symlink: %v", err)
	}
	if err := overlay.AddFile("/usr/bin/bash", 0o755, []byte("bash")); err != nil {
		t.Fatalf("add bash: %v", err)
	}
	if err := overlay.AddSymlink("/usr/bin/sh", "bash"); err != nil {
		t.Fatalf("add sh symlink: %v", err)
	}

	resolved, entry, err := ResolvePath(overlay.Root(), "/bin/sh")
	if err != nil {
		t.Fatalf("resolve intermediate symlink: %v", err)
	}
	if resolved != "/usr/bin/bash" {
		t.Fatalf("resolved path = %q, want /usr/bin/bash", resolved)
	}
	if entry.File == nil {
		t.Fatalf("resolved intermediate symlink did not yield a file")
	}

	cmd, err := ResolveCommand(overlay.Root(), []string{"sh", "-c", "true"}, []string{"PATH=/bin"})
	if err != nil {
		t.Fatalf("resolve command through intermediate symlink: %v", err)
	}
	if strings.Join(cmd, " ") != "/bin/sh -c true" {
		t.Fatalf("resolved command = %#v", cmd)
	}
}

func TestLookupPathCleansInputAndReportsNonDirectory(t *testing.T) {
	rootDir := t.TempDir()
	mustWriteFile(t, filepath.Join(rootDir, "file"), "contents")

	entry, err := LookupPath(NewHostFS(rootDir, nil), "///file")
	if err != nil {
		t.Fatalf("lookup cleaned path: %v", err)
	}
	if entry.File == nil {
		t.Fatalf("lookup did not return file")
	}
	if _, err := LookupPath(NewHostFS(rootDir, nil), "/file/child"); err == nil {
		t.Fatalf("lookup through file error = %v", err)
	}
}

func TestImageFSPreservesTrailingSpaceNamesAcrossBackends(t *testing.T) {
	t.Run("host", func(t *testing.T) {
		rootDir := t.TempDir()
		plainPath := filepath.Join(rootDir, "collision")
		spacedPath := filepath.Join(rootDir, "collision ")
		mustWriteFile(t, plainPath, "A")
		mustWriteFile(t, spacedPath, "B")
		plainInfo, plainErr := os.Stat(plainPath)
		spacedInfo, spacedErr := os.Stat(spacedPath)
		if plainErr == nil && spacedErr == nil && os.SameFile(plainInfo, spacedInfo) {
			t.Skip("host filesystem aliases trailing-space names")
		}
		root := NewHostFS(rootDir, nil)
		if got := readFile(t, root, "/collision"); got != "A" {
			t.Fatalf("plain file = %q", got)
		}
		if got := readFile(t, root, "/collision "); got != "B" {
			t.Fatalf("spaced file = %q", got)
		}
	})

	t.Run("overlay", func(t *testing.T) {
		overlay := NewOverlay(nil)
		if err := overlay.AddFile("/collision", 0o644, []byte("A")); err != nil {
			t.Fatal(err)
		}
		if err := overlay.AddFile("/collision ", 0o644, []byte("B")); err != nil {
			t.Fatal(err)
		}
		if err := overlay.AddSymlink("/spaced-link", "collision "); err != nil {
			t.Fatal(err)
		}
		root := overlay.Root()
		if got := readFile(t, root, "/collision"); got != "A" {
			t.Fatalf("plain file = %q", got)
		}
		if got := readFile(t, root, "/collision "); got != "B" {
			t.Fatalf("spaced file = %q", got)
		}
		resolved, _, err := ResolvePath(root, "/spaced-link")
		if err != nil || resolved != "/collision " {
			t.Fatalf("spaced symlink resolved to %q, %v", resolved, err)
		}
	})

	t.Run("tar", func(t *testing.T) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		mustWriteTarFile(t, tw, "collision", "A")
		mustWriteTarFile(t, tw, "collision ", "B")
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		tfs, err := NewTarFS(t.Context(), bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer tfs.Close()
		if got := readFile(t, tfs.Root(), "/collision"); got != "A" {
			t.Fatalf("plain file = %q", got)
		}
		if got := readFile(t, tfs.Root(), "/collision "); got != "B" {
			t.Fatalf("spaced file = %q", got)
		}
	})
}

func TestTarFSKeepsImplicitDirectoryChildrenWhenDirectoryHeaderAppearsLater(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarFile(t, tw, "usr/lib/libearly.so.1.0", "early")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "usr/lib",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}); err != nil {
		t.Fatalf("write usr/lib header: %v", err)
	}
	mustWriteTarFile(t, tw, "usr/lib/late.o", "late")
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	tfs, err := NewTarFS(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("read tarfs: %v", err)
	}
	defer tfs.Close()
	root := tfs.Root()
	if got := readFile(t, root, "/usr/lib/libearly.so.1.0"); got != "early" {
		t.Fatalf("early file = %q", got)
	}
	if got := readFile(t, root, "/usr/lib/late.o"); got != "late" {
		t.Fatalf("late file = %q", got)
	}
	entry, err := LookupPath(root, "/usr/lib")
	if err != nil {
		t.Fatalf("lookup /usr/lib: %v", err)
	}
	entries, err := entry.Dir.ReadDir()
	if err != nil {
		t.Fatalf("read /usr/lib: %v", err)
	}
	if got := direntNames(entries); got != "late.o,libearly.so.1.0" {
		t.Fatalf("/usr/lib entries = %q", got)
	}
}

func TestSeekableTarFSReadsPayloadsFromTarFile(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarFile(t, tw, "bin/tool", "tool payload")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "bin/tool-hardlink",
		Typeflag: tar.TypeLink,
		Mode:     0o644,
		Linkname: "bin/tool",
	}); err != nil {
		t.Fatalf("write hardlink header: %v", err)
	}
	mustWriteTarFile(t, tw, "etc/config", "config payload")
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	tarPath := filepath.Join(t.TempDir(), "root.tar")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tar: %v", err)
	}

	tfs, err := NewSeekableTarFS(context.Background(), tarPath)
	if err != nil {
		t.Fatalf("read seekable tarfs: %v", err)
	}
	defer tfs.Close()
	root := tfs.Root()
	if got := readFile(t, root, "/bin/tool"); got != "tool payload" {
		t.Fatalf("tool = %q", got)
	}
	if got := readFile(t, root, "/bin/tool-hardlink"); got != "tool payload" {
		t.Fatalf("hardlink = %q", got)
	}
	if got := readFile(t, root, "/etc/config"); got != "config payload" {
		t.Fatalf("config = %q", got)
	}
}

func TestTarFSRejectsUnrepresentableMetadata(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("uint32 overflow metadata requires a 64-bit int")
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarFile(t, tw, "valid", "kept only if the archive is accepted")
	oversizedUID := uint64(math.MaxUint32) + 1
	if err := tw.WriteHeader(&tar.Header{
		Name:     "invalid",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
		Uid:      int(oversizedUID),
		Format:   tar.FormatPAX,
	}); err != nil {
		t.Fatalf("write invalid metadata header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	t.Run("streaming", func(t *testing.T) {
		tfs, err := NewTarFS(context.Background(), bytes.NewReader(buf.Bytes()))
		if tfs != nil {
			_ = tfs.Close()
			t.Fatal("malformed archive returned a partially populated filesystem")
		}
		if err == nil {
			t.Fatal("malformed archive was accepted")
		}
	})

	t.Run("seekable", func(t *testing.T) {
		tarPath := filepath.Join(t.TempDir(), "invalid.tar")
		if err := os.WriteFile(tarPath, buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write tar: %v", err)
		}
		tfs, err := NewSeekableTarFS(context.Background(), tarPath)
		if tfs != nil {
			_ = tfs.Close()
			t.Fatal("malformed archive returned a partially populated filesystem")
		}
		if err == nil {
			t.Fatal("malformed archive was accepted")
		}
	})
}

func FuzzValidateTarFSMetadataBoundaries(f *testing.F) {
	f.Add(int64(0), int64(0), int64(0), int64(0), int64(0), int64(0))
	f.Add(int64(math.MaxInt64), int64(math.MaxUint32), int64(math.MaxUint32), int64(math.MaxUint32>>8), int64(math.MaxUint32), int64(0))
	f.Add(int64(-1), int64(0), int64(0), int64(0), int64(0), int64(0))
	f.Add(int64(0), int64(-1), int64(0), int64(0), int64(0), int64(0))
	f.Add(int64(0), int64(math.MaxUint32)+1, int64(0), int64(0), int64(0), int64(0))
	f.Add(int64(0), int64(0), int64(math.MaxUint32)+1, int64(0), int64(0), int64(0))
	f.Add(int64(0), int64(0), int64(0), int64(math.MaxUint32>>8)+1, int64(0), int64(0))
	f.Add(int64(0), int64(0), int64(0), int64(0), int64(math.MaxUint32)+1, int64(0))
	f.Add(int64(math.MaxInt64), int64(0), int64(0), int64(0), int64(0), int64(1))

	f.Fuzz(func(t *testing.T, size, uidValue, gidValue, major, minor, offset int64) {
		uid := int(uidValue)
		gid := int(gidValue)
		if int64(uid) != uidValue || int64(gid) != gidValue {
			return
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeChar,
			Size:     size,
			Uid:      uid,
			Gid:      gid,
			Devmajor: major,
			Devminor: minor,
		}
		metadata, err := validateTarHeaderMetadata(hdr)
		wantMetadataError := size < 0 ||
			uidValue < 0 || uidValue > math.MaxUint32 ||
			gidValue < 0 || gidValue > math.MaxUint32 ||
			major < 0 || major > math.MaxUint32>>8 ||
			minor < 0 || minor > math.MaxUint32
		if (err != nil) != wantMetadataError {
			t.Fatalf("metadata validation error = %v, want error %t", err, wantMetadataError)
		}
		if err != nil {
			return
		}
		if metadata.size != uint64(size) || metadata.uid != uint32(uid) || metadata.gid != uint32(gid) {
			t.Fatalf("metadata conversion = %#v", metadata)
		}
		wantRDev := uint32(uint64(major)<<8 | uint64(minor))
		if metadata.rdev != wantRDev {
			t.Fatalf("rdev = %d, want %d", metadata.rdev, wantRDev)
		}

		rangeErr := validateTarPayloadRange(offset, size)
		wantRangeError := offset < 0 || size > math.MaxInt64-offset
		if (rangeErr != nil) != wantRangeError {
			t.Fatalf("payload range validation error = %v, want error %t", rangeErr, wantRangeError)
		}
	})
}

func direntNames(entries []DirEnt) string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return strings.Join(names, ",")
}

func readFile(t *testing.T, root Directory, guestPath string) string {
	t.Helper()
	entry, err := LookupPath(root, guestPath)
	if err != nil {
		t.Fatalf("lookup %s: %v", guestPath, err)
	}
	if entry.File == nil {
		t.Fatalf("%s is not a file", guestPath)
	}
	size, _ := entry.File.Stat()
	data, err := entry.File.ReadAt(0, uint32(size))
	if err != nil {
		t.Fatalf("read %s: %v", guestPath, err)
	}
	return string(data)
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustWriteExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func mustWriteTarFile(t *testing.T, tw *tar.Writer, name, contents string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(contents)),
	}); err != nil {
		t.Fatalf("write %s header: %v", name, err)
	}
	if _, err := tw.Write([]byte(contents)); err != nil {
		t.Fatalf("write %s payload: %v", name, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
