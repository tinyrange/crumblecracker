//go:build !darwin && ((!linux && !windows) || !amd64)

package virgl

import (
	"errors"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func ProbeHostRenderer() error {
	return errors.New("VirGL host rendering is available on Darwin, Linux/amd64 and Windows/amd64")
}

func NewHostRenderer() (virtio.GPURenderer, error) {
	return nil, ProbeHostRenderer()
}

func NewHostRendererWithShareGroup(uintptr, uintptr) (virtio.GPURenderer, error) {
	return NewHostRenderer()
}
