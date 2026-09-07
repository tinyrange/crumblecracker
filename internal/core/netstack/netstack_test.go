package netstack

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestAttachNetworkInterfaceGeneratesUnicastHostMAC(t *testing.T) {
	ns := New(nil)
	if err := ns.SetGuestMAC(net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}); err != nil {
		t.Fatal(err)
	}
	if _, err := ns.AttachNetworkInterface(); err != nil {
		t.Fatal(err)
	}

	host := macFromUint64(macAddr(ns.hostMAC.Load()))
	if len(host) != 6 {
		t.Fatalf("host mac length = %d", len(host))
	}
	if host[0]&1 != 0 {
		t.Fatalf("host mac is multicast: %s", host.String())
	}
	if host[0]&2 == 0 {
		t.Fatalf("host mac is not locally administered: %s", host.String())
	}
}

func TestAttachNetworkInterfaceUsesConfiguredHostMAC(t *testing.T) {
	ns := New(nil)
	hostMAC := net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x01}
	if err := ns.SetGuestMAC(net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}); err != nil {
		t.Fatal(err)
	}
	if err := ns.SetHostMAC(hostMAC); err != nil {
		t.Fatal(err)
	}
	if _, err := ns.AttachNetworkInterface(); err != nil {
		t.Fatal(err)
	}

	if got := macFromUint64(macAddr(ns.hostMAC.Load())); !bytes.Equal(got, hostMAC) {
		t.Fatalf("host mac = %s, want %s", got, hostMAC)
	}
}

func TestSetHostMACRejectsMulticast(t *testing.T) {
	ns := New(nil)
	if err := ns.SetHostMAC(net.HardwareAddr{0x03, 0x42, 0x0a, 0x2a, 0x00, 0x01}); err == nil {
		t.Fatal("SetHostMAC accepted multicast MAC")
	}
}

func TestNetworkInterfaceDropsSpoofedGuestSources(t *testing.T) {
	h := newBenchmarkHarness(t)
	deliveries := 0
	if err := h.stack.BindUDPCallback("10.42.0.1:9000", func(_ *udpCallbackEndpoint, _ []byte, _ net.UDPAddr) {
		deliveries++
	}); err != nil {
		t.Fatal(err)
	}

	valid := buildBenchmarkUDPFrame(40000, 9000, []byte("valid"))
	if err := h.nic.DeliverGuestPacket(valid, true); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("valid packet deliveries = %d, want 1", deliveries)
	}

	spoofedIP := append([]byte(nil), valid...)
	copy(spoofedIP[ethernetHeaderLen+12:ethernetHeaderLen+16], net.IPv4(10, 42, 0, 3).To4())
	if err := h.nic.DeliverGuestPacket(spoofedIP, true); err != nil {
		t.Fatal(err)
	}
	spoofedMAC := append([]byte(nil), valid...)
	copy(spoofedMAC[6:12], []byte{0x02, 0x42, 0x0a, 0x2a, 0, 3})
	if err := h.nic.DeliverGuestPacket(spoofedMAC, true); err != nil {
		t.Fatal(err)
	}

	if deliveries != 1 {
		t.Fatalf("spoofed packets reached host endpoint: deliveries = %d", deliveries)
	}
	status := h.stack.collectDebugStatus()
	if status.SourceIPv4Violations != 1 || status.SourceMACViolations != 1 {
		t.Fatalf("source violation counters = IPv4:%d MAC:%d, want 1 each", status.SourceIPv4Violations, status.SourceMACViolations)
	}
}

func TestServiceProxyBridgesUDP(t *testing.T) {
	host, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	hostPort := host.LocalAddr().(*net.UDPAddr).Port

	hostPayloads := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 2048)
		n, addr, err := host.ReadFrom(buf)
		if err != nil {
			return
		}
		hostPayloads <- append([]byte(nil), buf[:n]...)
		_, _ = host.WriteTo([]byte("pong"), addr)
	}()

	h := newBenchmarkHarness(t)
	guestFrames := make(chan []byte, 1)
	h.nic.AttachVirtioBackend(func(frame []byte) error {
		guestFrames <- append([]byte(nil), frame...)
		return nil
	})

	guestPort := uint16(40200)
	serviceIP := net.IP(h.stack.serviceIPv4[:])
	frame := buildBenchmarkUDPFrameTo(guestPort, uint16(hostPort), serviceIP, []byte("ping"))
	if err := h.nic.DeliverGuestPacket(frame, true); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-hostPayloads:
		if !bytes.Equal(got, []byte("ping")) {
			t.Fatalf("host udp payload = %q, want ping", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("host did not receive proxied UDP payload")
	}

	select {
	case reply := <-guestFrames:
		srcIP, dstIP, srcPort, dstPort, payload, ok := parseTestUDPFrame(reply)
		if !ok {
			t.Fatalf("reply frame is not UDP: %x", reply)
		}
		if !srcIP.Equal(serviceIP) {
			t.Fatalf("reply src ip = %s, want %s", srcIP, serviceIP)
		}
		if !dstIP.Equal(benchmarkGuestIP) {
			t.Fatalf("reply dst ip = %s, want %s", dstIP, benchmarkGuestIP)
		}
		if srcPort != uint16(hostPort) || dstPort != guestPort {
			t.Fatalf("reply ports = %d -> %d, want %d -> %d", srcPort, dstPort, hostPort, guestPort)
		}
		udp := reply[ethernetHeaderLen+ipv4HeaderLen : ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+len(payload)]
		if got := udpChecksum(srcIP, dstIP, udp); got != 0 {
			t.Fatalf("reply udp checksum = 0x%04x, want 0", got)
		}
		if !bytes.Equal(payload, []byte("pong")) {
			t.Fatalf("reply payload = %q, want pong", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guest did not receive UDP service proxy reply")
	}
}

func parseTestUDPFrame(frame []byte) (srcIP, dstIP net.IP, srcPort, dstPort uint16, payload []byte, ok bool) {
	if len(frame) < ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen {
		return nil, nil, 0, 0, nil, false
	}
	if etherType(binary.BigEndian.Uint16(frame[12:14])) != etherTypeIPv4 {
		return nil, nil, 0, 0, nil, false
	}
	ip := frame[ethernetHeaderLen:]
	if protocolNumber(ip[9]) != udpProtocolNumber {
		return nil, nil, 0, 0, nil, false
	}
	ihl := int(ip[0]&0x0f) * 4
	if len(ip) < ihl+udpHeaderLen {
		return nil, nil, 0, 0, nil, false
	}
	udp := ip[ihl:]
	length := int(binary.BigEndian.Uint16(udp[4:6]))
	if length < udpHeaderLen || len(udp) < length {
		return nil, nil, 0, 0, nil, false
	}
	return net.IP(ip[12:16]), net.IP(ip[16:20]),
		binary.BigEndian.Uint16(udp[0:2]),
		binary.BigEndian.Uint16(udp[2:4]),
		udp[udpHeaderLen:length],
		true
}
