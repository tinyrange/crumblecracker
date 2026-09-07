package fsimage

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

func TestWriteDispatchesExt4(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "issue"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, imagefs.NewHostFS(root, nil), Options{Type: TypeExt4}); err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes()[1024+56 : 1024+58]; got[0] != 0x53 || got[1] != 0xef {
		t.Fatalf("ext4 magic = % x, want 53 ef", got)
	}
}

func TestWriteDispatchesVFAT(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI"), []byte("mock efi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, imagefs.NewHostFS(root, nil), Options{Type: TypeVFAT}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if len(data) != defaultFATSize {
		t.Fatalf("image size = %d, want %d", len(data), defaultFATSize)
	}
	if data[510] != 0x55 || data[511] != 0xaa {
		t.Fatalf("FAT boot signature = % x, want 55 aa", data[510:512])
	}
	if got := string(data[82:90]); got != "FAT32   " {
		t.Fatalf("FAT system identifier = %q, want FAT32", got)
	}
}

func TestWriteVFATPassesFsckIfInstalled(t *testing.T) {
	fsck, err := exec.LookPath("fsck.fat")
	if err != nil {
		fsck, err = exec.LookPath("dosfsck")
		if err != nil {
			t.Skip("fsck.fat/dosfsck not installed; skipping")
		}
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI"), []byte("mock efi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(t.TempDir(), "rootfs.vfat")
	if err := WriteFile(context.Background(), img, imagefs.NewHostFS(root, nil), Options{Type: TypeVFAT}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fsck, "-n", img)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck.fat failed: %v\n%s", err, string(out))
	}
}

func TestParseTypeAcceptsPlannedFormats(t *testing.T) {
	for _, value := range []string{"ext4", "vfat", "ffs", "iso9660"} {
		if _, err := ParseType(value); err != nil {
			t.Fatalf("ParseType(%q): %v", value, err)
		}
	}
}

func TestWriteRejectsUnimplementedWriter(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	err := Write(context.Background(), &buf, imagefs.NewHostFS(root, nil), Options{Type: TypeISO9660})
	if err == nil {
		t.Fatal("Write(iso9660) succeeded before an iso9660 writer exists")
	}
}
