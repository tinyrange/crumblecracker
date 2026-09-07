package virtio

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	mmioMagicValue      = 0x74726976
	mmioVersion         = 2
	mmioVendorID        = 0x554d4551
	mmioDeviceIDConsole = 3

	regMagicValue           = 0x000
	regVersion              = 0x004
	regDeviceID             = 0x008
	regVendorID             = 0x00c
	regDeviceFeatures       = 0x010
	regDeviceFeatSel        = 0x014
	regDriverFeatures       = 0x020
	regDriverFeatSel        = 0x024
	regGuestPageSize        = 0x028
	regQueueSel             = 0x030
	regQueueNumMax          = 0x034
	regQueueNum             = 0x038
	regQueueAlign           = 0x03c
	regQueuePFN             = 0x040
	regQueueReady           = 0x044
	regQueueNotify          = 0x050
	regInterruptStatus      = 0x060
	regInterruptAck         = 0x064
	regStatus               = 0x070
	regQueueDescLow         = 0x080
	regQueueDescHigh        = 0x084
	regQueueAvailLow        = 0x090
	regQueueAvailHigh       = 0x094
	regQueueUsedLow         = 0x0a0
	regQueueUsedHigh        = 0x0a4
	regSharedMemorySel      = 0x0ac
	regSharedMemoryLenLow   = 0x0b0
	regSharedMemoryLenHigh  = 0x0b4
	regSharedMemoryBaseLow  = 0x0b8
	regSharedMemoryBaseHigh = 0x0bc
	regConfigGen            = 0x0fc
	regConfig               = 0x100

	featureVersion1 = uint64(1) << 32
	featureSize     = uint64(1) << 0

	intVring  = 0x1
	intConfig = 0x2

	queueRx = 0
	queueTx = 1

	descFNext  = 1
	descFWrite = 2
)

func mmioTransportVersion(legacy bool) uint32 {
	if legacy {
		return 1
	}
	return mmioVersion
}

type GuestMemory interface {
	ReadIPA(addr uint64, size int) ([]byte, error)
	WriteIPA(addr uint64, data []byte) error
}

type guestMemoryReaderInto interface {
	ReadIPAInto(addr uint64, dst []byte) error
}

type IRQController interface {
	SetIRQ(irq uint32, level bool) error
}

type Console struct {
	Base uint64
	Size uint64
	IRQ  uint32
	Out  io.Writer

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
	queues           [2]queue
}

type queue struct {
	size         uint16
	ready        bool
	descAddr     uint64
	availAddr    uint64
	usedAddr     uint64
	lastAvailIdx uint16
	usedIdx      uint16
	noNotify     bool
	descMem      []byte
	availMem     []byte
	usedMem      []byte
}

func (q *queue) clearCache() {
	q.descMem = nil
	q.availMem = nil
	q.usedMem = nil
}

type descriptor struct {
	addr   uint64
	length uint32
	flags  uint16
	next   uint16
}

func NewConsole(base, size uint64, irq uint32, out io.Writer) *Console {
	c := &Console{
		Base: base,
		Size: size,
		IRQ:  irq,
		Out:  out,
	}
	c.resetLocked()
	return c
}

func (c *Console) Attach(mem GuestMemory, irq IRQController) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mem = mem
	c.irq = irq
}

func (c *Console) Contains(addr uint64, size int) bool {
	return addr >= c.Base && addr+uint64(size) <= c.Base+c.Size
}

func (c *Console) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("virtio@%x", c.Base),
		Properties: map[string]fdt.Property{
			"compatible": {Strings: []string{"virtio,mmio"}},
			"reg":        {U64: []uint64{c.Base, c.Size}},
			"interrupts": {U32: []uint32{0, c.IRQ, 4}},
			"status":     {Strings: []string{"okay"}},
		},
	}
}

func (c *Console) Read(addr uint64, size int) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	offset := addr - c.Base
	switch offset {
	case regMagicValue:
		return truncateValue(mmioMagicValue, size), nil
	case regVersion:
		return truncateValue(mmioVersion, size), nil
	case regDeviceID:
		return truncateValue(mmioDeviceIDConsole, size), nil
	case regVendorID:
		return truncateValue(mmioVendorID, size), nil
	case regDeviceFeatures:
		if c.deviceFeatureSel == 0 {
			return truncateValue(featureSize, size), nil
		}
		if c.deviceFeatureSel == 1 {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regQueueNumMax:
		if c.queueSel < uint32(len(c.queues)) {
			return truncateValue(256, size), nil
		}
		return 0, nil
	case regQueueNum:
		if c.queueSel < uint32(len(c.queues)) {
			return truncateValue(uint64(c.queues[c.queueSel].size), size), nil
		}
		return 0, nil
	case regQueueReady:
		if c.queueSel < uint32(len(c.queues)) && c.queues[c.queueSel].ready {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regInterruptStatus:
		return truncateValue(uint64(c.interruptStatus), size), nil
	case regStatus:
		return truncateValue(uint64(c.status), size), nil
	case regConfigGen:
		return truncateValue(uint64(c.configGeneration), size), nil
	}

	if offset >= regConfig && offset+uint64(size) <= regConfig+12 {
		cfg := c.configBytesLocked()
		return truncateValue(readConfigValue(cfg[offset-regConfig:], size), size), nil
	}
	return 0, nil
}

func (c *Console) Write(addr uint64, size int, value uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	offset := addr - c.Base
	switch offset {
	case regDeviceFeatSel:
		c.deviceFeatureSel = uint32(value)
	case regDriverFeatSel:
		c.driverFeatureSel = uint32(value)
	case regDriverFeatures:
		if c.driverFeatureSel == 0 {
			c.driverFeatures = (c.driverFeatures &^ 0xffffffff) | uint64(uint32(value))
		} else if c.driverFeatureSel == 1 {
			c.driverFeatures = (c.driverFeatures & 0xffffffff) | (uint64(uint32(value)) << 32)
		}
	case regQueueSel:
		c.queueSel = uint32(value)
	case regQueueNum:
		if q := c.selectedQueueLocked(); q != nil {
			q.size = uint16(value)
		}
	case regQueueReady:
		if q := c.selectedQueueLocked(); q != nil {
			q.ready = value != 0
			if value == 0 {
				q.lastAvailIdx = 0
				q.usedIdx = 0
			}
		}
	case regQueueDescLow:
		if q := c.selectedQueueLocked(); q != nil {
			c.setQueueAddr(&q.descAddr, uint32(value), true)
		}
	case regQueueDescHigh:
		if q := c.selectedQueueLocked(); q != nil {
			c.setQueueAddr(&q.descAddr, uint32(value), false)
		}
	case regQueueAvailLow:
		if q := c.selectedQueueLocked(); q != nil {
			c.setQueueAddr(&q.availAddr, uint32(value), true)
		}
	case regQueueAvailHigh:
		if q := c.selectedQueueLocked(); q != nil {
			c.setQueueAddr(&q.availAddr, uint32(value), false)
		}
	case regQueueUsedLow:
		if q := c.selectedQueueLocked(); q != nil {
			c.setQueueAddr(&q.usedAddr, uint32(value), true)
		}
	case regQueueUsedHigh:
		if q := c.selectedQueueLocked(); q != nil {
			c.setQueueAddr(&q.usedAddr, uint32(value), false)
		}
	case regInterruptAck:
		c.interruptStatus &^= uint32(value)
		return c.updateIRQLocked()
	case regStatus:
		c.status = uint32(value)
		if c.status == 0 {
			c.resetLocked()
		}
	case regQueueNotify:
		if int(value) == queueTx {
			if err := c.processTXLocked(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Console) processTXLocked() error {
	q := &c.queues[queueTx]
	if !q.ready || q.size == 0 || c.mem == nil {
		return nil
	}

	header, err := c.mem.ReadIPA(q.availAddr, 4)
	if err != nil {
		return err
	}
	availIdx := binary.LittleEndian.Uint16(header[2:4])
	for q.lastAvailIdx != availIdx {
		slot := q.lastAvailIdx % q.size
		entry, err := c.mem.ReadIPA(q.availAddr+4+uint64(slot)*2, 2)
		if err != nil {
			return err
		}
		head := binary.LittleEndian.Uint16(entry)
		data, err := c.readChainLocked(q, head)
		if err != nil {
			return err
		}
		if len(data) > 0 && c.Out != nil {
			if _, err := c.Out.Write(data); err != nil {
				return fmt.Errorf("write console output: %w", err)
			}
		}
		if err := c.writeUsedLocked(q, head, uint32(len(data))); err != nil {
			return err
		}
		q.lastAvailIdx++
	}

	c.interruptStatus |= intVring
	return c.updateIRQLocked()
}

func (c *Console) readChainLocked(q *queue, head uint16) ([]byte, error) {
	var out []byte
	index := head
	for i := uint16(0); i < q.size; i++ {
		desc, err := c.readDescriptorLocked(q, index)
		if err != nil {
			return nil, err
		}
		if desc.flags&descFWrite == 0 && desc.length > 0 {
			chunk, err := c.mem.ReadIPA(desc.addr, int(desc.length))
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
	return nil, fmt.Errorf("virtio-console descriptor chain loop")
}

func (c *Console) readDescriptorLocked(q *queue, index uint16) (descriptor, error) {
	if index >= q.size {
		return descriptor{}, fmt.Errorf("descriptor index %d out of range", index)
	}
	buf, err := c.mem.ReadIPA(q.descAddr+uint64(index)*16, 16)
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

func (c *Console) writeUsedLocked(q *queue, head uint16, usedLen uint32) error {
	slot := q.usedIdx % q.size
	elem := make([]byte, 8)
	binary.LittleEndian.PutUint32(elem[0:4], uint32(head))
	binary.LittleEndian.PutUint32(elem[4:8], usedLen)
	if err := c.mem.WriteIPA(q.usedAddr+4+uint64(slot)*8, elem); err != nil {
		return err
	}
	q.usedIdx++
	idx := make([]byte, 2)
	binary.LittleEndian.PutUint16(idx, q.usedIdx)
	return c.mem.WriteIPA(q.usedAddr+2, idx)
}

func (c *Console) updateIRQLocked() error {
	if c.irq == nil {
		return nil
	}
	level := c.interruptStatus != 0
	if c.irqHigh == level {
		return nil
	}
	c.irqHigh = level
	return c.irq.SetIRQ(c.IRQ, level)
}

func (c *Console) setQueueAddr(target *uint64, value uint32, low bool) {
	if c.queueSel >= uint32(len(c.queues)) {
		return
	}
	if low {
		*target = (*target &^ 0xffffffff) | uint64(value)
	} else {
		*target = (*target & 0xffffffff) | (uint64(value) << 32)
	}
}

func (c *Console) resetLocked() {
	c.deviceFeatureSel = 0
	c.driverFeatureSel = 0
	c.driverFeatures = 0
	c.queueSel = 0
	c.status = 0
	c.interruptStatus = 0
	c.irqHigh = false
	c.configGeneration++
	c.queues = [2]queue{}
}

func (c *Console) selectedQueueLocked() *queue {
	if c.queueSel >= uint32(len(c.queues)) {
		return nil
	}
	return &c.queues[c.queueSel]
}

func (c *Console) configBytesLocked() []byte {
	var cfg [12]byte
	binary.LittleEndian.PutUint16(cfg[0:2], 80)
	binary.LittleEndian.PutUint16(cfg[2:4], 25)
	binary.LittleEndian.PutUint32(cfg[4:8], 1)
	binary.LittleEndian.PutUint32(cfg[8:12], 0)
	return cfg[:]
}

func readConfigValue(buf []byte, size int) uint64 {
	switch size {
	case 1:
		return uint64(buf[0])
	case 2:
		return uint64(binary.LittleEndian.Uint16(buf[:2]))
	case 4:
		return uint64(binary.LittleEndian.Uint32(buf[:4]))
	case 8:
		return binary.LittleEndian.Uint64(buf[:8])
	default:
		var out uint64
		for i := size - 1; i >= 0; i-- {
			out = (out << 8) | uint64(buf[i])
		}
		return out
	}
}

func truncateValue(value uint64, size int) uint64 {
	switch size {
	case 1:
		return value & 0xff
	case 2:
		return value & 0xffff
	case 4:
		return value & 0xffffffff
	default:
		return value
	}
}
