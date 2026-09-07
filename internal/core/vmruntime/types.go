package vmruntime

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

const (
	// BusyBox treats a mount source named exactly "rootfs" as the kernel's
	// synthetic root and omits it from targeted df lookups. Use a distinct
	// virtio-fs tag so ordinary disk-space tools recognize the mounted guest
	// filesystem.
	RootFSTag     = "vmsh-rootfs"
	EmulatorTag   = "ccx3"
	GuestCID      = 3
	ControlPort   = 10777
	ClipboardPort = 10778
	DisplayPort   = 10779
)

// DirectoryShare describes a host directory exposed inside the guest.
type DirectoryShare struct {
	Source   string
	Mount    string
	Writable bool
	MapOwner bool
	OwnerUID uint32
	OwnerGID uint32
	Cache    string
}

func CanonicalDirectoryShare(share DirectoryShare) (DirectoryShare, error) {
	mount := strings.TrimSpace(share.Mount)
	if mount == "" {
		return DirectoryShare{}, fmt.Errorf("share mount path is required")
	}
	if !strings.HasPrefix(mount, "/") {
		return DirectoryShare{}, fmt.Errorf("share mount path %q must be absolute", mount)
	}
	mount = path.Clean(mount)
	if mount == "/" {
		return DirectoryShare{}, fmt.Errorf("share mount path / cannot replace the VM root filesystem")
	}
	share.Mount = mount
	return share, nil
}

func CloseShareMounts(shares []virtio.ShareMount) error {
	var errs []error
	for _, share := range shares {
		if closer, ok := share.Backend.(interface{ Close() error }); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

func CloseFSBackend(backend virtio.FSBackend) error {
	if closer, ok := backend.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// RunRequest is the backend-neutral request shape for the managed guest runtime.
type RunRequest struct {
	Kernel                 []byte
	KernelRelease          string
	ModuleSymvers          []byte
	Init                   []byte
	AMD64EmulatorPath      string
	Modules                []alpine.Module
	Image                  *oci.Image
	InitSystem             string
	RootFS                 virtio.FSBackend
	Shares                 []DirectoryShare
	Mounts                 []virtio.ShareMount
	Command                []string
	Env                    []string
	WorkDir                string
	User                   string
	MemoryMB               uint64
	BalloonMB              uint64
	CPUs                   int
	NestedVirt             bool
	Dmesg                  bool
	Persistent             bool
	Network                *GuestNetworkConfig
	NetDevice              *virtio.Net
	DisplayWidth           uint32
	DisplayHeight          uint32
	Accelerated3D          bool
	OpenGLShareContext     uintptr
	OpenGLSharePixelFormat uintptr
	SnapshotDir            string
	RestoreSnapshot        string
	UnixTime               int64
}

// RunResult is the backend-neutral result shape for one-shot guest execution.
type RunResult struct {
	ExitCode   int
	Output     string
	Transcript string
}
