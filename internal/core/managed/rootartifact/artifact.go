package rootartifact

import (
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

type Artifact struct {
	Kernel      []byte
	Initrd      []byte
	RootBlock   virtio.BlockBackend
	RootFS      imagefs.Directory
	ExtraBlocks []virtio.BlockBackend
	Metadata    map[string]string
	Cleanup     func() error
}

func (a Artifact) Close() error {
	if a.Cleanup == nil {
		return nil
	}
	return a.Cleanup()
}
