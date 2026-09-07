//go:build windows && arm64

package vm

import (
	"net"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type windowsNetworkRuntime struct {
	*networkRuntime
}

func newWindowsARM64NetworkRuntime(cfg *client.NetworkConfig) (*windowsNetworkRuntime, error) {
	common, err := newNetworkRuntime(networkDeviceConfig{
		Config: cfg,
		IP:     net.IPv4(10, 42, 0, 2),
		MAC:    net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02},
		Base:   arm64vm.NetBase,
		Size:   arm64vm.NetSize,
		IRQ:    arm64vm.NetIRQ,
	})
	if err != nil || common == nil {
		return nil, err
	}
	return &windowsNetworkRuntime{networkRuntime: common}, nil
}

func windowsNetworkDevice(network *windowsNetworkRuntime) *virtio.Net {
	if network == nil {
		return nil
	}
	return network.Device()
}

func windowsNetworkGuestInitConfig(network *windowsNetworkRuntime) *vmruntime.GuestNetworkConfig {
	if network == nil {
		return nil
	}
	return network.GuestInitConfig()
}
