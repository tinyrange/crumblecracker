//go:build windows && amd64

package vm

import (
	"net"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type windowsNetworkRuntime struct {
	*networkRuntime
}

func newWindowsAMD64NetworkRuntime(cfg *client.NetworkConfig) (*windowsNetworkRuntime, error) {
	common, err := newNetworkRuntime(networkDeviceConfig{
		Config: cfg,
		IP:     net.IPv4(10, 42, 0, 2),
		MAC:    net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02},
		Base:   amd64vm.NetBase,
		Size:   amd64vm.NetSize,
		IRQ:    amd64vm.NetIRQ,
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

func windowsNetworkGuestAddress(network *windowsNetworkRuntime) string {
	if network == nil {
		return (&networkRuntime{}).GuestAddress()
	}
	return network.GuestAddress()
}

func windowsNetworkGuestInitConfig(network *windowsNetworkRuntime) *vmruntime.GuestNetworkConfig {
	if network == nil {
		return nil
	}
	return network.GuestInitConfig()
}
