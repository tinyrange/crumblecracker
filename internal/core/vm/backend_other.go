//go:build (!darwin || !arm64) && (!linux || (!arm64 && !amd64)) && (!windows || (!amd64 && !arm64))

package vm

import (
	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	vmhost "github.com/tinyrange/crumblecracker/internal/core/vm/host"
)

func NewRuntimeBackend(kernel *alpine.Manager, images *oci.Store, guestInitCache string) Backend {
	_ = kernel
	_ = images
	_ = guestInitCache
	return vmhost.UnsupportedBackend{}
}
