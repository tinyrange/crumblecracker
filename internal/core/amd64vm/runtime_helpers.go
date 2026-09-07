package amd64vm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

func BuildFSDevices(req RunRequest, trace io.Writer) ([]*virtio.FS, virtio.ShareMounter, error) {
	rootFSBackend := req.RootFS
	rootFSOwned := false
	if rootFSBackend == nil {
		if req.Image == nil {
			return nil, nil, fmt.Errorf("image or rootfs backend is required")
		}
		rootFSBackend = virtio.NewImageFS(req.Image.RootFS, req.Image.RootFSDir)
		rootFSOwned = true
	}
	shares := append([]virtio.ShareMount(nil), req.Mounts...)
	for i, share := range req.Shares {
		mount, err := BuildShareMount(i, share)
		if err != nil {
			var rootErr error
			if rootFSOwned {
				rootErr = vmruntime.CloseFSBackend(rootFSBackend)
			}
			return nil, nil, errors.Join(err, vmruntime.CloseShareMounts(shares), rootErr)
		}
		shares = append(shares, mount)
	}
	rootFSBackend = virtio.NewMountedFS(rootFSBackend, shares)
	rootFS, _ := rootFSBackend.(virtio.ShareMounter)

	devs := []*virtio.FS{
		newFSDevice(RootFSBase, RootFSIRQ, RootFSTag, rootFSBackend, trace),
	}
	if strings.TrimSpace(req.AMD64EmulatorPath) != "" {
		sourceDir := filepath.Dir(req.AMD64EmulatorPath)
		devs = append(devs, newFSDevice(ShareFSBase, ShareFSIRQ, EmulatorTag, virtio.NewImageFS(imagefs.NewHostFS(sourceDir, nil), sourceDir), trace))
	}
	virtio.AttachFSBackingUsageTracker(devs)
	return devs, rootFS, nil
}

func BuildShareMount(index int, share DirectoryShare) (virtio.ShareMount, error) {
	share, err := vmruntime.CanonicalDirectoryShare(share)
	if err != nil {
		return virtio.ShareMount{}, err
	}
	_, backend, err := buildShareBackend(index, share)
	if err != nil {
		return virtio.ShareMount{}, err
	}
	return virtio.ShareMount{
		GuestPath: share.Mount,
		Backend:   backend,
		Writable:  share.Writable,
		CacheMode: share.Cache,
	}, nil
}

func VirtioMMIODeviceArg(base uint64, irq uint32) string {
	return fmt.Sprintf("virtio_mmio.device=4k@0x%x:%d", base, irq)
}

func VirtioFSCommandLineArgs(fsdevs []*virtio.FS) []string {
	args := make([]string, 0, len(fsdevs))
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		args = append(args, VirtioMMIODeviceArg(fsdev.Base, fsdev.IRQ))
	}
	return args
}

func newFSDevice(base uint64, irq uint32, tag string, backend virtio.FSBackend, trace io.Writer) *virtio.FS {
	fsdev := virtio.NewFS(base, RootFSSize, irq, tag, backend)
	if trace == nil && strings.TrimSpace(os.Getenv("CCX3_DEBUG_VIRTIOFS")) != "" {
		trace = os.Stderr
	}
	fsdev.Log = trace
	fsdev.Strict = true
	return fsdev
}

func buildShareBackend(index int, share DirectoryShare) (string, virtio.FSBackend, error) {
	source := strings.TrimSpace(share.Source)
	if source == "" {
		return "", nil, fmt.Errorf("share %d: source is required", index)
	}
	mount := strings.TrimSpace(share.Mount)
	if mount == "" || !strings.HasPrefix(mount, "/") {
		return "", nil, fmt.Errorf("share %d: mount must be an absolute guest path", index)
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", nil, fmt.Errorf("share %d: stat source: %w", index, err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("share %d: source must be a directory", index)
	}
	tag := fmt.Sprintf("share%d", index)
	if share.Writable {
		if share.MapOwner {
			return tag, virtio.NewPassthroughFSWithOwner(source, nil, share.OwnerUID, share.OwnerGID), nil
		}
		return tag, virtio.NewPassthroughFS(source, nil), nil
	}
	if share.MapOwner {
		return tag, virtio.NewImageFSWithOwner(imagefs.NewHostFS(source, nil), source, share.OwnerUID, share.OwnerGID), nil
	}
	return tag, virtio.NewImageFS(imagefs.NewHostFS(source, nil), source), nil
}
