package virtio

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	mmioDeviceIDRNG = 4
	rngQueue        = 0
)

type RNG struct {
	Base       uint64
	Size       uint64
	IRQ        uint32
	Reader     io.Reader
	LegacyMMIO bool

	mu               sync.Mutex
	mem              GuestMemory
	irq              IRQController
	deviceFeatureSel uint32
	driverFeatureSel uint32
	driverFeatures   uint64
	queueSel         uint32
	status           uint32
	interruptStatus  uint32
	irqHigh          bool
	configGeneration uint32
	queue            queue
}

func NewRNG(base, size uint64, irq uint32) *RNG {
	r := &RNG{
		Base:   base,
		Size:   size,
		IRQ:    irq,
		Reader: rand.Reader,
	}
	r.resetLocked()
	return r
}

func (r *RNG) Attach(mem GuestMemory, irq IRQController) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mem = mem
	r.irq = irq
}

func (r *RNG) Contains(addr uint64, size int) bool {
	return addr >= r.Base && addr+uint64(size) <= r.Base+r.Size
}

func (r *RNG) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("virtio@%x", r.Base),
		Properties: map[string]fdt.Property{
			"compatible":   {Strings: []string{"virtio,mmio"}},
			"dma-coherent": {Flag: true},
			"reg":          {U64: []uint64{r.Base, r.Size}},
			"interrupts":   {U32: []uint32{0, r.IRQ, 4}},
			"status":       {Strings: []string{"okay"}},
		},
	}
}

func (r *RNG) Read(addr uint64, size int) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	offset := addr - r.Base
	switch offset {
	case regMagicValue:
		return truncateValue(mmioMagicValue, size), nil
	case regVersion:
		return truncateValue(uint64(mmioTransportVersion(r.LegacyMMIO)), size), nil
	case regDeviceID:
		return truncateValue(mmioDeviceIDRNG, size), nil
	case regVendorID:
		return truncateValue(mmioVendorID, size), nil
	case regDeviceFeatures:
		if r.LegacyMMIO {
			return 0, nil
		}
		if r.deviceFeatureSel == 1 {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regQueueNumMax:
		if r.queueSel == rngQueue {
			return truncateValue(256, size), nil
		}
		return 0, nil
	case regQueueNum:
		if r.queueSel == rngQueue {
			return truncateValue(uint64(r.queue.size), size), nil
		}
		return 0, nil
	case regQueueReady:
		if r.LegacyMMIO {
			return 0, nil
		}
		if r.queueSel == rngQueue && r.queue.ready {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regQueuePFN:
		if r.LegacyMMIO && r.queueSel == rngQueue && r.queue.ready {
			return truncateValue(r.queue.descAddr/4096, size), nil
		}
		return 0, nil
	case regInterruptStatus:
		return truncateValue(uint64(r.interruptStatus), size), nil
	case regStatus:
		return truncateValue(uint64(r.status), size), nil
	case regConfigGen:
		return truncateValue(uint64(r.configGeneration), size), nil
	default:
		return 0, nil
	}
}

func (r *RNG) Write(addr uint64, size int, value uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	offset := addr - r.Base
	switch offset {
	case regDeviceFeatSel:
		r.deviceFeatureSel = uint32(value)
	case regDriverFeatSel:
		r.driverFeatureSel = uint32(value)
	case regDriverFeatures:
		if r.driverFeatureSel == 0 {
			r.driverFeatures = (r.driverFeatures &^ 0xffffffff) | uint64(uint32(value))
		} else if r.driverFeatureSel == 1 {
			r.driverFeatures = (r.driverFeatures & 0xffffffff) | (uint64(uint32(value)) << 32)
		}
	case regQueueSel:
		r.queueSel = uint32(value)
	case regQueueNum:
		if r.queueSel == rngQueue {
			r.queue.size = uint16(value)
		}
	case regQueueReady:
		if r.LegacyMMIO {
			return nil
		}
		if r.queueSel == rngQueue {
			r.queue.ready = value != 0
			if value == 0 {
				r.queue.lastAvailIdx = 0
				r.queue.usedIdx = 0
			}
		}
	case regGuestPageSize, regQueueAlign:
		if r.LegacyMMIO {
			return nil
		}
	case regQueuePFN:
		if r.LegacyMMIO && r.queueSel == rngQueue {
			if value == 0 {
				r.queue.ready = false
				r.queue.descAddr = 0
				r.queue.availAddr = 0
				r.queue.usedAddr = 0
				return nil
			}
			r.configureLegacyQueueLocked(uint32(value))
		}
	case regQueueDescLow:
		if r.queueSel == rngQueue {
			r.setQueueAddr(&r.queue.descAddr, uint32(value), true)
		}
	case regQueueDescHigh:
		if r.queueSel == rngQueue {
			r.setQueueAddr(&r.queue.descAddr, uint32(value), false)
		}
	case regQueueAvailLow:
		if r.queueSel == rngQueue {
			r.setQueueAddr(&r.queue.availAddr, uint32(value), true)
		}
	case regQueueAvailHigh:
		if r.queueSel == rngQueue {
			r.setQueueAddr(&r.queue.availAddr, uint32(value), false)
		}
	case regQueueUsedLow:
		if r.queueSel == rngQueue {
			r.setQueueAddr(&r.queue.usedAddr, uint32(value), true)
		}
	case regQueueUsedHigh:
		if r.queueSel == rngQueue {
			r.setQueueAddr(&r.queue.usedAddr, uint32(value), false)
		}
	case regInterruptAck:
		r.interruptStatus &^= uint32(value)
		return r.updateIRQLocked()
	case regStatus:
		r.status = uint32(value)
		if r.status == 0 {
			r.resetLocked()
		}
	case regQueueNotify:
		if int(value) == rngQueue {
			if err := r.processQueueLocked(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *RNG) processQueueLocked() error {
	q := &r.queue
	if !q.ready || q.size == 0 || r.mem == nil {
		return nil
	}

	header, err := r.mem.ReadIPA(q.availAddr, 4)
	if err != nil {
		return err
	}
	availIdx := binary.LittleEndian.Uint16(header[2:4])
	for q.lastAvailIdx != availIdx {
		slot := q.lastAvailIdx % q.size
		entry, err := r.mem.ReadIPA(q.availAddr+4+uint64(slot)*2, 2)
		if err != nil {
			return err
		}
		head := binary.LittleEndian.Uint16(entry)
		written, err := r.fillChainLocked(q, head)
		if err != nil {
			return err
		}
		if err := r.writeUsedLocked(q, head, written); err != nil {
			return err
		}
		q.lastAvailIdx++
	}

	r.interruptStatus |= intVring
	return r.updateIRQLocked()
}

func (r *RNG) fillChainLocked(q *queue, head uint16) (uint32, error) {
	var written uint32
	index := head
	for i := uint16(0); i < q.size; i++ {
		desc, err := r.readDescriptorLocked(q, index)
		if err != nil {
			return written, err
		}
		if desc.flags&descFWrite != 0 && desc.length > 0 {
			buf := make([]byte, desc.length)
			if _, err := io.ReadFull(r.Reader, buf); err != nil {
				return written, fmt.Errorf("read random bytes: %w", err)
			}
			if err := r.mem.WriteIPA(desc.addr, buf); err != nil {
				return written, err
			}
			written += desc.length
		}
		if desc.flags&descFNext == 0 {
			return written, nil
		}
		index = desc.next
	}
	return written, fmt.Errorf("virtio-rng descriptor chain loop")
}

func (r *RNG) readDescriptorLocked(q *queue, index uint16) (descriptor, error) {
	if index >= q.size {
		return descriptor{}, fmt.Errorf("descriptor index %d out of range", index)
	}
	buf, err := r.mem.ReadIPA(q.descAddr+uint64(index)*16, 16)
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

func (r *RNG) writeUsedLocked(q *queue, head uint16, usedLen uint32) error {
	slot := q.usedIdx % q.size
	elem := make([]byte, 8)
	binary.LittleEndian.PutUint32(elem[0:4], uint32(head))
	binary.LittleEndian.PutUint32(elem[4:8], usedLen)
	if err := r.mem.WriteIPA(q.usedAddr+4+uint64(slot)*8, elem); err != nil {
		return err
	}
	q.usedIdx++
	idx := make([]byte, 2)
	binary.LittleEndian.PutUint16(idx, q.usedIdx)
	return r.mem.WriteIPA(q.usedAddr+2, idx)
}

func (r *RNG) updateIRQLocked() error {
	if r.irq == nil {
		return nil
	}
	level := r.interruptStatus != 0
	if r.irqHigh == level {
		return nil
	}
	r.irqHigh = level
	return r.irq.SetIRQ(r.IRQ, level)
}

func (r *RNG) IRQAsserted() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interruptStatus != 0 || r.irqHigh
}

func (r *RNG) setQueueAddr(target *uint64, value uint32, low bool) {
	if r.queueSel != rngQueue {
		return
	}
	if low {
		*target = (*target &^ 0xffffffff) | uint64(value)
	} else {
		*target = (*target & 0xffffffff) | (uint64(value) << 32)
	}
}

func (r *RNG) configureLegacyQueueLocked(pfn uint32) {
	q := &r.queue
	if q.size == 0 {
		q.size = 256
	}
	q.ready = true
	q.descAddr = uint64(pfn) * 4096
	q.availAddr = q.descAddr + 16*uint64(q.size)
	used := q.availAddr + 4 + 2*uint64(q.size)
	q.usedAddr = alignVirtio(used, 4096)
	q.lastAvailIdx = 0
	q.usedIdx = 0
}

func (r *RNG) resetLocked() {
	r.deviceFeatureSel = 0
	r.driverFeatureSel = 0
	r.driverFeatures = 0
	r.queueSel = 0
	r.status = 0
	r.interruptStatus = 0
	r.irqHigh = false
	r.configGeneration++
	r.queue = queue{}
}
