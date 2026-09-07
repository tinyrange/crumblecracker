package ext4image

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/ext4image/ext4"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) {
	return f(p)
}

type readDirErrorDirectory struct {
	imagefs.Directory
	err error
}

func (d readDirErrorDirectory) ReadDir() ([]imagefs.DirEnt, error) {
	return nil, d.err
}

func TestWriteBuildsExt4Image(t *testing.T) {
	overlay := imagefs.NewOverlay(nil)
	if err := overlay.AddFile("/etc/issue", 0o644, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := overlay.AddSymlink("/etc/issue.link", "issue"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, overlay.Root(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := buf.Len(); got != defaultImageSize {
		t.Fatalf("image size = %d, want %d", got, defaultImageSize)
	}
	if got := buf.Bytes()[1024+56 : 1024+58]; got[0] != 0x53 || got[1] != 0xef {
		t.Fatalf("ext4 magic = % x, want 53 ef", got)
	}
}

func TestBuildRejectsFilesBeyondInlineExtentLimit(t *testing.T) {
	file := ext4LimitTestFile{size: maxSupportedFileSize() + 1}
	root := ext4LimitTestDir{file: file}
	image, err := Build(context.Background(), root, Options{})
	if image != nil {
		t.Fatal("unsupported ext4 layout published an image")
	}
	var limitErr *ext4.ExtentLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("oversized build error = %v, want ExtentLimitError", err)
	}
	if limitErr.RequiredExtents != ext4.MaxInlineExtents+1 || limitErr.SupportedExtents != ext4.MaxInlineExtents {
		t.Fatalf("extent limit error = %#v", limitErr)
	}

	overlay := imagefs.NewOverlay(nil)
	if err := overlay.AddFile("/ok", 0o644, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), overlay.Root(), Options{}); err != nil {
		t.Fatalf("build after layout rejection: %v", err)
	}
}

type ext4LimitTestDir struct {
	file ext4LimitTestFile
}

func (d ext4LimitTestDir) Stat() fs.FileMode       { return fs.ModeDir | 0o755 }
func (d ext4LimitTestDir) ModTime() time.Time      { return time.Unix(0, 0) }
func (d ext4LimitTestDir) Owner() (uint32, uint32) { return 0, 0 }
func (d ext4LimitTestDir) RDev() uint32            { return 0 }
func (d ext4LimitTestDir) ReadDir() ([]imagefs.DirEnt, error) {
	return []imagefs.DirEnt{{Name: "large", Mode: 0o644}}, nil
}
func (d ext4LimitTestDir) Lookup(name string) (imagefs.Entry, error) {
	if name != "large" {
		return imagefs.Entry{}, fs.ErrNotExist
	}
	return imagefs.Entry{File: d.file}, nil
}

type ext4LimitTestFile struct {
	size uint64
}

func (f ext4LimitTestFile) Stat() (uint64, fs.FileMode) { return f.size, 0o644 }
func (f ext4LimitTestFile) ModTime() time.Time          { return time.Unix(0, 0) }
func (f ext4LimitTestFile) Owner() (uint32, uint32)     { return 0, 0 }
func (f ext4LimitTestFile) RDev() uint32                { return 0 }
func (f ext4LimitTestFile) ReadAt(uint64, uint32) ([]byte, error) {
	return nil, errors.New("oversized test file should not be read")
}

func TestWriteReturnsOutputFailure(t *testing.T) {
	backingErr := errors.New("output unavailable")
	err := Write(context.Background(), writeFunc(func([]byte) (int, error) {
		return 0, backingErr
	}), imagefs.NewOverlay(nil).Root(), Options{})
	if !errors.Is(err, backingErr) {
		t.Fatalf("Write error = %v, want wrapped %v", err, backingErr)
	}
}

func TestWriteFilePreservesDestinationAndPrimaryError(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "root.ext4")
	want := []byte("existing image")
	if err := os.WriteFile(destination, want, 0o644); err != nil {
		t.Fatalf("write existing image: %v", err)
	}
	primaryErr := errors.New("source filesystem unavailable")
	err := WriteFile(context.Background(), destination, readDirErrorDirectory{err: primaryErr}, Options{})
	if !errors.Is(err, primaryErr) {
		t.Fatalf("WriteFile error = %v, want wrapped %v", err, primaryErr)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read existing image: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination bytes = %q, want %q", got, want)
	}
	if _, err := os.Stat(destination + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary image remains: %v", err)
	}
}
