//go:build linux && amd64

package vm

import (
	"context"
	"fmt"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	kvmhost "github.com/tinyrange/crumblecracker/internal/core/vm/host/kvm"
	"github.com/tinyrange/crumblecracker/internal/core/vm/mounts"
)

func (i *linuxInstance) Flush(ctx context.Context) error {
	if i == nil || i.managedInstance == nil {
		return fmt.Errorf("instance is not running")
	}
	return i.managedInstance.Flush(ctx)
}

func (i *linuxInstance) RootSnapshot() (imagefs.Directory, error) {
	return i.RootSnapshotContext(context.Background())
}

func (i *linuxInstance) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	if i == nil || i.rootFS == nil {
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	if i.managedInstance == nil {
		return nil, fmt.Errorf("instance is not running")
	}
	if !i.caps.RootSnapshot {
		return mounts.RootSnapshotWithCapabilities("Linux", i.caps, i.rootFS, i.defaultRootDir)
	}
	return mounts.RootSnapshotContext(ctx, i.rootFS, i.defaultRootDir)
}

func (i *linuxInstance) SnapshotImage(imageName string) (imagefs.Directory, error) {
	return i.SnapshotImageContext(context.Background(), imageName)
}

func (i *linuxInstance) SnapshotImageContext(ctx context.Context, imageName string) (imagefs.Directory, error) {
	if i == nil || i.rootFS == nil {
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	if i.managedInstance == nil {
		return nil, fmt.Errorf("instance is not running")
	}
	if i.image != nil && i.image.Name == imageName {
		if i.defaultRootDir != "" {
			return mounts.RootSnapshotContext(ctx, i.rootFS, i.defaultRootDir)
		}
		return i.RootSnapshotContext(ctx)
	}
	return mounts.ImageSnapshotContextWithCapabilities(ctx, "Linux", i.caps, i.rootFS, imageName, kvmhost.ImageMountPath(imageName))
}
