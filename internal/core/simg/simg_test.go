package simg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

func TestOpenFixtureLocatesSquashFS(t *testing.T) {
	img, err := Open(alpineFixture(t))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer img.Close()

	if img.Size == 0 {
		t.Fatalf("image size was not populated")
	}
	if img.SquashFSOffset <= 0 {
		t.Fatalf("SquashFSOffset = %d, want positive offset", img.SquashFSOffset)
	}
	if img.SIF == nil {
		t.Fatalf("fixture did not parse as SIF")
	}
	if img.SIFArch() != "amd64" {
		t.Fatalf("SIF arch = %q, want amd64", img.SIFArch())
	}
}

func TestBuildImageFSFixture(t *testing.T) {
	root, meta, arch, err := BuildImageFS(alpineFixture(t))
	if err != nil {
		t.Fatalf("build image fs: %v", err)
	}
	if arch != "amd64" {
		t.Fatalf("arch = %q, want amd64", arch)
	}
	if _, ok := meta["/"]; !ok {
		t.Fatalf("metadata is missing root entry")
	}
	entry, err := imagefs.LookupPath(root, "/etc/alpine-release")
	if err != nil {
		t.Fatalf("lookup alpine release: %v", err)
	}
	if entry.File == nil {
		t.Fatalf("alpine release entry is not a file")
	}
}

func TestBuildImageFSArm64Fixture(t *testing.T) {
	root, meta, arch, err := BuildImageFS(alpineArm64Fixture(t))
	if err != nil {
		t.Fatalf("build image fs: %v", err)
	}
	if arch != "arm64" {
		t.Fatalf("arch = %q, want arm64", arch)
	}
	if _, ok := meta["/bin/busybox"]; !ok {
		t.Fatalf("metadata is missing busybox entry")
	}
	entry, err := imagefs.LookupPath(root, "/etc/alpine-release")
	if err != nil {
		t.Fatalf("lookup alpine release: %v", err)
	}
	if entry.File == nil {
		t.Fatalf("alpine release entry is not a file")
	}
}

func TestOpenRejectsInvalidImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-simg")
	if err := os.WriteFile(path, []byte(strings.Repeat("not-squashfs", 1024)), 0o644); err != nil {
		t.Fatalf("write invalid image: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatalf("invalid image opened successfully")
	}
}

func alpineFixture(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "alpine.simg")
}

func alpineArm64Fixture(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "alpine-arm64.simg")
}
