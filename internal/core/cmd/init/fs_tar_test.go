//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/managed/guestagent"
)

func TestFSTarRoundTripPreservesMetadataAndSymlink(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("make src: %v", err)
	}
	fileMtime := time.Unix(1700000000, 0)
	dirMtime := time.Unix(1700000500, 0)
	script := filepath.Join(src, "script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatalf("chmod script: %v", err)
	}
	if err := os.Chtimes(script, fileMtime, fileMtime); err != nil {
		t.Fatalf("chtime script: %v", err)
	}
	if err := os.Symlink("script.sh", filepath.Join(src, "script-link")); err != nil {
		t.Fatalf("symlink script: %v", err)
	}
	if err := os.Chtimes(src, dirMtime, dirMtime); err != nil {
		t.Fatalf("chtime src dir: %v", err)
	}

	info, err := os.Lstat(src)
	if err != nil {
		t.Fatalf("lstat src: %v", err)
	}
	var archive bytes.Buffer
	if err := guestagent.WritePathTar(&archive, src, filepath.Base(src), info); err != nil {
		t.Fatalf("write tar: %v", err)
	}
	assertTarSymlink(t, archive.Bytes(), "src/script-link", "script.sh")

	dst := filepath.Join(parent, "dst")
	if err := guestagent.ExtractTarToPath(bytes.NewReader(archive.Bytes()), "", dst, false); err != nil {
		t.Fatalf("extract tar: %v", err)
	}
	copied := filepath.Join(dst, "script.sh")
	copiedInfo, err := os.Stat(copied)
	if err != nil {
		t.Fatalf("stat copied script: %v", err)
	}
	if got := copiedInfo.Mode().Perm(); got != 0o755 {
		t.Fatalf("copied mode = %#o, want 0755", got)
	}
	if got := copiedInfo.ModTime().Unix(); got != fileMtime.Unix() {
		t.Fatalf("copied mtime = %d, want %d", got, fileMtime.Unix())
	}
	linkInfo, err := os.Lstat(filepath.Join(dst, "script-link"))
	if err != nil {
		t.Fatalf("lstat copied link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("copied link mode = %v, want symlink", linkInfo.Mode())
	}
	if target, err := os.Readlink(filepath.Join(dst, "script-link")); err != nil || target != "script.sh" {
		t.Fatalf("copied symlink target = %q err=%v, want script.sh", target, err)
	}
	dirInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat copied dir: %v", err)
	}
	if got := dirInfo.ModTime().Unix(); got != dirMtime.Unix() {
		t.Fatalf("copied dir mtime = %d, want %d", got, dirMtime.Unix())
	}
}

func assertTarSymlink(t *testing.T, data []byte, name, target string) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if err != nil {
			t.Fatalf("tar entry %s not found: %v", name, err)
		}
		if header.Name != name {
			continue
		}
		if header.Typeflag != tar.TypeSymlink || header.Linkname != target {
			t.Fatalf("tar symlink %s = type %d target %q, want symlink target %q", name, header.Typeflag, header.Linkname, target)
		}
		return
	}
}
