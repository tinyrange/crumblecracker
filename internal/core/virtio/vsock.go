package virtio

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	mmioDeviceIDVsock = 19

	vsockQueueRX    = 0
	vsockQueueTX    = 1
	vsockQueueEvent = 2

	vsockQueueSize      = 128
	vsockInterruptVring = 0x1
	vsockDefaultBufSize = 64 * 1024
)

type VsockBackend interface {
	Listen(port uint32) (VsockListener, error)
	Connect(port uint32) (VsockConn, error)
}

type VsockListener interface {
	Accept() (VsockConn, error)
	Close() error
	Port() uint32
}

type VsockConn interface {
	io.ReadWriteCloser
	LocalPort() uint32
	RemotePort() uint32
}

type Vsock struct {
	Base     uint64
	Size     uint64
	IRQ      uint32
	GuestCID uint64

	mu               sync.Mutex
	creditCond       *sync.Cond
	mem              GuestMemory
	irq              IRQController
	backend          VsockBackend
	deviceFeatureSel uint32
	driverFeatureSel uint32
	driverFeatures   uint64
	queueSel         uint32
	status           uint32
	interruptStatus  uint32
	irqHigh          bool
	configGeneration uint32
	queues           [3]queue
	connections      map[vsockConnKey]*vsockConnection
	pendingRx        [][]byte
	closed           chan struct{}
	wg               sync.WaitGroup
	mmioReads        uint64
	mmioWrites       uint64
	txNotifies       uint64
	rxNotifies       uint64
	connectRequests  uint64
	queuedRxPackets  uint64
	irqTransitions   uint64
}

type vsockConnKey struct {
	localPort  uint32
	remotePort uint32
}

type vsockConnection struct {
	key                  vsockConnKey
	state                int
	peerAlloc            uint32
	peerCnt              uint32
	txCnt                uint32
	rxCnt                uint32
	creditRequestPending bool
	backend              VsockConn
}

const (
	vsockConnStateIdle = iota
	vsockConnStateConnecting
	vsockConnStateConnected
	vsockConnStateClosing
)

func NewVsock(base, size uint64, irq uint32, guestCID uint64, backend VsockBackend) *Vsock {
	v := &Vsock{
		Base:     base,
		Size:     size,
		IRQ:      irq,
		GuestCID: guestCID,
		backend:  backend,
		closed:   make(chan struct{}),
	}
	v.creditCond = sync.NewCond(&v.mu)
	v.resetLocked()
	return v
}

func (v *Vsock) Attach(mem GuestMemory, irq IRQController) {
	v.mu.Lock()
	v.mem = mem
	v.irq = irq
	v.mu.Unlock()
	if registrar, ok := mem.(guestMemoryDetachRegistrar); ok {
		registrar.RegisterGuestMemoryDetach(v.Detach)
	}
}

// Detach prevents asynchronous backend readers from touching guest memory
// after the hypervisor starts unmapping it.
func (v *Vsock) Detach() {
	v.mu.Lock()
	v.mem = nil
	v.irq = nil
	v.pendingRx = nil
	v.mu.Unlock()
}

func (v *Vsock) Contains(addr uint64, size int) bool {
	return addr >= v.Base && addr+uint64(size) <= v.Base+v.Size
}

func (v *Vsock) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("virtio@%x", v.Base),
		Properties: map[string]fdt.Property{
			"compatible": {Strings: []string{"virtio,mmio"}},
			"reg":        {U64: []uint64{v.Base, v.Size}},
			"interrupts": {U32: []uint32{0, v.IRQ, 4}},
			"status":     {Strings: []string{"okay"}},
		},
	}
}

func (v *Vsock) Read(addr uint64, size int) (uint64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mmioReads++

	offset := addr - v.Base
	switch offset {
	case regMagicValue:
		return truncateValue(mmioMagicValue, size), nil
	case regVersion:
		return truncateValue(mmioVersion, size), nil
	case regDeviceID:
		return truncateValue(mmioDeviceIDVsock, size), nil
	case regVendorID:
		return truncateValue(mmioVendorID, size), nil
	case regDeviceFeatures:
		if v.deviceFeatureSel == 0 {
			return 0, nil
		}
		if v.deviceFeatureSel == 1 {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regQueueNumMax:
		if v.queueSel < uint32(len(v.queues)) {
			return truncateValue(vsockQueueSize, size), nil
		}
		return 0, nil
	case regQueueNum:
		if q := v.selectedQueueLocked(); q != nil {
			return truncateValue(uint64(q.size), size), nil
		}
		return 0, nil
	case regQueueReady:
		if q := v.selectedQueueLocked(); q != nil && q.ready {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regInterruptStatus:
		return truncateValue(uint64(v.interruptStatus), size), nil
	case regStatus:
		return truncateValue(uint64(v.status), size), nil
	case regConfigGen:
		return truncateValue(uint64(v.configGeneration), size), nil
	}

	if offset >= regConfig && offset+uint64(size) <= regConfig+8 {
		cfg := v.configBytesLocked()
		return truncateValue(readConfigValue(cfg[offset-regConfig:], size), size), nil
	}
	return 0, nil
}

func (v *Vsock) Write(addr uint64, size int, value uint64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mmioWrites++

	offset := addr - v.Base
	switch offset {
	case regDeviceFeatSel:
		v.deviceFeatureSel = uint32(value)
	case regDriverFeatSel:
		v.driverFeatureSel = uint32(value)
	case regDriverFeatures:
		if v.driverFeatureSel == 0 {
			v.driverFeatures = (v.driverFeatures &^ 0xffffffff) | uint64(uint32(value))
		} else if v.driverFeatureSel == 1 {
			v.driverFeatures = (v.driverFeatures & 0xffffffff) | (uint64(uint32(value)) << 32)
		}
	case regQueueSel:
		v.queueSel = uint32(value)
	case regQueueNum:
		if q := v.selectedQueueLocked(); q != nil {
			q.size = uint16(value)
		}
	case regQueueReady:
		if q := v.selectedQueueLocked(); q != nil {
			q.ready = value != 0
			if value == 0 {
				q.lastAvailIdx = 0
				q.usedIdx = 0
			}
		}
	case regQueueDescLow:
		if q := v.selectedQueueLocked(); q != nil {
			v.setQueueAddr(&q.descAddr, uint32(value), true)
		}
	case regQueueDescHigh:
		if q := v.selectedQueueLocked(); q != nil {
			v.setQueueAddr(&q.descAddr, uint32(value), false)
		}
	case regQueueAvailLow:
		if q := v.selectedQueueLocked(); q != nil {
			v.setQueueAddr(&q.availAddr, uint32(value), true)
		}
	case regQueueAvailHigh:
		if q := v.selectedQueueLocked(); q != nil {
			v.setQueueAddr(&q.availAddr, uint32(value), false)
		}
	case regQueueUsedLow:
		if q := v.selectedQueueLocked(); q != nil {
			v.setQueueAddr(&q.usedAddr, uint32(value), true)
		}
	case regQueueUsedHigh:
		if q := v.selectedQueueLocked(); q != nil {
			v.setQueueAddr(&q.usedAddr, uint32(value), false)
		}
	case regInterruptAck:
		v.interruptStatus &^= uint32(value)
		return v.updateIRQLocked()
	case regStatus:
		v.status = uint32(value)
		if v.status == 0 {
			v.resetLocked()
		}
	case regQueueNotify:
		switch value {
		case vsockQueueTX:
			v.txNotifies++
			return v.processTXLocked()
		case vsockQueueRX:
			v.rxNotifies++
			return v.processRXLocked()
		case vsockQueueEvent:
			return nil
		}
	}
	return nil
}

func (v *Vsock) Close() error {
	select {
	case <-v.closed:
		return nil
	default:
		close(v.closed)
	}

	v.mu.Lock()
	for key, conn := range v.connections {
		if conn.backend != nil {
			_ = conn.backend.Close()
		}
		delete(v.connections, key)
	}
	if v.creditCond != nil {
		v.creditCond.Broadcast()
	}
	v.mu.Unlock()
	v.wg.Wait()
	return nil
}

// Poke wakes an idle guest without adding a packet to the vsock queues. This
// lets restored guests make progress during control startup and managed execs
// without consuming credit or writing another control packet.
func (v *Vsock) Poke() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// A backend read can queue a packet just before the guest replenishes the
	// RX ring. If the corresponding queue notification is missed, merely
	// raising an interrupt leaves that packet pending forever because there is
	// no used descriptor for the guest to discover. Retry delivery before the
	// wakeup so periodic keepalives make forward progress.
	if err := v.processRXLocked(); err != nil {
		return err
	}
	if v.irq == nil {
		return nil
	}
	// Report an actual vring interrupt so the guest driver rescans completed
	// receive buffers. A bare line pulse with interruptStatus clear is handled
	// as a spurious interrupt and cannot recover a missed queue wakeup.
	v.interruptStatus |= vsockInterruptVring
	// Reasserting an already-high level does not produce a new wakeup on every
	// hypervisor. Pulse it low first while retaining the pending device status.
	if v.irqHigh {
		if err := v.irq.SetIRQ(v.IRQ, false); err != nil {
			return err
		}
		v.irqHigh = false
		v.irqTransitions++
	}
	return v.updateIRQLocked()
}

func (v *Vsock) Summary() string {
	if v == nil {
		return "virtio-vsock=<nil>"
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return fmt.Sprintf(
		"virtio-vsock mmio_reads=%d mmio_writes=%d status=%#x tx_notify=%d rx_notify=%d connect_requests=%d queued_rx=%d pending_rx=%d irq_transitions=%d irq_high=%t interrupt_status=%#x q0_ready=%t q1_ready=%t q2_ready=%t q0_last=%d q1_last=%d q2_last=%d",
		v.mmioReads,
		v.mmioWrites,
		v.status,
		v.txNotifies,
		v.rxNotifies,
		v.connectRequests,
		v.queuedRxPackets,
		len(v.pendingRx),
		v.irqTransitions,
		v.irqHigh,
		v.interruptStatus,
		v.queues[0].ready,
		v.queues[1].ready,
		v.queues[2].ready,
		v.queues[0].lastAvailIdx,
		v.queues[1].lastAvailIdx,
		v.queues[2].lastAvailIdx,
	)
}

func (v *Vsock) IRQAsserted() bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.interruptStatus != 0 || v.irqHigh
}

func (v *Vsock) Kick() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.interruptStatus |= vsockInterruptVring
	return v.updateIRQLocked()
}

func (v *Vsock) processTXLocked() error {
	q := &v.queues[vsockQueueTX]
	if !q.ready || q.size == 0 || v.mem == nil {
		return nil
	}
	header, err := v.mem.ReadIPA(q.availAddr, 4)
	if err != nil {
		return err
	}
	availIdx := binary.LittleEndian.Uint16(header[2:4])
	for q.lastAvailIdx != availIdx {
		slot := q.lastAvailIdx % q.size
		entry, err := v.mem.ReadIPA(q.availAddr+4+uint64(slot)*2, 2)
		if err != nil {
			return err
		}
		head := binary.LittleEndian.Uint16(entry)
		usedLen, err := v.handleTXPacketLocked(q, head)
		if err != nil {
			return err
		}
		if err := v.writeUsedLocked(q, head, usedLen); err != nil {
			return err
		}
		q.lastAvailIdx++
	}
	if err := v.processRXLocked(); err != nil {
		return err
	}
	v.interruptStatus |= vsockInterruptVring
	return v.updateIRQLocked()
}

func (v *Vsock) handleTXPacketLocked(q *queue, head uint16) (uint32, error) {
	data, err := v.readChainLocked(q, head)
	if err != nil {
		return 0, err
	}
	if len(data) < vsockHeaderSize {
		return uint32(len(data)), nil
	}
	hdr, err := parseVsockHeader(data)
	if err != nil {
		return uint32(len(data)), err
	}
	payload := data[vsockHeaderSize:]
	if uint32(len(payload)) < hdr.Len {
		return uint32(len(data)), fmt.Errorf("vsock payload truncated: have %d want %d", len(payload), hdr.Len)
	}
	payload = payload[:hdr.Len]

	switch hdr.Op {
	case vsockOpRequest:
		v.handleConnectLocked(hdr)
	case vsockOpResponse:
		v.handleResponseLocked(hdr)
	case vsockOpRST:
		v.handleResetLocked(hdr)
	case vsockOpShutdown:
		v.handleShutdownLocked(hdr)
	case vsockOpRW:
		v.handleDataLocked(hdr, payload)
	case vsockOpCreditUpdate:
		v.handleCreditUpdateLocked(hdr)
	case vsockOpCreditRequest:
		v.handleCreditRequestLocked(hdr)
	}
	return uint32(len(data)), nil
}

func (v *Vsock) handleConnectLocked(hdr vsockHeader) {
	v.connectRequests++
	key := vsockConnKey{localPort: hdr.DstPort, remotePort: hdr.SrcPort}
	if v.backend == nil {
		v.sendResetLocked(hdr)
		return
	}
	conn, err := v.backend.Connect(hdr.DstPort)
	if err != nil {
		v.sendResetLocked(hdr)
		return
	}
	v.connections[key] = &vsockConnection{
		key:       key,
		state:     vsockConnStateConnected,
		peerAlloc: hdr.BufAlloc,
		peerCnt:   hdr.FwdCnt,
		backend:   conn,
	}
	v.sendResponseLocked(hdr)
	v.wg.Add(1)
	go v.readFromBackend(conn, key)
}

func (v *Vsock) handleResponseLocked(hdr vsockHeader) {
	key := vsockConnKey{localPort: hdr.DstPort, remotePort: hdr.SrcPort}
	conn, ok := v.connections[key]
	if !ok || conn.state != vsockConnStateConnecting {
		v.sendResetLocked(hdr)
		return
	}
	conn.state = vsockConnStateConnected
	conn.peerAlloc = hdr.BufAlloc
	conn.peerCnt = hdr.FwdCnt
}

func (v *Vsock) handleResetLocked(hdr vsockHeader) {
	key := vsockConnKey{localPort: hdr.DstPort, remotePort: hdr.SrcPort}
	if conn, ok := v.connections[key]; ok {
		if conn.backend != nil {
			_ = conn.backend.Close()
		}
		delete(v.connections, key)
	}
}

func (v *Vsock) handleShutdownLocked(hdr vsockHeader) {
	key := vsockConnKey{localPort: hdr.DstPort, remotePort: hdr.SrcPort}
	conn, ok := v.connections[key]
	if !ok {
		return
	}
	conn.state = vsockConnStateClosing
	if conn.backend != nil {
		_ = conn.backend.Close()
	}
	v.sendResetLocked(hdr)
	delete(v.connections, key)
}

func (v *Vsock) handleDataLocked(hdr vsockHeader, payload []byte) {
	key := vsockConnKey{localPort: hdr.DstPort, remotePort: hdr.SrcPort}
	conn, ok := v.connections[key]
	if !ok || conn.state != vsockConnStateConnected {
		v.sendResetLocked(hdr)
		return
	}
	conn.peerAlloc = hdr.BufAlloc
	conn.peerCnt = hdr.FwdCnt
	if len(payload) > 0 {
		conn.rxCnt += uint32(len(payload))
		if _, err := conn.backend.Write(payload); err != nil {
			v.sendResetLocked(hdr)
			_ = conn.backend.Close()
			delete(v.connections, key)
			return
		}
	}
	v.sendCreditUpdateLocked(conn)
}

func (v *Vsock) handleCreditUpdateLocked(hdr vsockHeader) {
	key := vsockConnKey{localPort: hdr.DstPort, remotePort: hdr.SrcPort}
	conn, ok := v.connections[key]
	if !ok {
		return
	}
	conn.peerAlloc = hdr.BufAlloc
	conn.peerCnt = hdr.FwdCnt
	conn.creditRequestPending = false
	if v.creditCond != nil {
		v.creditCond.Broadcast()
	}
}

func (v *Vsock) handleCreditRequestLocked(hdr vsockHeader) {
	key := vsockConnKey{localPort: hdr.DstPort, remotePort: hdr.SrcPort}
	conn, ok := v.connections[key]
	if !ok {
		return
	}
	v.sendCreditUpdateLocked(conn)
}

func (v *Vsock) readFromBackend(conn VsockConn, key vsockConnKey) {
	defer v.wg.Done()
	buf := make([]byte, 3072)
	for {
		select {
		case <-v.closed:
			return
		default:
		}
		n, err := conn.Read(buf)
		if err != nil {
			select {
			case <-v.closed:
				return
			default:
			}
			v.mu.Lock()
			state, ok := v.connections[key]
			if ok && state.state == vsockConnStateConnected {
				v.queueRxPacketLocked(encodeVsockHeader(vsockHeader{
					SrcCID:  VSockCIDHost,
					DstCID:  v.GuestCID,
					SrcPort: key.localPort,
					DstPort: key.remotePort,
					Type:    vsockTypeStream,
					Op:      vsockOpShutdown,
					Flags:   vsockShutdownRecv | vsockShutdownSend,
				}))
				state.state = vsockConnStateClosing
				_ = v.processRXLocked()
			}
			v.mu.Unlock()
			return
		}
		if n == 0 {
			continue
		}
		for offset := 0; offset < n; {
			v.mu.Lock()
			state, ok := v.connections[key]
			if !ok || state.state != vsockConnStateConnected {
				v.mu.Unlock()
				return
			}
			credit := vsockPeerCredit(state)
			if credit == 0 {
				v.sendCreditRequestLocked(state)
				_ = v.processRXLocked()
				v.creditCond.Wait()
				v.mu.Unlock()
				continue
			}
			chunk := n - offset
			if uint32(chunk) > credit {
				chunk = int(credit)
			}
			v.sendDataLocked(state, buf[offset:offset+chunk])
			_ = v.processRXLocked()
			v.mu.Unlock()
			offset += chunk
		}
	}
}

func vsockPeerCredit(conn *vsockConnection) uint32 {
	if conn == nil || conn.peerAlloc == 0 {
		return 0
	}
	used := conn.txCnt - conn.peerCnt
	if used >= conn.peerAlloc {
		return 0
	}
	return conn.peerAlloc - used
}

func (v *Vsock) processRXLocked() error {
	q := &v.queues[vsockQueueRX]
	if !q.ready || q.size == 0 || v.mem == nil || len(v.pendingRx) == 0 {
		return nil
	}
	header, err := v.mem.ReadIPA(q.availAddr, 4)
	if err != nil {
		return err
	}
	availIdx := binary.LittleEndian.Uint16(header[2:4])
	processed := false
	for q.lastAvailIdx != availIdx && len(v.pendingRx) > 0 {
		slot := q.lastAvailIdx % q.size
		entry, err := v.mem.ReadIPA(q.availAddr+4+uint64(slot)*2, 2)
		if err != nil {
			return err
		}
		head := binary.LittleEndian.Uint16(entry)
		packet := v.pendingRx[0]
		written, err := v.fillChainLocked(q, head, packet)
		if err != nil {
			return err
		}
		if err := v.writeUsedLocked(q, head, written); err != nil {
			return err
		}
		v.pendingRx = v.pendingRx[1:]
		q.lastAvailIdx++
		processed = true
	}
	if processed {
		v.interruptStatus |= vsockInterruptVring
		return v.updateIRQLocked()
	}
	return nil
}

func (v *Vsock) queueRxPacketLocked(packet []byte) {
	v.pendingRx = append(v.pendingRx, append([]byte(nil), packet...))
	v.queuedRxPackets++
}

func (v *Vsock) sendResponseLocked(hdr vsockHeader) {
	v.queueRxPacketLocked(encodeVsockHeader(vsockHeader{
		SrcCID:   VSockCIDHost,
		DstCID:   v.GuestCID,
		SrcPort:  hdr.DstPort,
		DstPort:  hdr.SrcPort,
		Type:     vsockTypeStream,
		Op:       vsockOpResponse,
		BufAlloc: vsockDefaultBufSize,
	}))
}

func (v *Vsock) sendResetLocked(hdr vsockHeader) {
	v.queueRxPacketLocked(encodeVsockHeader(vsockHeader{
		SrcCID:  VSockCIDHost,
		DstCID:  v.GuestCID,
		SrcPort: hdr.DstPort,
		DstPort: hdr.SrcPort,
		Type:    vsockTypeStream,
		Op:      vsockOpRST,
	}))
}

func (v *Vsock) sendCreditUpdateLocked(conn *vsockConnection) {
	v.queueRxPacketLocked(encodeVsockHeader(vsockHeader{
		SrcCID:   VSockCIDHost,
		DstCID:   v.GuestCID,
		SrcPort:  conn.key.localPort,
		DstPort:  conn.key.remotePort,
		Type:     vsockTypeStream,
		Op:       vsockOpCreditUpdate,
		BufAlloc: vsockDefaultBufSize,
		FwdCnt:   conn.rxCnt,
	}))
}

func (v *Vsock) sendCreditRequestLocked(conn *vsockConnection) {
	if conn.creditRequestPending {
		return
	}
	conn.creditRequestPending = true
	v.queueRxPacketLocked(encodeVsockHeader(vsockHeader{
		SrcCID:   VSockCIDHost,
		DstCID:   v.GuestCID,
		SrcPort:  conn.key.localPort,
		DstPort:  conn.key.remotePort,
		Type:     vsockTypeStream,
		Op:       vsockOpCreditRequest,
		BufAlloc: vsockDefaultBufSize,
		FwdCnt:   conn.rxCnt,
	}))
}

func (v *Vsock) sendDataLocked(conn *vsockConnection, data []byte) {
	conn.txCnt += uint32(len(data))
	hdr := encodeVsockHeader(vsockHeader{
		SrcCID:   VSockCIDHost,
		DstCID:   v.GuestCID,
		SrcPort:  conn.key.localPort,
		DstPort:  conn.key.remotePort,
		Len:      uint32(len(data)),
		Type:     vsockTypeStream,
		Op:       vsockOpRW,
		BufAlloc: vsockDefaultBufSize,
		FwdCnt:   conn.rxCnt,
	})
	packet := append(hdr, data...)
	v.queueRxPacketLocked(packet)
}

func (v *Vsock) readChainLocked(q *queue, head uint16) ([]byte, error) {
	var out []byte
	index := head
	for i := uint16(0); i < q.size; i++ {
		desc, err := v.readDescriptorLocked(q, index)
		if err != nil {
			return nil, err
		}
		if desc.flags&descFWrite == 0 && desc.length > 0 {
			chunk, err := v.mem.ReadIPA(desc.addr, int(desc.length))
			if err != nil {
				return nil, err
			}
			out = append(out, chunk...)
		}
		if desc.flags&descFNext == 0 {
			return out, nil
		}
		index = desc.next
	}
	return nil, fmt.Errorf("virtio-vsock descriptor chain loop")
}

func (v *Vsock) fillChainLocked(q *queue, head uint16, packet []byte) (uint32, error) {
	index := head
	offset := 0
	for i := uint16(0); i < q.size; i++ {
		desc, err := v.readDescriptorLocked(q, index)
		if err != nil {
			return 0, err
		}
		if desc.flags&descFWrite != 0 && desc.length > 0 && offset < len(packet) {
			chunk := packet[offset:]
			if len(chunk) > int(desc.length) {
				chunk = chunk[:desc.length]
			}
			if err := v.mem.WriteIPA(desc.addr, chunk); err != nil {
				return 0, err
			}
			offset += len(chunk)
		}
		if desc.flags&descFNext == 0 {
			if offset < len(packet) {
				return uint32(offset), fmt.Errorf("virtio-vsock RX chain too small: wrote %d of %d bytes", offset, len(packet))
			}
			return uint32(offset), nil
		}
		index = desc.next
	}
	return uint32(offset), fmt.Errorf("virtio-vsock RX descriptor chain loop")
}

func (v *Vsock) readDescriptorLocked(q *queue, index uint16) (descriptor, error) {
	if index >= q.size {
		return descriptor{}, fmt.Errorf("descriptor index %d out of range", index)
	}
	buf, err := v.mem.ReadIPA(q.descAddr+uint64(index)*16, 16)
	if err != nil {
		return descriptor{}, err
	}
	return descriptor{
		addr:   binary.LittleEndian.Uint64(buf[0:8]),
		length: binary.LittleEndian.Uint32(buf[8:12]),
		flags:  binary.LittleEndian.Uint16(buf[12:14]),
		next:   binary.LittleEndian.Uint16(buf[14:16]),
	}, nil
}

func (v *Vsock) writeUsedLocked(q *queue, head uint16, usedLen uint32) error {
	slot := q.usedIdx % q.size
	elem := make([]byte, 8)
	binary.LittleEndian.PutUint32(elem[0:4], uint32(head))
	binary.LittleEndian.PutUint32(elem[4:8], usedLen)
	if err := v.mem.WriteIPA(q.usedAddr+4+uint64(slot)*8, elem); err != nil {
		return err
	}
	q.usedIdx++
	idx := make([]byte, 2)
	binary.LittleEndian.PutUint16(idx, q.usedIdx)
	return v.mem.WriteIPA(q.usedAddr+2, idx)
}

func (v *Vsock) updateIRQLocked() error {
	if v.irq == nil {
		return nil
	}
	level := v.interruptStatus != 0
	if v.irqHigh == level {
		if level {
			return v.irq.SetIRQ(v.IRQ, true)
		}
		return nil
	}
	v.irqHigh = level
	v.irqTransitions++
	return v.irq.SetIRQ(v.IRQ, level)
}

func (v *Vsock) selectedQueueLocked() *queue {
	if v.queueSel >= uint32(len(v.queues)) {
		return nil
	}
	return &v.queues[v.queueSel]
}

func (v *Vsock) setQueueAddr(target *uint64, value uint32, low bool) {
	if low {
		*target = (*target &^ 0xffffffff) | uint64(value)
	} else {
		*target = (*target & 0xffffffff) | (uint64(value) << 32)
	}
}

func (v *Vsock) configBytesLocked() []byte {
	var cfg [8]byte
	binary.LittleEndian.PutUint64(cfg[:], v.GuestCID)
	return cfg[:]
}

func (v *Vsock) resetLocked() {
	for _, conn := range v.connections {
		if conn != nil && conn.backend != nil {
			_ = conn.backend.Close()
		}
	}
	v.deviceFeatureSel = 0
	v.driverFeatureSel = 0
	v.driverFeatures = 0
	v.queueSel = 0
	v.status = 0
	v.interruptStatus = 0
	v.irqHigh = false
	v.configGeneration++
	v.queues = [3]queue{}
	v.connections = make(map[vsockConnKey]*vsockConnection)
	v.pendingRx = nil
	if v.creditCond != nil {
		v.creditCond.Broadcast()
	}
}

type SimpleVsockBackend struct {
	mu        sync.Mutex
	listeners map[uint32]*simpleVsockListener
}

func NewSimpleVsockBackend() *SimpleVsockBackend {
	return &SimpleVsockBackend{listeners: make(map[uint32]*simpleVsockListener)}
}

func (b *SimpleVsockBackend) Listen(port uint32) (VsockListener, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.listeners[port]; exists {
		return nil, fmt.Errorf("port %d already in use", port)
	}
	l := &simpleVsockListener{
		port:   port,
		conns:  make(chan *simpleVsockConn, 16),
		closed: make(chan struct{}),
	}
	b.listeners[port] = l
	return l, nil
}

func (b *SimpleVsockBackend) Connect(port uint32) (VsockConn, error) {
	b.mu.Lock()
	l, ok := b.listeners[port]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no listener on port %d", port)
	}
	clientSide := &simpleVsockConn{
		localPort:  0,
		remotePort: port,
		readCh:     make(chan []byte, 64),
		closed:     make(chan struct{}),
	}
	serverSide := &simpleVsockConn{
		localPort:  port,
		remotePort: 0,
		readCh:     make(chan []byte, 64),
		closed:     make(chan struct{}),
	}
	clientSide.peer = serverSide
	serverSide.peer = clientSide
	select {
	case l.conns <- serverSide:
		return clientSide, nil
	case <-l.closed:
		return nil, fmt.Errorf("listener closed")
	}
}

type simpleVsockListener struct {
	port   uint32
	conns  chan *simpleVsockConn
	closed chan struct{}
}

func (l *simpleVsockListener) Accept() (VsockConn, error) {
	select {
	case conn := <-l.conns:
		conn.readBuf = nil
		return conn, nil
	case <-l.closed:
		return nil, fmt.Errorf("listener closed")
	}
}

func (l *simpleVsockListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *simpleVsockListener) Port() uint32 {
	return l.port
}

type simpleVsockConn struct {
	localPort  uint32
	remotePort uint32
	peer       *simpleVsockConn
	readCh     chan []byte
	closed     chan struct{}
	readBuf    []byte
}

func (c *simpleVsockConn) Read(b []byte) (int, error) {
	if len(c.readBuf) > 0 {
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}
	select {
	case data := <-c.readCh:
		n := copy(b, data)
		if n < len(data) {
			c.readBuf = data[n:]
		}
		return n, nil
	case <-c.closed:
		return 0, io.EOF
	case <-c.peer.closed:
		return 0, io.EOF
	}
}

func (c *simpleVsockConn) Write(b []byte) (int, error) {
	if c.peer == nil {
		return 0, fmt.Errorf("no peer")
	}
	data := append([]byte(nil), b...)
	select {
	case c.peer.readCh <- data:
		return len(b), nil
	case <-c.peer.closed:
		return 0, fmt.Errorf("peer closed")
	case <-c.closed:
		return 0, fmt.Errorf("connection closed")
	}
}

func (c *simpleVsockConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *simpleVsockConn) LocalPort() uint32 {
	return c.localPort
}

func (c *simpleVsockConn) RemotePort() uint32 {
	return c.remotePort
}
