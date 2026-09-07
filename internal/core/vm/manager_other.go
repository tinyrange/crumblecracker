//go:build !darwin || !arm64

package vm

import (
	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
)

func NewRuntimeManager(kernel *alpine.Manager, images *oci.Store, guestInitCache string, openGLShareGroup func() (context, pixelFormat uintptr)) *Manager {
	backend := NewRuntimeBackend(kernel, images, guestInitCache)
	return NewManagerWithBackend(backend)
}
