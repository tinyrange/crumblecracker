//go:build linux

package kvm

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage"
)

func TestRootFSImageTypeEnv(t *testing.T) {
	t.Setenv("CCX3_ROOTFS_IMAGE_TYPE", "ext4")
	typ, err := RootFSImageType()
	if err != nil {
		t.Fatal(err)
	}
	if typ != fsimage.TypeExt4 {
		t.Fatalf("rootfs image type = %q, want %q", typ, fsimage.TypeExt4)
	}
}

func TestRootFSImageTypeLegacyExt4Env(t *testing.T) {
	t.Setenv("CCX3_ROOTFS_EXT4", "1")
	typ, err := RootFSImageType()
	if err != nil {
		t.Fatal(err)
	}
	if typ != fsimage.TypeExt4 {
		t.Fatalf("rootfs image type = %q, want %q", typ, fsimage.TypeExt4)
	}
}

func TestRootFSImageTypeAcceptsPlannedType(t *testing.T) {
	t.Setenv("CCX3_ROOTFS_IMAGE_TYPE", "vfat")
	typ, err := RootFSImageType()
	if err != nil {
		t.Fatal(err)
	}
	if typ != fsimage.TypeVFAT {
		t.Fatalf("rootfs image type = %q, want %q", typ, fsimage.TypeVFAT)
	}
}

func TestRootFSImageTypeRejectsUnknown(t *testing.T) {
	t.Setenv("CCX3_ROOTFS_IMAGE_TYPE", "definitely-not-a-filesystem")
	if _, err := RootFSImageType(); err == nil {
		t.Fatal("RootFSImageType accepted an unknown type")
	}
}

func TestRootFSImageConfigVars(t *testing.T) {
	got := RootFSImageConfigVars(fsimage.TypeExt4)
	if len(got) != 1 || got[0] != "CONFIG_EXT4_FS" {
		t.Fatalf("ext4 config vars = %#v, want CONFIG_EXT4_FS", got)
	}
	got = RootFSImageConfigVars(fsimage.TypeVFAT)
	want := []string{"CONFIG_FAT_FS", "CONFIG_VFAT_FS", "CONFIG_NLS_CODEPAGE_437", "CONFIG_NLS_ISO8859_1"}
	if len(got) != len(want) {
		t.Fatalf("vfat config vars = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("vfat config vars = %#v, want %#v", got, want)
		}
	}
}
