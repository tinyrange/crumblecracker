//go:build windows && arm64

package vm

import (
	"context"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/vm/mounts"
)

func (i *windowsInstance) Flush(ctx context.Context) error {
	return i.core().Flush(ctx)
}

func (i *windowsInstance) RootSnapshot() (imagefs.Directory, error) {
	return i.RootSnapshotContext(context.Background())
}

func (i *windowsInstance) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	if i == nil || i.rootFS == nil {
		return mounts.RootSnapshot(nil, "")
	}
	if !i.ManagedCapabilities().RootSnapshot {
		return mounts.RootSnapshotWithCapabilities("Linux", i.ManagedCapabilities(), i.rootFS, "")
	}
	return mounts.RootSnapshotContext(ctx, i.rootFS, "")
}

func (i *windowsInstance) SnapshotImage(imageName string) (imagefs.Directory, error) {
	return i.SnapshotImageContext(context.Background(), imageName)
}

func (i *windowsInstance) SnapshotImageContext(ctx context.Context, imageName string) (imagefs.Directory, error) {
	if i == nil || i.rootFS == nil {
		return mounts.RootSnapshot(nil, "")
	}
	if i.image != nil && i.image.Name == imageName {
		return i.RootSnapshotContext(ctx)
	}
	return mounts.ImageSnapshotContextWithCapabilities(ctx, "Linux", i.ManagedCapabilities(), i.rootFS, imageName, windowsImageMountPath(imageName))
}
