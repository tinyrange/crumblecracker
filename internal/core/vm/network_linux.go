//go:build linux

package vm

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type linuxNetworkRuntime struct {
	*networkRuntime
	switchNet *linuxVirtualSwitch
	rxQueue   chan []byte
	done      chan struct{}
	closeOnce sync.Once
	worker    sync.WaitGroup
	dropped   atomic.Uint64
}

func newLinuxAMD64NetworkRuntime(id string, cfg *client.NetworkConfig, switches ...*linuxVirtualSwitch) (*linuxNetworkRuntime, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}
	return newLinuxSwitchNetworkRuntimeOn(selectLinuxSwitch(switches), id, cfg, amd64vm.NetBase, amd64vm.NetSize, amd64vm.NetIRQ)
}

func newLinuxARM64NetworkRuntime(id string, cfg *client.NetworkConfig, switches ...*linuxVirtualSwitch) (*linuxNetworkRuntime, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}
	return newLinuxSwitchNetworkRuntimeOn(selectLinuxSwitch(switches), id, cfg, arm64vm.NetBase, arm64vm.NetSize, arm64vm.NetIRQ)
}

func newLinuxPCINetworkRuntime(id string, cfg *client.NetworkConfig, switches ...*linuxVirtualSwitch) (*linuxNetworkRuntime, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}
	return newLinuxSwitchNetworkRuntimeOn(selectLinuxSwitch(switches), id, cfg, 0, 0x1000, 11)
}

func selectLinuxSwitch(switches []*linuxVirtualSwitch) *linuxVirtualSwitch {
	if len(switches) != 0 && switches[0] != nil {
		return switches[0]
	}
	return defaultLinuxVirtualSwitch
}

func newLinuxSwitchNetworkRuntime(id string, cfg *client.NetworkConfig, base, size uint64, irq uint32) (*linuxNetworkRuntime, error) {
	return newLinuxSwitchNetworkRuntimeOn(newLinuxVirtualSwitch(), id, cfg, base, size, irq)
}

func newLinuxSwitchNetworkRuntimeOn(switchNet *linuxVirtualSwitch, id string, cfg *client.NetworkConfig, base, size uint64, irq uint32) (*linuxNetworkRuntime, error) {
	lease, err := switchNet.Register(id, cfg)
	if err != nil {
		return nil, err
	}
	runtime := &linuxNetworkRuntime{switchNet: switchNet, rxQueue: make(chan []byte, 256), done: make(chan struct{})}
	common, err := newNetworkRuntime(networkDeviceConfig{
		ID:     lease.id,
		Config: cfg,
		IP:     lease.ip,
		MAC:    lease.mac,
		Base:   base,
		Size:   size,
		IRQ:    irq,
		TXHook: func(packet []byte) bool {
			return switchNet.Forward(runtime, packet)
		},
		Cleanup: func() {
			switchNet.Unregister(lease.id)
		},
	})
	if err != nil {
		return nil, err
	}
	runtime.networkRuntime = common
	switchNet.Attach(runtime)
	runtime.worker.Add(1)
	go runtime.runRX()
	return runtime, nil
}

func (n *linuxNetworkRuntime) Close() error {
	if n == nil {
		return nil
	}
	if n.switchNet != nil {
		n.switchNet.Unregister(n.id)
		n.switchNet = nil
	}
	n.closeOnce.Do(func() { close(n.done); n.worker.Wait() })
	if n.networkRuntime == nil {
		return nil
	}
	return n.networkRuntime.Close()
}

func networkDevice(n *linuxNetworkRuntime) *virtio.Net {
	if n == nil {
		return nil
	}
	return n.Device()
}

func networkGuestAddress(n *linuxNetworkRuntime) string {
	if n == nil {
		return (&networkRuntime{}).GuestAddress()
	}
	return n.GuestAddress()
}

func networkGuestCIDR(n *linuxNetworkRuntime) string {
	if n == nil {
		return (&networkRuntime{}).GuestCIDR()
	}
	return n.GuestCIDR()
}

type linuxVirtualSwitch struct {
	mu        sync.Mutex
	leases    map[string]linuxNetworkLease
	endpoints map[string]*linuxNetworkRuntime
}

type linuxNetworkLease struct {
	id  string
	ip  net.IP
	mac net.HardwareAddr
}

func newLinuxVirtualSwitch() *linuxVirtualSwitch {
	return &linuxVirtualSwitch{leases: make(map[string]linuxNetworkLease), endpoints: make(map[string]*linuxNetworkRuntime)}
}

var defaultLinuxVirtualSwitch = newLinuxVirtualSwitch()

func (s *linuxVirtualSwitch) Register(id string, cfg ...*client.NetworkConfig) (linuxNetworkLease, error) {
	if s == nil {
		return linuxNetworkLease{}, fmt.Errorf("network switch is required")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultInstanceID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var requested *client.NetworkConfig
	if len(cfg) != 0 {
		requested = cfg[0]
	}
	if requested != nil && (strings.TrimSpace(requested.GuestIPv4) != "" || strings.TrimSpace(requested.GuestMAC) != "") {
		ip := net.ParseIP(strings.TrimSpace(requested.GuestIPv4)).To4()
		mac, err := net.ParseMAC(strings.TrimSpace(requested.GuestMAC))
		if ip == nil || err != nil || len(mac) != 6 {
			return linuxNetworkLease{}, fmt.Errorf("explicit network identity requires valid guest_ipv4 and guest_mac")
		}
		for otherID, lease := range s.leases {
			if otherID != id && (lease.ip.Equal(ip) || bytesEqualMAC(lease.mac, mac)) {
				return linuxNetworkLease{}, fmt.Errorf("network identity conflicts with VM %q", otherID)
			}
		}
		for otherID, endpoint := range s.endpoints {
			if otherID != id && endpoint != nil && (endpoint.ip.Equal(ip) || bytesEqualMAC(endpoint.mac, mac)) {
				return linuxNetworkLease{}, fmt.Errorf("network identity conflicts with VM %q", otherID)
			}
		}
		lease := linuxNetworkLease{id: id, ip: append(net.IP(nil), ip...), mac: append(net.HardwareAddr(nil), mac...)}
		s.leases[id] = lease
		return lease, nil
	}

	used := map[byte]bool{1: true}
	for _, lease := range s.leases {
		if ip4 := lease.ip.To4(); ip4 != nil {
			used[ip4[3]] = true
		}
	}
	for _, endpoint := range s.endpoints {
		if endpoint != nil && endpoint.ip != nil {
			if ip4 := endpoint.ip.To4(); ip4 != nil {
				used[ip4[3]] = true
			}
		}
	}
	host := byte(2)
	for ; host <= 254; host++ {
		if !used[host] {
			break
		}
	}
	lease := linuxNetworkLease{
		id:  id,
		ip:  net.IPv4(10, 42, 0, host),
		mac: net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, host},
	}
	if s.leases == nil {
		s.leases = make(map[string]linuxNetworkLease)
	}
	s.leases[id] = lease
	return lease, nil
}

func (s *linuxVirtualSwitch) Attach(endpoint *linuxNetworkRuntime) {
	if s == nil || endpoint == nil {
		return
	}
	s.mu.Lock()
	if s.endpoints == nil {
		s.endpoints = make(map[string]*linuxNetworkRuntime)
	}
	delete(s.leases, endpoint.id)
	s.endpoints[endpoint.id] = endpoint
	s.mu.Unlock()
}

func (s *linuxVirtualSwitch) Unregister(id string) {
	if s == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultInstanceID
	}
	s.mu.Lock()
	delete(s.leases, id)
	delete(s.endpoints, id)
	s.mu.Unlock()
}

func (s *linuxVirtualSwitch) Forward(source *linuxNetworkRuntime, frame []byte) bool {
	if s == nil || source == nil || len(frame) < 14 {
		return false
	}
	dst := append(net.HardwareAddr(nil), frame[0:6]...)
	src := append(net.HardwareAddr(nil), frame[6:12]...)
	ethType := binary.BigEndian.Uint16(frame[12:14])
	if ethType == 0x0806 {
		return s.forwardARP(source, frame)
	}
	if isLinuxBroadcastMAC(dst) || isLinuxMulticastMAC(dst) {
		s.forwardToAll(source.id, frame)
		return false
	}
	if bytesEqualMAC(dst, src) {
		return false
	}
	return s.forwardToMAC(source.id, dst, frame)
}

func (s *linuxVirtualSwitch) forwardARP(source *linuxNetworkRuntime, frame []byte) bool {
	if len(frame) < 42 {
		return false
	}
	targetIP := net.IP(frame[38:42]).To4()
	if targetIP == nil {
		return false
	}
	target := s.endpointByIP(source.id, targetIP)
	if target == nil {
		s.forwardToAll(source.id, frame)
		return false
	}
	target.enqueueSwitchFrame(frame)
	return true
}

func (s *linuxVirtualSwitch) endpointByIP(sourceID string, ip net.IP) *linuxNetworkRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, endpoint := range s.endpoints {
		if id == sourceID || endpoint == nil {
			continue
		}
		if endpoint.ip.To4() != nil && endpoint.ip.Equal(ip) {
			return endpoint
		}
	}
	return nil
}

func (s *linuxVirtualSwitch) forwardToMAC(sourceID string, mac net.HardwareAddr, frame []byte) bool {
	s.mu.Lock()
	var target *linuxNetworkRuntime
	for id, endpoint := range s.endpoints {
		if id == sourceID || endpoint == nil {
			continue
		}
		if bytesEqualMAC(endpoint.mac, mac) {
			target = endpoint
			break
		}
	}
	s.mu.Unlock()
	if target != nil {
		target.enqueueSwitchFrame(frame)
		return true
	}
	return false
}

func (s *linuxVirtualSwitch) forwardToAll(sourceID string, frame []byte) {
	s.mu.Lock()
	targets := make([]*linuxNetworkRuntime, 0, len(s.endpoints))
	for id, endpoint := range s.endpoints {
		if id != sourceID && endpoint != nil {
			targets = append(targets, endpoint)
		}
	}
	s.mu.Unlock()
	for _, target := range targets {
		target.enqueueSwitchFrame(frame)
	}
}

func (n *linuxNetworkRuntime) enqueueSwitchFrame(frame []byte) {
	if n == nil {
		return
	}
	copied := append([]byte(nil), frame...)
	select {
	case n.rxQueue <- copied:
	default:
		n.dropped.Add(1)
	}
}

func (n *linuxNetworkRuntime) runRX() {
	defer n.worker.Done()
	for {
		select {
		case frame := <-n.rxQueue:
			if n.dev != nil {
				_ = n.dev.EnqueueRxPacketOwned(frame)
			}
		case <-n.done:
			return
		}
	}
}

func (n *linuxNetworkRuntime) DroppedFrames() uint64 {
	if n == nil {
		return 0
	}
	return n.dropped.Load()
}

func isLinuxBroadcastMAC(mac net.HardwareAddr) bool {
	if len(mac) != 6 {
		return false
	}
	for _, b := range mac {
		if b != 0xff {
			return false
		}
	}
	return true
}

func isLinuxMulticastMAC(mac net.HardwareAddr) bool {
	return len(mac) == 6 && mac[0]&1 == 1
}

func bytesEqualMAC(a, b net.HardwareAddr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
