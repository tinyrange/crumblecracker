//go:build !darwin && (!linux || !amd64)

package virgl

import (
	"errors"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func NewHostRenderer() (virtio.GPURenderer, error) {
	return nil, errors.New("first-party VirGL host rendering is currently available on Darwin and Linux/amd64 only")
}

func NewHostRendererWithShareGroup(uintptr, uintptr) (virtio.GPURenderer, error) {
	return NewHostRenderer()
}
