//go:build linux

package kvm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func RuntimeImageFS(image *oci.Image) virtio.FSBackend {
	if image == nil {
		return nil
	}
	return virtio.NewImageFS(image.RootFS, image.RootFSDir)
}

func RootFSImageEnabled() bool {
	return strings.TrimSpace(os.Getenv("CCX3_ROOTFS_IMAGE_TYPE")) != "" || strings.TrimSpace(os.Getenv("CCX3_ROOTFS_EXT4")) != ""
}

func RootFSImageType() (fsimage.Type, error) {
	if value := strings.TrimSpace(os.Getenv("CCX3_ROOTFS_IMAGE_TYPE")); value != "" {
		return fsimage.ParseType(value)
	}
	if strings.TrimSpace(os.Getenv("CCX3_ROOTFS_EXT4")) != "" {
		return fsimage.TypeExt4, nil
	}
	return "", fmt.Errorf("rootfs image type is not configured")
}

func BuildRootFSImage(ctx context.Context, root imagefs.Directory, typ fsimage.Type) ([]byte, error) {
	var buf bytes.Buffer
	if err := fsimage.Write(ctx, &buf, root, fsimage.Options{Type: typ}); err != nil {
		return nil, fmt.Errorf("build %s rootfs image: %w", typ, err)
	}
	return buf.Bytes(), nil
}

func RootFSImageConfigVars(typ fsimage.Type) []string {
	switch typ {
	case fsimage.TypeExt4:
		return []string{"CONFIG_EXT4_FS"}
	case fsimage.TypeVFAT:
		return []string{"CONFIG_FAT_FS", "CONFIG_VFAT_FS", "CONFIG_NLS_CODEPAGE_437", "CONFIG_NLS_ISO8859_1"}
	default:
		return nil
	}
}
