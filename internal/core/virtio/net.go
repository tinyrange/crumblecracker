package virtio

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	mmioDeviceIDNet = 1

	netQueueRX = 0
	netQueueTX = 1

	netQueueSize = 256

	netFeatureCSUM          = uint64(1) << 0
	vringAvailNoInterrupt   = 1
	netFeatureMAC           = uint64(1) << 5
	netFeatureStatus        = uint64(1) << 16
	netFeatureMergeRX       = uint64(1) << 15
	netHdrFlagNeedsChecksum = 1

	netStatusLinkUp      = 1
	netHeaderLen         = 10
	netHeaderLenMergeRX  = 12
	maxNetPacketPoolSize = 256 * 1024
	netPacketPoolDepth   = 256
)

type netByteSlicePool struct {
	ch         chan []byte
	defaultCap int
	maxCap     int
}

func newNetByteSlicePool(defaultCap, maxCap int) *netByteSlicePool {
	return &netByteSlicePool{
		ch:         make(chan []byte, netPacketPoolDepth),
		defaultCap: defaultCap,
		maxCap:     maxCap,
	}
}

func (p *netByteSlicePool) get(size int) []byte {
	if size > p.maxCap {
		return make([]byte, size)
	}
	select {
	case buf := <-p.ch:
		if cap(buf) >= size {
			return buf[:size]
		}
	default:
	}
	capacity := p.defaultCap
	if capacity < size {
		capacity = size
	}
	return make([]byte, size, capacity)
}

func (p *netByteSlicePool) put(buf []byte) {
	if buf == nil || cap(buf) > p.maxCap {
		return
	}
	select {
	case p.ch <- buf[:0]:
	default:
	}
}

var netPacketPool = newNetByteSlicePool(2048, maxNetPacketPoolSize)

var netTXPacketBatchPool = sync.Pool{
	New: func() any {
		return new([netQueueSize]netTXPacket)
	},
}

type NetBackend interface {
	HandleTxPacket(packet []byte) error
}

// NetTXChecksumPolicy lets a backend request checksum completion for packets
// that leave a checksum-trusting local path. The packet does not include the
// virtio header and is only valid for the duration of the call.
type NetTXChecksumPolicy interface {
	NeedsTXChecksum(packet []byte) bool
}

type guestMemoryDetachRegistrar interface {
	RegisterGuestMemoryDetach(func())
}

type Net struct {
	Base         uint64
	Size         uint64
	IRQ          uint32
	MAC          net.HardwareAddr
	LegacyMMIO   bool
	AsyncTX      bool
	AsyncTXDelay time.Duration
	NoTXIRQ      bool
	NoTXUsed     bool

	DisableMergeRX     bool
	CompleteTXChecksum bool
	HeaderLength       int
	mu                 sync.Mutex
	mem                GuestMemory
	irq                IRQController
	backend            NetBackend
	deviceFeatureSel   uint32
	driverFeatureSel   uint32
	driverFeatures     uint64
	queueSel           uint32
	status             uint32
	interruptStatus    uint32
	irqHigh            bool
	configGeneration   uint32
	queues             [2]queue
	pendingRx          [][]byte
	legacy             bool
	scratch2           [2]byte
	scratch4           [4]byte
	scratch8           [8]byte
	scratch16          [16]byte
	asyncTXRunning     bool
	asyncTXAgain       bool
	asyncTXErr         error
	detached           bool
}

type netTXPacket struct {
	packet []byte
	buffer []byte
}

func NewNet(base, size uint64, irq uint32, mac net.HardwareAddr, backend NetBackend) *Net {
	n := &Net{
		Base:               base,
		Size:               size,
		IRQ:                irq,
		MAC:                append(net.HardwareAddr(nil), mac...),
		backend:            backend,
		CompleteTXChecksum: true,
	}
	if len(n.MAC) != 6 {
		n.MAC = net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}
	}
	n.resetLocked()
	return n
}

func (n *Net) Attach(mem GuestMemory, irq IRQController) {
	n.mu.Lock()
	n.mem = mem
	n.irq = irq
	n.detached = false
	n.mu.Unlock()
	if registrar, ok := mem.(guestMemoryDetachRegistrar); ok {
		registrar.RegisterGuestMemoryDetach(n.Detach)
	}
}

// Detach prevents any in-flight or future packet work from accessing guest
// memory. Hypervisor backends call it before unmapping the memory registered
// with KVM.
func (n *Net) Detach() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.mem = nil
	n.irq = nil
	n.detached = true
	n.pendingRx = nil
	for index := range n.queues {
		n.queues[index].clearCache()
	}
}

func (n *Net) Contains(addr uint64, size int) bool {
	return addr >= n.Base && addr+uint64(size) <= n.Base+n.Size
}

func (n *Net) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("virtio@%x", n.Base),
		Properties: map[string]fdt.Property{
			"compatible":   {Strings: []string{"virtio,mmio"}},
			"dma-coherent": {Flag: true},
			"reg":          {U64: []uint64{n.Base, n.Size}},
			"interrupts":   {U32: []uint32{0, n.IRQ, 4}},
			"status":       {Strings: []string{"okay"}},
		},
	}
}

func (n *Net) Read(addr uint64, size int) (uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	offset := addr - n.Base
	switch offset {
	case regMagicValue:
		return truncateValue(mmioMagicValue, size), nil
	case regVersion:
		return truncateValue(uint64(mmioTransportVersion(n.LegacyMMIO)), size), nil
	case regDeviceID:
		return truncateValue(mmioDeviceIDNet, size), nil
	case regVendorID:
		return truncateValue(mmioVendorID, size), nil
	case regDeviceFeatures:
		features := n.deviceFeaturesLocked()
		if n.LegacyMMIO {
			features = n.legacyFeaturesLocked()
		}
		if n.deviceFeatureSel == 0 {
			return truncateValue(features, size), nil
		}
		if n.deviceFeatureSel == 1 {
			return truncateValue(features>>32, size), nil
		}
		return 0, nil
	case regQueueNumMax:
		if n.queueSel < uint32(len(n.queues)) {
			return truncateValue(netQueueSize, size), nil
		}
		return 0, nil
	case regQueueNum:
		if q := n.selectedQueueLocked(); q != nil {
			return truncateValue(uint64(q.size), size), nil
		}
		return 0, nil
	case regQueueReady:
		if n.LegacyMMIO {
			return 0, nil
		}
		if q := n.selectedQueueLocked(); q != nil && q.ready {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regQueuePFN:
		if n.LegacyMMIO {
			if q := n.selectedQueueLocked(); q != nil && q.ready {
				return truncateValue(q.descAddr/4096, size), nil
			}
		}
		return 0, nil
	case regInterruptStatus:
		return truncateValue(uint64(n.interruptStatus), size), nil
	case regStatus:
		return truncateValue(uint64(n.status), size), nil
	case regConfigGen:
		return truncateValue(uint64(n.configGeneration), size), nil
	}

	if offset >= regConfig && offset+uint64(size) <= regConfig+8 {
		cfg := n.configBytesLocked()
		return truncateValue(readConfigValue(cfg[offset-regConfig:], size), size), nil
	}
	return 0, nil
}

func (n *Net) Write(addr uint64, size int, value uint64) error {
	n.mu.Lock()
	if n.asyncTXErr != nil {
		err := n.asyncTXErr
		n.asyncTXErr = nil
		n.mu.Unlock()
		return err
	}

	offset := addr - n.Base
	switch offset {
	case regDeviceFeatSel:
		n.deviceFeatureSel = uint32(value)
	case regDriverFeatSel:
		n.driverFeatureSel = uint32(value)
	case regDriverFeatures:
		if n.driverFeatureSel == 0 {
			n.driverFeatures = (n.driverFeatures &^ 0xffffffff) | uint64(uint32(value))
		} else if n.driverFeatureSel == 1 {
			n.driverFeatures = (n.driverFeatures & 0xffffffff) | (uint64(uint32(value)) << 32)
		}
	case regQueueSel:
		n.queueSel = uint32(value)
	case regQueueNum:
		if q := n.selectedQueueLocked(); q != nil {
			q.size = uint16(value)
		}
	case regQueueReady:
		if n.LegacyMMIO {
			n.mu.Unlock()
			return nil
		}
		if q := n.selectedQueueLocked(); q != nil {
			q.ready = value != 0
			if value == 0 {
				q.lastAvailIdx = 0
				q.usedIdx = 0
				q.clearCache()
			} else if n.queueSel == netQueueRX {
				if err := n.processRXLocked(); err != nil {
					n.mu.Unlock()
					return err
				}
			}
		}
	case regGuestPageSize, regQueueAlign:
		if n.LegacyMMIO {
			n.mu.Unlock()
			return nil
		}
	case regQueuePFN:
		if n.LegacyMMIO {
			if q := n.selectedQueueLocked(); q != nil {
				if value == 0 {
					q.ready = false
					q.descAddr = 0
					q.availAddr = 0
					q.usedAddr = 0
					q.clearCache()
					n.mu.Unlock()
					return nil
				}
				n.configureLegacyQueueLocked(q, uint32(value))
				if n.queueSel == netQueueRX {
					if err := n.processRXLocked(); err != nil {
						n.mu.Unlock()
						return err
					}
				}
			}
		}
	case regQueueDescLow:
		if q := n.selectedQueueLocked(); q != nil {
			n.setQueueAddr(&q.descAddr, uint32(value), true)
			q.clearCache()
		}
	case regQueueDescHigh:
		if q := n.selectedQueueLocked(); q != nil {
			n.setQueueAddr(&q.descAddr, uint32(value), false)
			q.clearCache()
		}
	case regQueueAvailLow:
		if q := n.selectedQueueLocked(); q != nil {
			n.setQueueAddr(&q.availAddr, uint32(value), true)
			q.clearCache()
		}
	case regQueueAvailHigh:
		if q := n.selectedQueueLocked(); q != nil {
			n.setQueueAddr(&q.availAddr, uint32(value), false)
			q.clearCache()
		}
	case regQueueUsedLow:
		if q := n.selectedQueueLocked(); q != nil {
			n.setQueueAddr(&q.usedAddr, uint32(value), true)
			q.clearCache()
		}
	case regQueueUsedHigh:
		if q := n.selectedQueueLocked(); q != nil {
			n.setQueueAddr(&q.usedAddr, uint32(value), false)
			q.clearCache()
		}
	case regInterruptAck:
		n.interruptStatus &^= uint32(value)
		err := n.updateIRQLocked()
		n.mu.Unlock()
		return err
	case regStatus:
		n.status = uint32(value)
		if n.status == 0 {
			n.resetLocked()
		}
	case regQueueNotify:
		switch value {
		case netQueueTX:
			if n.AsyncTX {
				n.scheduleAsyncTXLocked()
				n.mu.Unlock()
				return nil
			}
			packetBatch := getNetTXPacketBatch()
			packets, err := n.processTXLocked(packetBatch[:0])
			n.mu.Unlock()
			if err != nil {
				releaseNetTXPackets(packets)
				releaseNetTXPacketBatch(packetBatch, len(packets))
				return err
			}
			err = n.deliverTXPackets(packets)
			releaseNetTXPacketBatch(packetBatch, len(packets))
			return err
		case netQueueRX:
			err := n.processRXLocked()
			n.mu.Unlock()
			return err
		}
	}
	n.mu.Unlock()
	return nil
}

func (n *Net) ReadLegacy(offset uint16, size int) (uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.legacy = true

	switch offset {
	case 0:
		return truncateValue(n.legacyFeaturesLocked(), size), nil
	case 8:
		if q := n.selectedQueueLocked(); q != nil && q.ready {
			return truncateValue(uint64(q.descAddr/4096), size), nil
		}
		return 0, nil
	case 12:
		if n.queueSel < uint32(len(n.queues)) {
			return truncateValue(netQueueSize, size), nil
		}
		return 0, nil
	case 14:
		return truncateValue(uint64(n.queueSel), size), nil
	case 18:
		return truncateValue(uint64(n.status), size), nil
	case 19:
		isr := n.interruptStatus
		n.interruptStatus = 0
		if err := n.updateIRQLocked(); err != nil {
			return 0, err
		}
		return truncateValue(uint64(isr), size), nil
	}

	if offset >= 20 && int(offset)+size <= 28 {
		cfg := n.configBytesLocked()
		return truncateValue(readConfigValue(cfg[offset-20:], size), size), nil
	}
	return 0, nil
}

func (n *Net) WriteLegacy(offset uint16, size int, value uint64) error {
	n.mu.Lock()
	n.legacy = true
	if n.asyncTXErr != nil {
		err := n.asyncTXErr
		n.asyncTXErr = nil
		n.mu.Unlock()
		return err
	}

	switch offset {
	case 4:
		n.driverFeatures = uint64(uint32(value))
	case 8:
		if q := n.selectedQueueLocked(); q != nil {
			if value == 0 {
				q.ready = false
				q.descAddr = 0
				q.availAddr = 0
				q.usedAddr = 0
				q.clearCache()
				n.mu.Unlock()
				return nil
			}
			n.configureLegacyQueueLocked(q, uint32(value))
			if n.queueSel == netQueueRX {
				if err := n.processRXLocked(); err != nil {
					n.mu.Unlock()
					return err
				}
			}
		}
	case 14:
		n.queueSel = uint32(value)
	case 16:
		switch value {
		case netQueueTX:
			if n.AsyncTX {
				n.scheduleAsyncTXLocked()
				n.mu.Unlock()
				return nil
			}
			packetBatch := getNetTXPacketBatch()
			packets, err := n.processTXLocked(packetBatch[:0])
			n.mu.Unlock()
			if err != nil {
				releaseNetTXPackets(packets)
				releaseNetTXPacketBatch(packetBatch, len(packets))
				return err
			}
			err = n.deliverTXPackets(packets)
			releaseNetTXPacketBatch(packetBatch, len(packets))
			return err
		case netQueueRX:
			err := n.processRXLocked()
			n.mu.Unlock()
			return err
		}
	case 18:
		n.status = uint32(uint8(value))
		if n.status == 0 {
			n.resetLocked()
			n.legacy = true
		}
	}
	n.mu.Unlock()
	return nil
}

func (n *Net) scheduleAsyncTXLocked() {
	if n.asyncTXRunning {
		n.asyncTXAgain = true
		return
	}
	n.asyncTXRunning = true
	if n.AsyncTXDelay > 0 {
		time.AfterFunc(n.AsyncTXDelay, n.runAsyncTX)
		return
	}
	go n.runAsyncTX()
}

func (n *Net) runAsyncTX() {
	for {
		packetBatch := getNetTXPacketBatch()
		n.mu.Lock()
		packets, err := n.processTXLocked(packetBatch[:0])
		n.mu.Unlock()
		if err == nil {
			err = n.deliverTXPackets(packets)
		}
		releaseNetTXPacketBatch(packetBatch, len(packets))

		n.mu.Lock()
		if err != nil {
			n.asyncTXErr = err
		}
		if !n.asyncTXAgain || err != nil {
			n.asyncTXRunning = false
			n.asyncTXAgain = false
			n.mu.Unlock()
			return
		}
		n.asyncTXAgain = false
		n.mu.Unlock()
	}
}

func (n *Net) EnqueueRxPacket(packet []byte) error {
	return n.enqueueRxPacket(packet, false)
}

func (n *Net) EnqueueRxPacketOwned(packet []byte) error {
	return n.enqueueRxPacket(packet, true)
}

func (n *Net) enqueueRxPacket(packet []byte, owned bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.detached {
		return nil
	}
	if len(n.pendingRx) == 0 {
		delivered, err := n.processOneRXPacketLocked(packet)
		if err != nil {
			return err
		}
		if delivered {
			return nil
		}
	}
	if !owned {
		packet = append([]byte(nil), packet...)
	}
	n.pendingRx = append(n.pendingRx, packet)
	return n.processRXLocked()
}

func getNetPacketBuffer() []byte {
	return netPacketPool.get(0)
}

func releaseNetPacketBuffer(buf []byte) {
	netPacketPool.put(buf)
}

func getNetTXPacketBatch() *[netQueueSize]netTXPacket {
	return netTXPacketBatchPool.Get().(*[netQueueSize]netTXPacket)
}

func releaseNetTXPacketBatch(batch *[netQueueSize]netTXPacket, used int) {
	if batch == nil {
		return
	}
	clear(batch[:used])
	netTXPacketBatchPool.Put(batch)
}

func readGuestMemoryInto(mem GuestMemory, addr uint64, dst []byte) error {
	if reader, ok := mem.(guestMemoryReaderInto); ok {
		return reader.ReadIPAInto(addr, dst)
	}
	buf, err := mem.ReadIPA(addr, len(dst))
	if err != nil {
		return err
	}
	copy(dst, buf)
	return nil
}

func (n *Net) readIPAInto(addr uint64, dst []byte) error {
	return readGuestMemoryInto(n.mem, addr, dst)
}

func (n *Net) ensureQueueCacheLocked(q *queue) error {
	if q.size == 0 || q.descAddr == 0 || q.availAddr == 0 || q.usedAddr == 0 {
		return nil
	}
	if q.descMem != nil && q.availMem != nil && q.usedMem != nil {
		return nil
	}
	slicer, ok := n.mem.(guestMemorySlicer)
	if !ok {
		return nil
	}
	descLen := int(q.size) * 16
	availLen := 4 + int(q.size)*2 + 2
	usedLen := 4 + int(q.size)*8 + 2
	descMem, err := slicer.SliceIPA(q.descAddr, descLen)
	if err != nil {
		return err
	}
	availMem, err := slicer.SliceIPA(q.availAddr, availLen)
	if err != nil {
		return err
	}
	usedMem, err := slicer.SliceIPA(q.usedAddr, usedLen)
	if err != nil {
		return err
	}
	q.descMem = descMem
	q.availMem = availMem
	q.usedMem = usedMem
	return nil
}

func (n *Net) readAvailHeaderLocked(q *queue) (flags uint16, idx uint16, err error) {
	if err := n.ensureQueueCacheLocked(q); err != nil {
		return 0, 0, err
	}
	if len(q.availMem) >= 4 {
		return binary.LittleEndian.Uint16(q.availMem[0:2]), binary.LittleEndian.Uint16(q.availMem[2:4]), nil
	}
	if err := n.readIPAInto(q.availAddr, n.scratch4[:]); err != nil {
		return 0, 0, err
	}
	return binary.LittleEndian.Uint16(n.scratch4[0:2]), binary.LittleEndian.Uint16(n.scratch4[2:4]), nil
}

func (n *Net) readAvailRingEntryLocked(q *queue, slot uint16) (uint16, error) {
	offset := 4 + int(slot)*2
	if len(q.availMem) >= offset+2 {
		return binary.LittleEndian.Uint16(q.availMem[offset : offset+2]), nil
	}
	if err := n.readIPAInto(q.availAddr+uint64(offset), n.scratch2[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(n.scratch2[:]), nil
}

func (n *Net) processTXLocked(packets []netTXPacket) ([]netTXPacket, error) {
	q := &n.queues[netQueueTX]
	if !q.ready || q.size == 0 || n.mem == nil {
		return nil, nil
	}
	_, availIdx, err := n.readAvailHeaderLocked(q)
	if err != nil {
		return nil, err
	}
	headerLen := n.netHeaderLenLocked()
	for q.lastAvailIdx != availIdx {
		slot := q.lastAvailIdx % q.size
		head, err := n.readAvailRingEntryLocked(q, slot)
		if err != nil {
			releaseNetTXPackets(packets)
			return nil, err
		}
		data, err := n.readChainLocked(q, head, false)
		if err != nil {
			releaseNetTXPackets(packets)
			return nil, err
		}
		releaseData := data
		if len(data) >= headerLen {
			packet := data[headerLen:]
			completeChecksum := n.CompleteTXChecksum
			if !completeChecksum {
				if policy, ok := n.backend.(NetTXChecksumPolicy); ok {
					completeChecksum = policy.NeedsTXChecksum(packet)
				}
			}
			if completeChecksum {
				if err := fixTXChecksum(data, headerLen); err != nil {
					releaseNetPacketBuffer(releaseData)
					releaseNetTXPackets(packets)
					return nil, err
				}
			}
			if n.backend != nil {
				if len(packets) == cap(packets) {
					releaseNetPacketBuffer(releaseData)
					releaseNetTXPackets(packets)
					return nil, fmt.Errorf("virtio-net tx batch full")
				}
				packets = append(packets, netTXPacket{
					packet: packet,
					buffer: releaseData,
				})
				releaseData = nil
			}
		}
		releaseNetPacketBuffer(releaseData)
		if !n.NoTXUsed {
			if err := n.writeUsedLocked(q, head, 0); err != nil {
				releaseNetTXPackets(packets)
				return nil, err
			}
		}
		q.lastAvailIdx++
	}
	if !n.NoTXUsed && !n.NoTXIRQ && !n.queueInterruptSuppressedLocked(q) {
		n.interruptStatus |= intVring
		if err := n.updateIRQLocked(); err != nil {
			releaseNetTXPackets(packets)
			return nil, err
		}
	}
	return packets, nil
}

func (n *Net) deliverTXPackets(packets []netTXPacket) error {
	for i, packet := range packets {
		if n.backend == nil {
			releaseNetPacketBuffer(packet.buffer)
			continue
		}
		err := n.backend.HandleTxPacket(packet.packet)
		releaseNetPacketBuffer(packet.buffer)
		if err != nil {
			releaseNetTXPackets(packets[i+1:])
			return err
		}
	}
	return nil
}

func releaseNetTXPackets(packets []netTXPacket) {
	for _, packet := range packets {
		releaseNetPacketBuffer(packet.buffer)
	}
}

func (n *Net) processRXLocked() error {
	q := &n.queues[netQueueRX]
	if !q.ready || q.size == 0 || n.mem == nil || len(n.pendingRx) == 0 {
		return nil
	}
	_, availIdx, err := n.readAvailHeaderLocked(q)
	if err != nil {
		return err
	}
	processed := false
	for q.lastAvailIdx != availIdx && len(n.pendingRx) > 0 {
		slot := q.lastAvailIdx % q.size
		head, err := n.readAvailRingEntryLocked(q, slot)
		if err != nil {
			return err
		}
		packet := n.pendingRx[0]
		written, err := n.writeRXPacketLocked(q, head, packet)
		if err != nil {
			return err
		}
		if err := n.writeUsedLocked(q, head, written); err != nil {
			return err
		}
		n.pendingRx = n.pendingRx[1:]
		q.lastAvailIdx++
		processed = true
	}
	if processed {
		if !n.queueInterruptSuppressedLocked(q) {
			n.interruptStatus |= intVring
			return n.updateIRQLocked()
		}
	}
	return nil
}

func (n *Net) processOneRXPacketLocked(packet []byte) (bool, error) {
	q := &n.queues[netQueueRX]
	if !q.ready || q.size == 0 || n.mem == nil {
		return false, nil
	}
	_, availIdx, err := n.readAvailHeaderLocked(q)
	if err != nil {
		return false, err
	}
	if q.lastAvailIdx == availIdx {
		return false, nil
	}
	slot := q.lastAvailIdx % q.size
	head, err := n.readAvailRingEntryLocked(q, slot)
	if err != nil {
		return false, err
	}
	written, err := n.writeRXPacketLocked(q, head, packet)
	if err != nil {
		return false, err
	}
	if err := n.writeUsedLocked(q, head, written); err != nil {
		return false, err
	}
	q.lastAvailIdx++
	if !n.queueInterruptSuppressedLocked(q) {
		n.interruptStatus |= intVring
		return true, n.updateIRQLocked()
	}
	return true, nil
}

func (n *Net) readChainLocked(q *queue, head uint16, writable bool) ([]byte, error) {
	out := getNetPacketBuffer()
	index := head
	for i := uint16(0); i < q.size; i++ {
		desc, err := n.readDescriptorLocked(q, index)
		if err != nil {
			releaseNetPacketBuffer(out)
			return nil, err
		}
		isWrite := desc.flags&descFWrite != 0
		if isWrite == writable && desc.length > 0 {
			oldLen := len(out)
			newLen := oldLen + int(desc.length)
			if newLen > cap(out) {
				grown := make([]byte, newLen)
				copy(grown, out)
				releaseNetPacketBuffer(out)
				out = grown[:oldLen]
			}
			out = out[:newLen]
			if err := readGuestMemoryInto(n.mem, desc.addr, out[oldLen:newLen]); err != nil {
				releaseNetPacketBuffer(out)
				return nil, err
			}
		}
		if desc.flags&descFNext == 0 {
			return out, nil
		}
		index = desc.next
	}
	releaseNetPacketBuffer(out)
	return nil, fmt.Errorf("virtio-net descriptor chain loop")
}

func (n *Net) writeRXPacketLocked(q *queue, head uint16, packet []byte) (uint32, error) {
	headerLen := n.netHeaderLenLocked()
	hdr := n.scratch16[:netHeaderLenMergeRX]
	clear(hdr)
	if headerLen == netHeaderLenMergeRX {
		binary.LittleEndian.PutUint16(hdr[10:12], 1)
	}
	totalLen := headerLen + len(packet)
	offset := 0
	index := head
	for i := uint16(0); i < q.size; i++ {
		desc, err := n.readDescriptorLocked(q, index)
		if err != nil {
			return uint32(offset), err
		}
		if desc.flags&descFWrite != 0 && desc.length > 0 && offset < totalLen {
			written, err := n.writeRXChunk(desc.addr, int(desc.length), hdr[:headerLen], packet, offset)
			if err != nil {
				return uint32(offset), err
			}
			offset += written
		}
		if desc.flags&descFNext == 0 {
			if offset < totalLen {
				return uint32(offset), fmt.Errorf("virtio-net RX chain too small: wrote %d of %d bytes", offset, totalLen)
			}
			return uint32(offset), nil
		}
		index = desc.next
	}
	return uint32(offset), fmt.Errorf("virtio-net RX descriptor chain loop")
}

func (n *Net) writeRXChunk(addr uint64, size int, hdr, packet []byte, offset int) (int, error) {
	written := 0
	if offset < len(hdr) {
		h := hdr[offset:]
		if len(h) > size {
			h = h[:size]
		}
		if len(h) > 0 {
			if err := n.mem.WriteIPA(addr, h); err != nil {
				return written, err
			}
			addr += uint64(len(h))
			written += len(h)
		}
	}
	if written < size {
		packetOffset := offset - len(hdr)
		if packetOffset < 0 {
			packetOffset = 0
		}
		if packetOffset < len(packet) {
			p := packet[packetOffset:]
			if len(p) > size-written {
				p = p[:size-written]
			}
			if len(p) > 0 {
				if err := n.mem.WriteIPA(addr, p); err != nil {
					return written, err
				}
				written += len(p)
			}
		}
	}
	return written, nil
}

func (n *Net) readDescriptorLocked(q *queue, index uint16) (descriptor, error) {
	if index >= q.size {
		return descriptor{}, fmt.Errorf("descriptor index %d out of range", index)
	}
	if err := n.ensureQueueCacheLocked(q); err != nil {
		return descriptor{}, err
	}
	offset := int(index) * 16
	var buf []byte
	if len(q.descMem) >= offset+16 {
		buf = q.descMem[offset : offset+16]
	} else {
		if err := n.readIPAInto(q.descAddr+uint64(offset), n.scratch16[:]); err != nil {
			return descriptor{}, err
		}
		buf = n.scratch16[:]
	}
	return descriptor{
		addr:   binary.LittleEndian.Uint64(buf[0:8]),
		length: binary.LittleEndian.Uint32(buf[8:12]),
		flags:  binary.LittleEndian.Uint16(buf[12:14]),
		next:   binary.LittleEndian.Uint16(buf[14:16]),
	}, nil
}

func (n *Net) writeUsedLocked(q *queue, head uint16, usedLen uint32) error {
	slot := q.usedIdx % q.size
	if err := n.ensureQueueCacheLocked(q); err != nil {
		return err
	}
	offset := 4 + int(slot)*8
	if len(q.usedMem) >= offset+8 {
		binary.LittleEndian.PutUint32(q.usedMem[offset:offset+4], uint32(head))
		binary.LittleEndian.PutUint32(q.usedMem[offset+4:offset+8], usedLen)
	} else {
		binary.LittleEndian.PutUint32(n.scratch8[0:4], uint32(head))
		binary.LittleEndian.PutUint32(n.scratch8[4:8], usedLen)
		if err := n.mem.WriteIPA(q.usedAddr+4+uint64(slot)*8, n.scratch8[:]); err != nil {
			return err
		}
	}
	q.usedIdx++
	if len(q.usedMem) >= 4 {
		binary.LittleEndian.PutUint16(q.usedMem[2:4], q.usedIdx)
		return nil
	}
	binary.LittleEndian.PutUint16(n.scratch2[:], q.usedIdx)
	return n.mem.WriteIPA(q.usedAddr+2, n.scratch2[:])
}

func (n *Net) queueInterruptSuppressedLocked(q *queue) bool {
	if q == nil || n.mem == nil || q.availAddr == 0 {
		return false
	}
	header, err := n.mem.ReadIPA(q.availAddr, 2)
	if err != nil || len(header) < 2 {
		return false
	}
	flags := binary.LittleEndian.Uint16(header)
	return flags&vringAvailNoInterrupt != 0
}

func (n *Net) selectedQueueLocked() *queue {
	if n.queueSel >= uint32(len(n.queues)) {
		return nil
	}
	return &n.queues[n.queueSel]
}

func (n *Net) setQueueAddr(target *uint64, value uint32, low bool) {
	if low {
		*target = (*target &^ 0xffffffff) | uint64(value)
	} else {
		*target = (*target & 0xffffffff) | (uint64(value) << 32)
	}
}

func (n *Net) configureLegacyQueueLocked(q *queue, pfn uint32) {
	q.clearCache()
	if q.size == 0 {
		q.size = netQueueSize
	}
	q.ready = true
	q.descAddr = uint64(pfn) * 4096
	q.availAddr = q.descAddr + 16*uint64(q.size)
	used := q.availAddr + 4 + 2*uint64(q.size)
	q.usedAddr = alignVirtio(used, 4096)
	q.lastAvailIdx = 0
	q.usedIdx = 0
}

func (n *Net) updateIRQLocked() error {
	if n.irq == nil {
		return nil
	}
	level := n.interruptStatus != 0
	if n.irqHigh == level {
		return nil
	}
	n.irqHigh = level
	return n.irq.SetIRQ(n.IRQ, level)
}

func (n *Net) Summary() string {
	if n == nil {
		return "virtio-net=<nil>"
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return fmt.Sprintf(
		"virtio-net status=%#x legacy=%t interrupt_status=%#x irq_high=%t pending_rx=%d q0_ready=%t q0_last=%d q0_used=%d q1_ready=%t q1_last=%d q1_used=%d",
		n.status,
		n.legacy,
		n.interruptStatus,
		n.irqHigh,
		len(n.pendingRx),
		n.queues[netQueueRX].ready,
		n.queues[netQueueRX].lastAvailIdx,
		n.queues[netQueueRX].usedIdx,
		n.queues[netQueueTX].ready,
		n.queues[netQueueTX].lastAvailIdx,
		n.queues[netQueueTX].usedIdx,
	)
}

func (n *Net) IRQAsserted() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.irqHigh
}

func (n *Net) configBytesLocked() []byte {
	var cfg [8]byte
	copy(cfg[0:6], n.MAC)
	binary.LittleEndian.PutUint16(cfg[6:8], netStatusLinkUp)
	return cfg[:]
}

func (n *Net) legacyFeaturesLocked() uint64 {
	return n.deviceFeaturesLocked() &^ featureVersion1
}

func (n *Net) deviceFeaturesLocked() uint64 {
	features := featureVersion1 | netFeatureCSUM | netFeatureMAC | netFeatureStatus
	if !n.DisableMergeRX {
		features |= netFeatureMergeRX
	}
	return features
}

func (n *Net) netHeaderLenLocked() int {
	if n.HeaderLength != 0 {
		return n.HeaderLength
	}
	if n.driverFeatures&netFeatureMergeRX != 0 {
		return netHeaderLenMergeRX
	}
	return netHeaderLen
}

func (n *Net) resetLocked() {
	n.deviceFeatureSel = 0
	n.driverFeatureSel = 0
	n.driverFeatures = 0
	n.queueSel = 0
	n.status = 0
	n.interruptStatus = 0
	n.irqHigh = false
	n.legacy = false
	n.configGeneration++
	n.queues = [2]queue{}
	n.pendingRx = nil
}

func fixTXChecksum(frame []byte, headerLen int) error {
	if len(frame) < headerLen {
		return nil
	}
	flags := frame[0]
	if flags&netHdrFlagNeedsChecksum == 0 {
		return nil
	}
	csumStart := int(binary.LittleEndian.Uint16(frame[6:8]))
	csumOffset := int(binary.LittleEndian.Uint16(frame[8:10]))
	packet := frame[headerLen:]
	if csumStart < 0 || csumStart >= len(packet) || csumStart+csumOffset+2 > len(packet) {
		return fmt.Errorf("virtio-net checksum range out of packet: start=%d offset=%d len=%d", csumStart, csumOffset, len(packet))
	}
	sum := internetChecksum(packet[csumStart:])
	binary.BigEndian.PutUint16(packet[csumStart+csumOffset:csumStart+csumOffset+2], sum)
	return nil
}

func internetChecksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}
