package vm

import "github.com/tinyrange/crumblecracker/internal/core/virtio"

func closeVirtioFSDevices(devices []*virtio.FS) {
	for _, device := range devices {
		if device != nil {
			_ = device.Close()
		}
	}
}
