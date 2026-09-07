//go:build linux

package vm

import (
	"bytes"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestLinuxVirtualSwitchAllocatesDistinctLeases(t *testing.T) {
	defaultLinuxVirtualSwitch.Unregister("freebsd")
	defaultLinuxVirtualSwitch.Unregister("openbsd")
	leaseA := mustRegisterLinuxLease(t, defaultLinuxVirtualSwitch, "freebsd", nil)
	t.Cleanup(func() { defaultLinuxVirtualSwitch.Unregister(leaseA.id) })
	leaseB := mustRegisterLinuxLease(t, defaultLinuxVirtualSwitch, "openbsd", nil)
	t.Cleanup(func() { defaultLinuxVirtualSwitch.Unregister(leaseB.id) })

	if got := leaseA.ip.String(); got != "10.42.0.2" {
		t.Fatalf("first lease IP = %s, want 10.42.0.2", got)
	}
	if got := leaseA.mac.String(); got != "02:42:0a:2a:00:02" {
		t.Fatalf("first lease MAC = %s, want 02:42:0a:2a:00:02", got)
	}
	if got := leaseB.ip.String(); got != "10.42.0.3" {
		t.Fatalf("second lease IP = %s, want 10.42.0.3", got)
	}
	if got := leaseB.mac.String(); got != "02:42:0a:2a:00:03" {
		t.Fatalf("second lease MAC = %s, want 02:42:0a:2a:00:03", got)
	}
}

func TestLinuxSwitchHonorsExplicitIdentityAndScopesConflicts(t *testing.T) {
	cfg := &client.NetworkConfig{Enabled: true, GuestIPv4: "10.42.0.77", GuestMAC: "02:42:0a:2a:00:4d"}
	firstDomain := newLinuxVirtualSwitch()
	lease := mustRegisterLinuxLease(t, firstDomain, "one", cfg)
	if lease.ip.String() != cfg.GuestIPv4 || lease.mac.String() != cfg.GuestMAC {
		t.Fatalf("lease = %s/%s, want %s/%s", lease.ip, lease.mac, cfg.GuestIPv4, cfg.GuestMAC)
	}
	if _, err := firstDomain.Register("two", cfg); err == nil {
		t.Fatal("duplicate explicit identity was accepted in one domain")
	}
	secondDomain := newLinuxVirtualSwitch()
	other := mustRegisterLinuxLease(t, secondDomain, "two", cfg)
	if !other.ip.Equal(lease.ip) || !bytes.Equal(other.mac, lease.mac) {
		t.Fatalf("isolated domain identity = %s/%s", other.ip, other.mac)
	}
}

func TestLinuxEndpointQueueIsBoundedFIFO(t *testing.T) {
	runtime := &linuxNetworkRuntime{rxQueue: make(chan []byte, 2)}
	runtime.enqueueSwitchFrame([]byte{1})
	runtime.enqueueSwitchFrame([]byte{2})
	runtime.enqueueSwitchFrame([]byte{3})
	if runtime.DroppedFrames() != 1 {
		t.Fatalf("dropped frames = %d, want 1", runtime.DroppedFrames())
	}
	if first, second := <-runtime.rxQueue, <-runtime.rxQueue; !bytes.Equal(first, []byte{1}) || !bytes.Equal(second, []byte{2}) {
		t.Fatalf("queued frames = %v, %v", first, second)
	}
}

func TestLinuxSwitchConsumesKnownPeerUnicast(t *testing.T) {
	switchNet := newLinuxVirtualSwitch()
	leaseA := mustRegisterLinuxLease(t, switchNet, "a", nil)
	leaseB := mustRegisterLinuxLease(t, switchNet, "b", nil)
	a := &linuxNetworkRuntime{
		networkRuntime: &networkRuntime{id: leaseA.id, ip: leaseA.ip, mac: leaseA.mac},
		rxQueue:        make(chan []byte, 1),
	}
	b := &linuxNetworkRuntime{
		networkRuntime: &networkRuntime{id: leaseB.id, ip: leaseB.ip, mac: leaseB.mac},
		rxQueue:        make(chan []byte, 1),
	}
	switchNet.Attach(a)
	switchNet.Attach(b)
	frame := make([]byte, 14)
	copy(frame[:6], leaseB.mac)
	copy(frame[6:12], leaseA.mac)
	frame[12], frame[13] = 0x08, 0x00
	if !switchNet.Forward(a, frame) {
		t.Fatal("known peer unicast was not consumed by virtual switch")
	}
	if got := <-b.rxQueue; !bytes.Equal(got, frame) {
		t.Fatalf("forwarded frame = %x, want %x", got, frame)
	}
	copy(frame[:6], defaultGatewayMACBytes)
	if switchNet.Forward(a, frame) {
		t.Fatal("gateway frame was consumed instead of reaching netstack")
	}
}

func mustRegisterLinuxLease(t *testing.T, s *linuxVirtualSwitch, id string, cfg *client.NetworkConfig) linuxNetworkLease {
	t.Helper()
	lease, err := s.Register(id, cfg)
	if err != nil {
		t.Fatalf("register lease: %v", err)
	}
	return lease
}

func TestLinuxPCINetworkRuntimeAttachesToVirtualSwitch(t *testing.T) {
	defaultLinuxVirtualSwitch.Unregister("openbsd")
	defaultLinuxVirtualSwitch.Unregister("freebsd")

	openbsdNet, err := newLinuxPCINetworkRuntime("openbsd", &client.NetworkConfig{Enabled: true})
	if err != nil {
		t.Fatalf("create OpenBSD PCI network runtime: %v", err)
	}
	freebsdNet, err := newLinuxPCINetworkRuntime("freebsd", &client.NetworkConfig{Enabled: true})
	if err != nil {
		_ = openbsdNet.Close()
		t.Fatalf("create FreeBSD PCI network runtime: %v", err)
	}
	t.Cleanup(func() { _ = openbsdNet.Close() })
	t.Cleanup(func() { _ = freebsdNet.Close() })

	if got := openbsdNet.GuestAddress(); got != "10.42.0.2" {
		t.Fatalf("OpenBSD runtime IP = %s, want 10.42.0.2", got)
	}
	if got := freebsdNet.GuestAddress(); got != "10.42.0.3" {
		t.Fatalf("FreeBSD runtime IP = %s, want 10.42.0.3", got)
	}
	if target := defaultLinuxVirtualSwitch.endpointByIP("", openbsdNet.ip); target != openbsdNet {
		t.Fatalf("OpenBSD runtime was not attached to virtual switch")
	}
	if target := defaultLinuxVirtualSwitch.endpointByIP("", freebsdNet.ip); target != freebsdNet {
		t.Fatalf("FreeBSD runtime was not attached to virtual switch")
	}

	if err := openbsdNet.Close(); err != nil {
		t.Fatalf("close OpenBSD runtime: %v", err)
	}
	if target := defaultLinuxVirtualSwitch.endpointByIP("", openbsdNet.ip); target != nil {
		t.Fatalf("OpenBSD runtime remained attached after close")
	}
}
