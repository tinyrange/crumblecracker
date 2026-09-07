package cvmfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestAtomicCachePublication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	target := filepath.Join(root, "objects", "aa", "bb", "object")
	oldData := []byte("complete-old-entry")
	newData := []byte("complete-new-entry")
	if err := writeAtomicFile(target, func(dst io.Writer) error {
		_, err := dst.Write(oldData)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	written := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- writeAtomicFile(target, func(dst io.Writer) error {
			if _, err := dst.Write(newData[:5]); err != nil {
				return err
			}
			close(written)
			<-release
			_, err := dst.Write(newData[5:])
			return err
		})
	}()
	<-written

	data, err := readPrivateCacheFile(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, oldData) {
		t.Fatalf("data visible before commit = %q, want %q", data, oldData)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data, err = readPrivateCacheFile(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, newData) {
		t.Fatalf("published data = %q, want %q", data, newData)
	}
}

func TestFailedCacheWritePreservesPublishedEntry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	target := filepath.Join(root, "state", "repo", "manifest")
	want := []byte("Cpublished")
	if err := writeAtomicFile(target, func(dst io.Writer) error {
		_, err := dst.Write(want)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("download interrupted")
	err := writeAtomicFile(target, func(dst io.Writer) error {
		_, _ = dst.Write([]byte("Cpartial"))
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("write error = %v, want %v", err, wantErr)
	}
	data, err := readPrivateCacheFile(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("data after failed write = %q, want %q", data, want)
	}
}

func TestCachePublicationSerializesWritersPerEntry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	target := filepath.Join(root, "files", "repo", "aa", "bb", "file")
	var active atomic.Int32
	var maximum atomic.Int32
	start := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(value byte) {
			<-start
			done <- writeAtomicFile(target, func(dst io.Writer) error {
				current := active.Add(1)
				defer active.Add(-1)
				for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
				}
				_, err := dst.Write(bytes.Repeat([]byte{value}, 64*1024))
				return err
			})
		}(byte('a' + i))
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent writers = %d, want 1", got)
	}
	data, err := readPrivateCacheFile(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 64*1024 || (data[0] != 'a' && data[0] != 'b') || !bytes.Equal(data, bytes.Repeat(data[:1], len(data))) {
		t.Fatal("published entry contains interleaved or incomplete writer data")
	}
}

func TestCacheStateIsOwnerPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cache privacy is represented by ACLs, not Unix mode bits")
	}
	root := filepath.Join(t.TempDir(), "cache")
	target := cvmfsFileCachePath(root, "repo", "/file")
	if err := writeAtomicFile(target, func(dst io.Writer) error {
		_, err := dst.Write([]byte("Cmanifest"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "files"), filepath.Dir(target)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory %q mode = %04o, want 0700", path, got)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("entry mode = %04o, want 0600", got)
	}

	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.CacheDir = root
	if _, _, err := client.CachedFileSize("cvmfs://repo/file"); err == nil {
		t.Fatal("public cache entry was accepted")
	}
}

func TestCacheReaderRejectsSymlinkEntry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	target := filepath.Join(root, "objects", "aa", "bb", "object")
	if err := ensurePrivateCacheDir(root, filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("attacker-controlled"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateCacheFile(root, target); err == nil {
		t.Fatal("symlink cache entry was accepted")
	}
}
