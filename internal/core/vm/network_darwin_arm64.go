//go:build darwin && arm64

package vm

import (
	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type darwinNetworkRuntime struct{ *networkRuntime }

func newDarwinARM64NetworkRuntime(id string, cfg *client.NetworkConfig) (*darwinNetworkRuntime, error) {
	common, err := newNetworkRuntime(networkDeviceConfig{ID: id, Config: cfg, Base: arm64vm.NetBase, Size: arm64vm.NetSize, IRQ: arm64vm.NetIRQ})
	if err != nil || common == nil {
		return nil, err
	}
	return &darwinNetworkRuntime{common}, nil
}
func (n *darwinNetworkRuntime) Close() error {
	if n == nil {
		return nil
	}
	return n.networkRuntime.Close()
}
func (n *darwinNetworkRuntime) guestInitConfig() *vmruntime.GuestNetworkConfig {
	if n == nil {
		return nil
	}
	return n.GuestInitConfig()
}

func darwinNetworkGuestAddress(n *darwinNetworkRuntime) string {
	if n == nil {
		return (&networkRuntime{}).GuestAddress()
	}
	return n.GuestAddress()
}

func darwinNetworkGuestCIDR(n *darwinNetworkRuntime) string {
	if n == nil {
		return (&networkRuntime{}).GuestCIDR()
	}
	return n.GuestCIDR()
}
