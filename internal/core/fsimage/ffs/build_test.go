package ffs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

func TestBuildFFSWritesOpenBSDRoot(t *testing.T) {
	overlay := imagefs.NewOverlay(nil)
	if err := overlay.AddFile("/sbin/init", 0o755, []byte("hello openbsd\n")); err != nil {
		t.Fatal(err)
	}
	if err := overlay.AddDevice("/dev/console", fs.ModeDevice|fs.ModeCharDevice|0o600, 0); err != nil {
		t.Fatal(err)
	}
	if err := overlay.AddSymlink("/etc/init", "/sbin/init"); err != nil {
		t.Fatal(err)
	}
	img, err := Build(context.Background(), overlay.Root(), Options{
		SizeBytes:         16 << 20,
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, img.Size())
	if _, err := img.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	labelOff := ffsPartLBA*ffsSectorSize + ffsSectorSize
	if got := binary.LittleEndian.Uint32(data[labelOff : labelOff+4]); got != dlMagic {
		t.Fatalf("disklabel magic = %#x, want %#x", got, dlMagic)
	}
	var labelXOR uint16
	for off := 0; off < 148+16*16; off += 2 {
		labelXOR ^= binary.LittleEndian.Uint16(data[labelOff+off : labelOff+off+2])
	}
	if labelXOR != 0 {
		t.Fatalf("disklabel checksum xor = %#x, want 0", labelXOR)
	}
	fsOff := int64(binary.LittleEndian.Uint32(data[labelOff+148+4 : labelOff+148+8]))
	fsOff *= ffsSectorSize
	if got := binary.LittleEndian.Uint32(data[fsOff+ffsSBOFF+1372 : fsOff+ffsSBOFF+1376]); got != ffsMagic {
		t.Fatalf("superblock magic = %#x, want %#x", got, ffsMagic)
	}
	reader := newFFSTestReader(t, data)
	initIno := reader.lookupPath("/sbin/init")
	if got := reader.inodeMode(initIno); got != ifreg|0o755 {
		t.Fatalf("/sbin/init mode = %#o, want %#o", got, ifreg|0o755)
	}
	if got := string(reader.fileData(initIno)); got != "hello openbsd\n" {
		t.Fatalf("/sbin/init contents = %q", got)
	}
	consoleIno := reader.lookupPath("/dev/console")
	if got := reader.inodeMode(consoleIno); got != ifchr|0o600 {
		t.Fatalf("/dev/console mode = %#o, want %#o", got, ifchr|0o600)
	}
	linkIno := reader.lookupPath("/etc/init")
	if got := reader.symlinkTarget(linkIno); got != "/sbin/init" {
		t.Fatalf("/etc/init target = %q", got)
	}
}

func TestBuildFFSRawLayoutStartsAtBlockDeviceRoot(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "sbin"))
	mustWriteMode(t, filepath.Join(root, "sbin", "init"), "hello freebsd\n", 0o755)
	meta := map[string]fsmeta.Entry{
		"/sbin/init": {Mode: fsmeta.LinuxModeFromFileMode(0o755)},
	}
	img, err := Build(context.Background(), imagefs.NewHostFS(root, meta), Options{
		SizeBytes:         16 << 20,
		DeterministicTime: time.Unix(1234, 0),
		Layout:            LayoutRaw,
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, img.Size())
	if _, err := img.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(data[ffsSBOFF+1372 : ffsSBOFF+1376]); got != ffsMagic {
		t.Fatalf("raw superblock magic = %#x, want %#x", got, ffsMagic)
	}
	labelOff := ffsPartLBA*ffsSectorSize + ffsSectorSize
	if got := binary.LittleEndian.Uint32(data[labelOff : labelOff+4]); got == dlMagic {
		t.Fatalf("raw layout unexpectedly wrote OpenBSD disklabel magic %#x", got)
	}
	reader := newRawFFSTestReader(t, data)
	initIno := reader.lookupPath("/sbin/init")
	if got := string(reader.fileData(initIno)); got != "hello freebsd\n" {
		t.Fatalf("/sbin/init contents = %q", got)
	}
}

func TestBuildFFSRawLayoutLargeCylinderGroupsAtFreeBSDOffsets(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "sbin"))
	mustWriteMode(t, filepath.Join(root, "sbin", "init"), "hello freebsd\n", 0o755)
	meta := map[string]fsmeta.Entry{
		"/sbin/init": {Mode: fsmeta.LinuxModeFromFileMode(0o755)},
	}
	img, err := Build(context.Background(), imagefs.NewHostFS(root, meta), Options{
		SizeBytes:         5 << 30,
		DeterministicTime: time.Unix(1234, 0),
		Layout:            LayoutRaw,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb [ffsBSize]byte
	if _, err := img.ReadAt(sb[:], ffsSBOFF); err != nil {
		t.Fatal(err)
	}
	fpg := binary.LittleEndian.Uint32(sb[188:192])
	if fpg == 0 {
		t.Fatal("superblock fs_fpg is zero")
	}
	for cgx := uint32(0); cgx < 4; cgx++ {
		var cg [ffsBSize]byte
		off := int64((cgx*fpg + ffsCBlkNo) * ffsFSize)
		if _, err := img.ReadAt(cg[:], off); err != nil {
			t.Fatalf("read cg %d at %d: %v", cgx, off, err)
		}
		if got := binary.LittleEndian.Uint32(cg[4:8]); got != cgMagic {
			t.Fatalf("cg %d magic at FreeBSD offset = %#x, want %#x", cgx, got, cgMagic)
		}
		if got := binary.LittleEndian.Uint32(cg[12:16]); got != cgx {
			t.Fatalf("cg %d cgx at FreeBSD offset = %d", cgx, got)
		}
	}
}

func TestBuildFFSLazilyMapsRegularFiles(t *testing.T) {
	file := &lazyTestFile{data: []byte("lazy payload\n")}
	root := lazyTestDir{entries: map[string]imagefs.Entry{
		"payload": {File: file},
	}}
	img, err := Build(context.Background(), root, Options{
		SizeBytes:         16 << 20,
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if file.reads != 0 {
		t.Fatalf("Build read file payload %d times; want lazy mapping", file.reads)
	}
	data := make([]byte, img.Size())
	if _, err := img.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if file.reads == 0 {
		t.Fatal("image read did not read mapped file payload")
	}
	reader := newFFSTestReader(t, data)
	payloadIno := reader.lookupPath("/payload")
	if got := string(reader.fileData(payloadIno)); got != "lazy payload\n" {
		t.Fatalf("/payload contents = %q", got)
	}
}

func TestBuildFFSTooSmallReturnsCapacityError(t *testing.T) {
	root := lazyTestDir{entries: map[string]imagefs.Entry{
		"large": {File: patternTestFile{size: 1 << 20}},
	}}
	img, err := Build(context.Background(), root, Options{
		SizeBytes: ffsDBlkNo*ffsFSize + ffsBSize,
	})
	if img != nil {
		t.Fatal("too-small build published an image")
	}
	var capacityErr *CapacityError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("too-small build error = %v, want CapacityError", err)
	}
	if capacityErr.RequiredBytes <= capacityErr.AvailableBytes {
		t.Fatalf("capacity error = %#v", capacityErr)
	}

	if _, err := Build(context.Background(), root, Options{}); err != nil {
		t.Fatalf("build after capacity failure: %v", err)
	}
}

func TestFFSFragmentAllocatorBoundaries(t *testing.T) {
	b := &ffsBuilder{
		fsSize:    ffsFrag * ffsFSize,
		usedFrags: make([]bool, ffsFrag),
	}
	for _, count := range []uint32{0, ffsFrag + 1} {
		if _, err := b.allocFragRun(count); err == nil {
			t.Fatalf("allocFragRun(%d) succeeded", count)
		}
	}
	frag, err := b.allocFragRun(ffsFrag)
	if err != nil {
		t.Fatalf("allocate exact-capacity block: %v", err)
	}
	if frag != 0 {
		t.Fatalf("exact-capacity block starts at fragment %d, want 0", frag)
	}
	if _, err := b.allocFragRun(1); err == nil {
		t.Fatal("allocation beyond capacity succeeded")
	} else {
		var capacityErr *CapacityError
		if !errors.As(err, &capacityErr) {
			t.Fatalf("allocation error = %v, want CapacityError", err)
		}
		if capacityErr.AvailableBytes != ffsFrag*ffsFSize || capacityErr.RequiredBytes != (ffsFrag+1)*ffsFSize {
			t.Fatalf("capacity error = %#v", capacityErr)
		}
	}
}

func TestBuildFFSDynamicInodeTableKeepsDirectoriesReadable(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "sbin"))
	mustWriteMode(t, filepath.Join(root, "sbin", "init"), "base init\n", 0o755)
	for i := 0; i < 900; i++ {
		mustMkdir(t, filepath.Join(root, "usr", "share", "dir"+strconv.Itoa(i)))
	}
	meta := map[string]fsmeta.Entry{
		"/sbin/init": {Mode: fsmeta.LinuxModeFromFileMode(0o755)},
	}
	img, err := Build(context.Background(), imagefs.NewHostFS(root, meta), Options{
		SizeBytes:         32 << 20,
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, img.Size())
	if _, err := img.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	reader := newFFSTestReader(t, data)
	sbinIno := reader.lookupPath("/sbin")
	if got := reader.inodeMode(sbinIno); got&ifmt != ifdir {
		t.Fatalf("/sbin mode = %#o, want directory", got)
	}
	initIno := reader.lookupPath("/sbin/init")
	if got := reader.inodeMode(initIno); got != ifreg|0o755 {
		t.Fatalf("/sbin/init mode = %#o, want %#o", got, ifreg|0o755)
	}
	for _, ino := range []uint32{128, 129} {
		if got := reader.inodeMode(ino); got&ifmt != ifdir {
			t.Fatalf("inode %d mode = %#o, want directory; inode table may overlap metadata", ino, got)
		}
	}
}

func TestBuildFFSWritesIndirectFileData(t *testing.T) {
	file := patternTestFile{size: 512 << 10}
	root := lazyTestDir{entries: map[string]imagefs.Entry{
		"large": {File: file},
	}}
	img, err := Build(context.Background(), root, Options{
		SizeBytes:         32 << 20,
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, img.Size())
	if _, err := img.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	reader := newFFSTestReader(t, data)
	ino := reader.lookupPath("/large")
	got := reader.fileData(ino)
	for _, off := range []int{0, ffsBSize*12 - 1, ffsBSize * 12, len(got) - 1} {
		if got[off] != patternByte(off) {
			t.Fatalf("large[%d] = %#x, want %#x", off, got[off], patternByte(off))
		}
	}
}

func TestBuildFFSPacksLargeDirectoriesIntoDirectoryBlocks(t *testing.T) {
	entries := map[string]imagefs.Entry{}
	for i := 0; i < 120; i++ {
		entries[fmt.Sprintf("file%03d", i)] = imagefs.Entry{File: &lazyTestFile{data: []byte("x")}}
	}
	root := lazyTestDir{entries: entries}
	img, err := Build(context.Background(), root, Options{
		SizeBytes:         32 << 20,
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, img.Size())
	if _, err := img.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	reader := newFFSTestReader(t, data)
	rootData := reader.fileData(ffsRootIno)
	if len(rootData) <= ffsSectorSize {
		t.Fatalf("root directory size = %d, want more than one directory block", len(rootData))
	}
	for i := 0; i < 120; i++ {
		name := fmt.Sprintf("file%03d", i)
		if _, ok := reader.dirEntries(ffsRootIno)[name]; !ok {
			t.Fatalf("%s not found", name)
		}
	}
	for blockStart := 0; blockStart < len(rootData); blockStart += ffsSectorSize {
		for off := blockStart; off < blockStart+ffsSectorSize; {
			reclen := int(binary.LittleEndian.Uint16(rootData[off+4 : off+6]))
			if reclen <= 0 {
				t.Fatalf("zero reclen at directory offset %d", off)
			}
			if off+reclen > blockStart+ffsSectorSize {
				t.Fatalf("directory record at %d crosses 512-byte boundary: reclen=%d", off, reclen)
			}
			off += reclen
		}
	}
}

func TestBuildFFSOpenBSDBaseSubsetStructure(t *testing.T) {
	base := os.Getenv("CC_TEST_OPENBSD_BASE79")
	if base == "" {
		base = filepath.Join("..", "..", "..", ".cache", "openbsd79", "base79.tgz")
	}
	if st, err := os.Stat(base); err != nil || st.Size() == 0 {
		t.Skip("OpenBSD base79.tgz not present")
	}
	f, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tfs, err := imagefs.NewTarFSWithOptions(context.Background(), gz, imagefs.TarFSOptions{Include: includeOpenBSDBaseSubsetTestFile})
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()
	img, err := Build(context.Background(), tfs.Root(), Options{
		SizeBytes:         224 << 20,
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, img.Size())
	if _, err := img.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	reader := newFFSTestReader(t, data)
	for _, p := range []string{"/sbin", "/usr", "/usr/libexec"} {
		ino := reader.lookupPath(p)
		if got := reader.inodeMode(ino); got&ifmt != ifdir {
			t.Fatalf("%s mode = %#o, want directory", p, got)
		}
	}
	for _, p := range []string{"/sbin/init", "/usr/libexec/ld.so"} {
		ino := reader.lookupPath(p)
		t.Logf("%s ino=%d mode=%#o size=%d", p, ino, reader.inodeMode(ino), reader.inodeSize(ino))
		if got := reader.inodeMode(ino); got&ifmt != ifreg {
			t.Fatalf("%s mode = %#o, want regular", p, got)
		}
		if len(reader.fileData(ino)) == 0 {
			t.Fatalf("%s has empty data", p)
		}
	}
}

func TestBuildFFSOpenBSDBaseSubsetFileBytes(t *testing.T) {
	base := openBSDBaseSubsetTestPath(t)
	root := openBSDBaseSubsetTestRoot(t, base)
	img, err := Build(context.Background(), root, Options{
		SizeBytes:         128 << 20,
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if _, err := io.Copy(&data, io.NewSectionReader(img, 0, img.Size())); err != nil {
		t.Fatal(err)
	}
	reader := newFFSTestReader(t, data.Bytes())
	names := []string{"/sbin/init", "/usr/libexec/ld.so"}
	libDir, err := imagefs.LookupPath(root, "/usr/lib")
	if err != nil {
		t.Fatalf("lookup /usr/lib: %v", err)
	}
	entries, err := libDir.Dir.ReadDir()
	if err != nil {
		t.Fatalf("read /usr/lib: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, "libc.so.") || strings.HasPrefix(entry.Name, "libpthread.so.") {
			names = append(names, "/usr/lib/"+entry.Name)
		}
	}
	for _, name := range names {
		entry, err := imagefs.LookupPath(root, name)
		if err != nil {
			t.Fatalf("lookup source %s: %v", name, err)
		}
		if entry.File == nil {
			t.Fatalf("source %s is not a file", name)
		}
		size, _ := entry.File.Stat()
		want, err := entry.File.ReadAt(0, uint32(size))
		if err != nil {
			t.Fatalf("read source %s: %v", name, err)
		}
		got := reader.fileData(reader.lookupPath(name))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s data mismatch: got %d bytes, want %d", name, len(got), len(want))
		}
	}
}

func TestBuildFFSOpenBSDFullBaseFileBytes(t *testing.T) {
	if os.Getenv("CC_TEST_OPENBSD_FULL_BASE_FFS") == "" {
		t.Skip("set CC_TEST_OPENBSD_FULL_BASE_FFS=1 to validate full OpenBSD base FFS image bytes")
	}
	base := openBSDBaseSubsetTestPath(t)
	root := openBSDBaseFullTestRoot(t, base)
	img, err := Build(context.Background(), root, Options{
		DeterministicTime: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := newFFSTestReaderAt(t, img)
	names := []string{"/sbin/init", "/bin/sh", "/bin/echo", "/bin/sleep", "/usr/libexec/ld.so"}
	libDir, err := imagefs.LookupPath(root, "/usr/lib")
	if err != nil {
		t.Fatalf("lookup /usr/lib: %v", err)
	}
	entries, err := libDir.Dir.ReadDir()
	if err != nil {
		t.Fatalf("read /usr/lib: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, "libc.so.") {
			names = append(names, "/usr/lib/"+entry.Name)
			break
		}
	}
	for _, name := range names {
		entry, err := imagefs.LookupPath(root, name)
		if err != nil {
			t.Fatalf("lookup source %s: %v", name, err)
		}
		if entry.File == nil {
			t.Fatalf("source %s is not a file", name)
		}
		size, _ := entry.File.Stat()
		want, err := entry.File.ReadAt(0, uint32(size))
		if err != nil {
			t.Fatalf("read source %s: %v", name, err)
		}
		got := reader.fileData(reader.lookupPath(name))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s data mismatch: got %d bytes, want %d", name, len(got), len(want))
		}
	}
}

func openBSDBaseSubsetTestPath(t *testing.T) string {
	t.Helper()
	base := os.Getenv("CC_TEST_OPENBSD_BASE79")
	if base == "" {
		base = filepath.Join("..", "..", "..", ".cache", "openbsd79", "base79.tgz")
	}
	if st, err := os.Stat(base); err != nil || st.Size() == 0 {
		t.Skip("OpenBSD base79.tgz not present")
	}
	return base
}

func openBSDBaseSubsetTestRoot(t *testing.T, base string) imagefs.Directory {
	t.Helper()
	f, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gz.Close() })
	tfs, err := imagefs.NewTarFSWithOptions(context.Background(), gz, imagefs.TarFSOptions{Include: includeOpenBSDBaseSubsetTestFile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tfs.Close() })
	return tfs.Root()
}

func openBSDBaseFullTestRoot(t *testing.T, base string) imagefs.Directory {
	t.Helper()
	f, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gz.Close() })
	tfs, err := imagefs.NewTarFS(context.Background(), gz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tfs.Close() })
	overlay := imagefs.NewOverlay(tfs.Root())
	initScript := []byte("#!/bin/sh\n/bin/echo OPENBSD_FULL_BASE_INIT_OK >/dev/console\nwhile :; do /bin/sleep 3600; done\n")
	if err := overlay.AddFile("/sbin/init", 0o755, initScript); err != nil {
		t.Fatalf("overlay shell init: %v", err)
	}
	return overlay.Root()
}

func includeOpenBSDBaseSubsetTestFile(name string, hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeDir {
		return true
	}
	switch cleanTestTarPath(name) {
	case "/sbin/init", "/bin/ksh", "/bin/sh", "/usr/libexec/ld.so":
		return true
	}
	base := path.Base(cleanTestTarPath(name))
	return strings.HasPrefix(cleanTestTarPath(name), "/usr/lib/") && (strings.HasPrefix(base, "libc.so.") || strings.HasPrefix(base, "libpthread.so."))
}

func cleanTestTarPath(name string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(name), "."), "/"))
}

type ffsTestReader struct {
	t     *testing.T
	data  io.ReaderAt
	base  int
	fsize int
	ipg   uint32
	fpg   uint32
}

func newFFSTestReader(t *testing.T, data []byte) *ffsTestReader {
	t.Helper()
	return newFFSTestReaderAt(t, bytes.NewReader(data))
}

func newRawFFSTestReader(t *testing.T, data []byte) *ffsTestReader {
	t.Helper()
	sb := make([]byte, ffsSBOFF+192)
	copy(sb, data[:len(sb)])
	return &ffsTestReader{
		t:     t,
		data:  bytes.NewReader(data),
		base:  0,
		fsize: int(binary.LittleEndian.Uint32(sb[ffsSBOFF+52 : ffsSBOFF+56])),
		ipg:   binary.LittleEndian.Uint32(sb[ffsSBOFF+184 : ffsSBOFF+188]),
		fpg:   binary.LittleEndian.Uint32(sb[ffsSBOFF+188 : ffsSBOFF+192]),
	}
}

func newFFSTestReaderAt(t *testing.T, data io.ReaderAt) *ffsTestReader {
	t.Helper()
	label := make([]byte, 512)
	if _, err := data.ReadAt(label, ffsPartLBA*ffsSectorSize+ffsSectorSize); err != nil {
		t.Fatal(err)
	}
	sb := make([]byte, ffsSBOFF+192)
	base := int(binary.LittleEndian.Uint32(label[148+4:148+8])) * ffsSectorSize
	if _, err := data.ReadAt(sb, int64(base)); err != nil {
		t.Fatal(err)
	}
	return &ffsTestReader{
		t:     t,
		data:  data,
		base:  base,
		fsize: int(binary.LittleEndian.Uint32(sb[ffsSBOFF+52 : ffsSBOFF+56])),
		ipg:   binary.LittleEndian.Uint32(sb[ffsSBOFF+184 : ffsSBOFF+188]),
		fpg:   binary.LittleEndian.Uint32(sb[ffsSBOFF+188 : ffsSBOFF+192]),
	}
}

func (r *ffsTestReader) lookupPath(name string) uint32 {
	r.t.Helper()
	ino := uint32(ffsRootIno)
	if name == "/" {
		return ino
	}
	for _, part := range splitTestPath(name) {
		entries := r.dirEntries(ino)
		next, ok := entries[part]
		if !ok {
			r.t.Fatalf("%s not found in inode %d", part, ino)
		}
		ino = next
	}
	return ino
}

func (r *ffsTestReader) inodeMode(ino uint32) uint16 {
	off := r.inodeOff(ino)
	return binary.LittleEndian.Uint16(r.readAt(off, 2))
}

func (r *ffsTestReader) inodeSize(ino uint32) uint64 {
	off := r.inodeOff(ino)
	return binary.LittleEndian.Uint64(r.readAt(off+8, 8))
}

func (r *ffsTestReader) symlinkTarget(ino uint32) string {
	off := r.inodeOff(ino)
	size := int(r.inodeSize(ino))
	return string(r.readAt(off+40, size))
}

func (r *ffsTestReader) fileData(ino uint32) []byte {
	off := r.inodeOff(ino)
	size := int(r.inodeSize(ino))
	out := make([]byte, size)
	for fileOff := 0; fileOff < size; {
		lbn := fileOff / ffsBSize
		block := r.fileBlock(off, lbn)
		start := r.base + int(block)*r.fsize
		n := ffsBSize
		if n > size-fileOff {
			n = size - fileOff
		}
		if _, err := r.data.ReadAt(out[fileOff:fileOff+n], int64(start)); err != nil && err != io.EOF {
			r.t.Fatalf("read file block %d at %d: %v", lbn, start, err)
		}
		fileOff += n
	}
	return out
}

func (r *ffsTestReader) fileBlock(inodeOff, lbn int) uint32 {
	if lbn < ffsNDaddr {
		return binary.LittleEndian.Uint32(r.readAt(inodeOff+40+lbn*4, 4))
	}
	lbn -= ffsNDaddr
	if lbn < ffsNindir {
		indir := binary.LittleEndian.Uint32(r.readAt(inodeOff+88, 4))
		off := r.base + int(indir)*r.fsize + lbn*4
		return binary.LittleEndian.Uint32(r.readAt(off, 4))
	}
	lbn -= ffsNindir
	double := binary.LittleEndian.Uint32(r.readAt(inodeOff+92, 4))
	rootOff := r.base + int(double)*r.fsize + (lbn/ffsNindir)*4
	indir := binary.LittleEndian.Uint32(r.readAt(rootOff, 4))
	off := r.base + int(indir)*r.fsize + (lbn%ffsNindir)*4
	return binary.LittleEndian.Uint32(r.readAt(off, 4))
}

func (r *ffsTestReader) dirEntries(ino uint32) map[string]uint32 {
	data := r.fileData(ino)
	out := map[string]uint32{}
	for off := 0; off+8 <= len(data); {
		entIno := binary.LittleEndian.Uint32(data[off : off+4])
		reclen := int(binary.LittleEndian.Uint16(data[off+4 : off+6]))
		namlen := int(data[off+7])
		if reclen <= 0 {
			break
		}
		if entIno != 0 && namlen > 0 {
			out[string(data[off+8:off+8+namlen])] = entIno
		}
		off += reclen
	}
	return out
}

func (r *ffsTestReader) inodeOff(ino uint32) int {
	cgx := ino / r.ipg
	local := ino % r.ipg
	return r.base + int((cgx*r.fpg+ffsIBlkNo)*uint32(r.fsize)+local*ffsInodeSize)
}

func (r *ffsTestReader) readAt(off, n int) []byte {
	r.t.Helper()
	buf := make([]byte, n)
	if _, err := r.data.ReadAt(buf, int64(off)); err != nil && err != io.EOF {
		r.t.Fatalf("read at %d: %v", off, err)
	}
	return buf
}

func splitTestPath(name string) []string {
	var out []string
	for _, part := range strings.Split(name, "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteMode(t *testing.T, path, contents string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

type lazyTestDir struct {
	entries map[string]imagefs.Entry
}

func (d lazyTestDir) Stat() fs.FileMode       { return fs.ModeDir | 0o755 }
func (d lazyTestDir) ModTime() time.Time      { return time.Unix(1234, 0) }
func (d lazyTestDir) Owner() (uint32, uint32) { return 0, 0 }
func (d lazyTestDir) RDev() uint32            { return 0 }
func (d lazyTestDir) ReadDir() ([]imagefs.DirEnt, error) {
	out := make([]imagefs.DirEnt, 0, len(d.entries))
	for name, entry := range d.entries {
		mode := fs.FileMode(0)
		if entry.Dir != nil {
			mode = fs.ModeDir | entry.Dir.Stat()
		} else if entry.Symlink != nil {
			mode = fs.ModeSymlink | entry.Symlink.Stat()
		} else if entry.File != nil {
			_, mode = entry.File.Stat()
		}
		out = append(out, imagefs.DirEnt{Name: name, Mode: mode})
	}
	return out, nil
}
func (d lazyTestDir) Lookup(name string) (imagefs.Entry, error) {
	entry, ok := d.entries[name]
	if !ok {
		return imagefs.Entry{}, os.ErrNotExist
	}
	return entry, nil
}

type lazyTestFile struct {
	data  []byte
	reads int
}

type patternTestFile struct {
	size uint64
}

func (f patternTestFile) Stat() (uint64, fs.FileMode) { return f.size, 0o644 }
func (f patternTestFile) ModTime() time.Time          { return time.Unix(1234, 0) }
func (f patternTestFile) Owner() (uint32, uint32)     { return 0, 0 }
func (f patternTestFile) RDev() uint32                { return 0 }
func (f patternTestFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	if off >= f.size {
		return nil, nil
	}
	end := off + uint64(size)
	if end > f.size {
		end = f.size
	}
	data := make([]byte, end-off)
	for i := range data {
		data[i] = patternByte(int(off) + i)
	}
	return data, nil
}

func patternByte(off int) byte {
	return byte((off * 37) + (off >> 8))
}

func (f *lazyTestFile) Stat() (uint64, fs.FileMode) { return uint64(len(f.data)), 0o644 }
func (f *lazyTestFile) ModTime() time.Time          { return time.Unix(1234, 0) }
func (f *lazyTestFile) Owner() (uint32, uint32)     { return 0, 0 }
func (f *lazyTestFile) RDev() uint32                { return 0 }
func (f *lazyTestFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	f.reads++
	if off >= uint64(len(f.data)) {
		return nil, nil
	}
	end := off + uint64(size)
	if end > uint64(len(f.data)) {
		end = uint64(len(f.data))
	}
	return append([]byte(nil), f.data[off:end]...), nil
}
