package virtio

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

type testNetBackend struct {
	packets [][]byte
}

func (b *testNetBackend) HandleTxPacket(packet []byte) error {
	b.packets = append(b.packets, append([]byte(nil), packet...))
	return nil
}

type testNetChecksumBackend struct {
	testNetBackend
	needsChecksum bool
}

type lifecycleTestNetMemory struct {
	testGuestMemory
	detach func()
}

func (m *lifecycleTestNetMemory) RegisterGuestMemoryDetach(detach func()) {
	m.detach = detach
}

func (b *testNetChecksumBackend) NeedsTXChecksum([]byte) bool {
	return b.needsChecksum
}

func TestNetLegacyFeaturesAdvertiseMergeRXByDefault(t *testing.T) {
	dev := NewNet(0, 0x1000, 11, nil, nil)

	features, err := dev.ReadLegacy(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if features&netFeatureMergeRX == 0 {
		t.Fatalf("legacy features are missing mergeable RX: %#x", features)
	}
	if features&netFeatureMAC == 0 || features&netFeatureStatus == 0 {
		t.Fatalf("legacy features missing base net features: %#x", features)
	}
}

func TestNetDoesNotAdvertiseUnimplementedHostTSO4(t *testing.T) {
	const hostTSO4 = uint64(1) << 11
	dev := NewNet(0, 0x1000, 11, nil, nil)
	features, err := dev.ReadLegacy(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if features&hostTSO4 != 0 {
		t.Fatalf("virtio-net features advertise unsupported HOST_TSO4: %#x", features)
	}
}

func TestNetLegacyFeaturesCanDisableMergeRX(t *testing.T) {
	dev := NewNet(0, 0x1000, 11, nil, nil)
	dev.DisableMergeRX = true

	features, err := dev.ReadLegacy(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if features&netFeatureMergeRX != 0 {
		t.Fatalf("legacy features include mergeable RX after disabling it: %#x", features)
	}
	if features&netFeatureMAC == 0 || features&netFeatureStatus == 0 {
		t.Fatalf("legacy features missing base net features: %#x", features)
	}
}

func TestNetLegacyTXStripsTenByteHeader(t *testing.T) {
	mem := make(testGuestMemory, 0x20000)
	backend := &testNetBackend{}
	irq := &testIRQ{}
	dev := NewNet(0, 0x1000, 11, net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}, backend)
	dev.DisableMergeRX = true
	dev.Attach(mem, irq)

	if err := dev.WriteLegacy(14, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}
	packet := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02, 0x08, 0x06}
	copy(mem[0x2000+netHeaderLen:], packet)
	writeDesc(mem, 0x10000, 0x2000, uint32(netHeaderLen+len(packet)), 0, 0)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+2:], 1)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+4:], 0)

	if err := dev.WriteLegacy(16, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if len(backend.packets) != 1 {
		t.Fatalf("packets = %d", len(backend.packets))
	}
	if !bytes.Equal(backend.packets[0], packet) {
		t.Fatalf("packet = %x, want %x", backend.packets[0], packet)
	}
}

func TestNetTXCompletesChecksumRequiredByBackend(t *testing.T) {
	mem := make(testGuestMemory, 0x20000)
	backend := &testNetChecksumBackend{needsChecksum: true}
	dev := NewNet(0, 0x1000, 11, nil, backend)
	dev.CompleteTXChecksum = false
	dev.DisableMergeRX = true
	dev.Attach(mem, &testIRQ{})

	if err := dev.WriteLegacy(14, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}
	packet, pseudoHeader := partialUDPChecksumPacket()
	header := mem[0x2000 : 0x2000+netHeaderLen]
	header[0] = netHdrFlagNeedsChecksum
	binary.LittleEndian.PutUint16(header[6:8], 14+20)
	binary.LittleEndian.PutUint16(header[8:10], 6)
	copy(mem[0x2000+netHeaderLen:], packet)
	writeDesc(mem, 0x10000, 0x2000, uint32(netHeaderLen+len(packet)), 0, 0)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+2:], 1)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+4:], 0)

	if err := dev.WriteLegacy(16, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if len(backend.packets) != 1 {
		t.Fatalf("packets = %d, want 1", len(backend.packets))
	}
	udp := backend.packets[0][14+20:]
	checksumInput := append(append([]byte(nil), pseudoHeader...), udp...)
	if got := internetChecksum(checksumInput); got != 0 {
		t.Fatalf("forwarded UDP checksum validation = %#04x, want 0", got)
	}
}

func partialUDPChecksumPacket() ([]byte, []byte) {
	const payload = "19+23=42"
	packet := make([]byte, 14+20+8+len(payload))
	copy(packet[0:6], []byte{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x03})
	copy(packet[6:12], []byte{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02})
	binary.BigEndian.PutUint16(packet[12:14], 0x0800)
	ip := packet[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+8+len(payload)))
	ip[8] = 64
	ip[9] = 17
	copy(ip[12:16], []byte{10, 42, 0, 2})
	copy(ip[16:20], []byte{10, 42, 0, 3})
	udp := packet[34:]
	binary.BigEndian.PutUint16(udp[0:2], 12345)
	binary.BigEndian.PutUint16(udp[2:4], 8080)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	pseudoHeader := make([]byte, 12)
	copy(pseudoHeader[0:4], ip[12:16])
	copy(pseudoHeader[4:8], ip[16:20])
	pseudoHeader[9] = 17
	binary.BigEndian.PutUint16(pseudoHeader[10:12], uint16(len(udp)))
	binary.BigEndian.PutUint16(udp[6:8], ^internetChecksum(pseudoHeader))
	return packet, pseudoHeader
}

func TestNetLegacyTXHonorsAvailNoInterrupt(t *testing.T) {
	mem := make(testGuestMemory, 0x20000)
	backend := &testNetBackend{}
	irq := &testIRQ{}
	dev := NewNet(0, 0x1000, 11, nil, backend)
	dev.DisableMergeRX = true
	dev.Attach(mem, irq)

	if err := dev.WriteLegacy(14, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}
	packet := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02, 0x08, 0x06}
	copy(mem[0x2000+netHeaderLen:], packet)
	writeDesc(mem, 0x10000, 0x2000, uint32(netHeaderLen+len(packet)), 0, 0)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize:], vringAvailNoInterrupt)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+2:], 1)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+4:], 0)

	if err := dev.WriteLegacy(16, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if len(backend.packets) != 1 {
		t.Fatalf("packets = %d", len(backend.packets))
	}
	if irq.level || dev.IRQAsserted() {
		t.Fatalf("TX queue asserted IRQ despite avail no-interrupt flag")
	}
	if isr, err := dev.ReadLegacy(19, 1); err != nil || isr != 0 {
		t.Fatalf("legacy ISR = %#x, %v; want 0, nil", isr, err)
	}
}

func TestNetTXCanForceTwelveByteHeader(t *testing.T) {
	mem := make(testGuestMemory, 0x20000)
	backend := &testNetBackend{}
	irq := &testIRQ{}
	dev := NewNet(0, 0x1000, 11, net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}, backend)
	dev.HeaderLength = netHeaderLenMergeRX
	dev.Attach(mem, irq)

	if err := dev.WriteLegacy(14, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}
	packet := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02, 0x08, 0x06}
	copy(mem[0x2000+netHeaderLenMergeRX:], packet)
	writeDesc(mem, 0x10000, 0x2000, uint32(netHeaderLenMergeRX+len(packet)), 0, 0)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+2:], 1)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+4:], 0)

	if err := dev.WriteLegacy(16, 2, netQueueTX); err != nil {
		t.Fatal(err)
	}
	if len(backend.packets) != 1 {
		t.Fatalf("packets = %d", len(backend.packets))
	}
	if !bytes.Equal(backend.packets[0], packet) {
		t.Fatalf("packet = %x, want %x", backend.packets[0], packet)
	}
}

func TestNetLegacyRXWritesTenByteHeader(t *testing.T) {
	mem := make(testGuestMemory, 0x20000)
	irq := &testIRQ{}
	dev := NewNet(0, 0x1000, 11, nil, nil)
	dev.DisableMergeRX = true
	dev.Attach(mem, irq)

	if err := dev.WriteLegacy(14, 2, netQueueRX); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}
	writeDesc(mem, 0x10000, 0x3000, 2048, descFWrite, 0)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+2:], 1)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+4:], 0)

	packet := []byte{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x08, 0x00}
	if err := dev.EnqueueRxPacket(packet); err != nil {
		t.Fatal(err)
	}

	if got := binary.LittleEndian.Uint16(mem[0x10000+16*netQueueSize+4096+4:]); got != 0 {
		t.Fatalf("used element id = %d", got)
	}
	usedLen := binary.LittleEndian.Uint32(mem[0x10000+16*netQueueSize+4096+8:])
	if usedLen != uint32(netHeaderLen+len(packet)) {
		t.Fatalf("used len = %d, want %d", usedLen, netHeaderLen+len(packet))
	}
	if !bytes.Equal(mem[0x3000+netHeaderLen:0x3000+netHeaderLen+uint64(len(packet))], packet) {
		t.Fatalf("packet payload was not written after ten-byte header")
	}
}

func TestNetGuestMemoryLifecycleDetachesRXBeforeUnmap(t *testing.T) {
	mem := &lifecycleTestNetMemory{testGuestMemory: make(testGuestMemory, 0x20000)}
	dev := NewNet(0, 0x1000, 11, nil, nil)
	dev.DisableMergeRX = true
	dev.Attach(mem, &testIRQ{})

	if err := dev.WriteLegacy(14, 2, netQueueRX); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}
	if mem.detach == nil {
		t.Fatal("guest memory did not receive a detach callback")
	}

	mem.detach()
	mem.testGuestMemory = nil // Models the hypervisor unmapping guest RAM.
	if err := dev.EnqueueRxPacketOwned([]byte{1, 2, 3}); err != nil {
		t.Fatalf("enqueue after guest teardown: %v", err)
	}
	if len(dev.pendingRx) != 0 {
		t.Fatalf("packets retained after guest teardown: %d", len(dev.pendingRx))
	}
}

func TestNetLegacyRXHonorsAvailNoInterrupt(t *testing.T) {
	mem := make(testGuestMemory, 0x20000)
	irq := &testIRQ{}
	dev := NewNet(0, 0x1000, 11, nil, nil)
	dev.DisableMergeRX = true
	dev.Attach(mem, irq)

	if err := dev.WriteLegacy(14, 2, netQueueRX); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}
	writeDesc(mem, 0x10000, 0x3000, 2048, descFWrite, 0)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize:], vringAvailNoInterrupt)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+2:], 1)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+4:], 0)

	packet := []byte{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x08, 0x00}
	if err := dev.EnqueueRxPacket(packet); err != nil {
		t.Fatal(err)
	}
	if irq.level || dev.IRQAsserted() {
		t.Fatalf("RX queue asserted IRQ despite avail no-interrupt flag")
	}
	usedLen := binary.LittleEndian.Uint32(mem[0x10000+16*netQueueSize+4096+8:])
	if usedLen != uint32(netHeaderLen+len(packet)) {
		t.Fatalf("used len = %d, want %d", usedLen, netHeaderLen+len(packet))
	}
	if isr, err := dev.ReadLegacy(19, 1); err != nil || isr != 0 {
		t.Fatalf("legacy ISR = %#x, %v; want 0, nil", isr, err)
	}
}

func TestNetLegacyRXDrainsPendingPacketsWhenQueueIsConfigured(t *testing.T) {
	mem := make(testGuestMemory, 0x20000)
	irq := &testIRQ{}
	dev := NewNet(0, 0x1000, 11, nil, nil)
	dev.DisableMergeRX = true
	dev.Attach(mem, irq)

	packet := []byte{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x08, 0x00}
	if err := dev.EnqueueRxPacket(packet); err != nil {
		t.Fatal(err)
	}

	if err := dev.WriteLegacy(14, 2, netQueueRX); err != nil {
		t.Fatal(err)
	}
	writeDesc(mem, 0x10000, 0x3000, 2048, descFWrite, 0)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+2:], 1)
	binary.LittleEndian.PutUint16(mem[0x10000+16*netQueueSize+4:], 0)

	if err := dev.WriteLegacy(8, 4, 0x10); err != nil {
		t.Fatal(err)
	}

	usedLen := binary.LittleEndian.Uint32(mem[0x10000+16*netQueueSize+4096+8:])
	if usedLen != uint32(netHeaderLen+len(packet)) {
		t.Fatalf("used len = %d, want %d", usedLen, netHeaderLen+len(packet))
	}
	if !bytes.Equal(mem[0x3000+netHeaderLen:0x3000+netHeaderLen+uint64(len(packet))], packet) {
		t.Fatalf("pending packet payload was not written when queue was configured")
	}
}
