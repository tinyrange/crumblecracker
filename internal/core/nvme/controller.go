package nvme

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

const (
	MMIOSize = 0x4000

	regCAP   = 0x00
	regVS    = 0x08
	regINTMS = 0x0c
	regINTMC = 0x10
	regCC    = 0x14
	regCSTS  = 0x1c
	regAQA   = 0x24
	regASQ   = 0x28
	regACQ   = 0x30

	doorbellBase = 0x1000

	adminQueueID = 0
	blockSize    = 512

	adminDeleteSQ    = 0x00
	adminCreateSQ    = 0x01
	adminGetLogPage  = 0x02
	adminDeleteCQ    = 0x04
	adminCreateCQ    = 0x05
	adminIdentify    = 0x06
	adminSetFeatures = 0x09
	adminGetFeatures = 0x0a
	adminAsyncEvent  = 0x0c

	ioFlush = 0x00
	ioWrite = 0x01
	ioRead  = 0x02
)

type Controller struct {
	Base uint64
	Size uint64
	IRQ  uint32

	mu      sync.Mutex
	mem     virtio.GuestMemory
	irq     virtio.IRQController
	backend virtio.BlockBackend

	cc      uint32
	csts    uint32
	intMask uint32
	aqa     uint32
	asq     uint64
	acq     uint64
	pageSz  uint64
	irqHigh bool
	queues  map[uint16]*queue
	scratch []byte
	prps    []byte

	msi msiConfig

	readOps    uint64
	readBytes  uint64
	writeOps   uint64
	writeBytes uint64
	lastTrace  time.Time
}

type queue struct {
	id         uint16
	size       uint16
	sqAddr     uint64
	cqAddr     uint64
	cqID       uint16
	sqHead     uint16
	sqTail     uint16
	cqHead     uint16
	cqTail     uint16
	cqPending  uint16
	cqPhase    bool
	interrupts bool
	sqMem      []byte
	cqMem      []byte
}

type command struct {
	opcode uint8
	cid    uint16
	nsid   uint32
	prp1   uint64
	prp2   uint64
	cdw10  uint32
	cdw11  uint32
	cdw12  uint32
	cdw13  uint32
}

var errPRPNotContiguous = errors.New("nvme PRP is not contiguous")

type guestMemoryReaderInto interface {
	ReadIPAInto(addr uint64, dst []byte) error
}

type guestMemorySlicer interface {
	SliceIPA(addr uint64, size int) ([]byte, error)
}

type msiIRQController interface {
	SetMSI(addr uint64, data uint32) error
}

type msiConfig struct {
	enabled bool
	addr    uint64
	data    uint32
}

func NewController(backend virtio.BlockBackend) *Controller {
	c := &Controller{
		Size:    MMIOSize,
		backend: backend,
		pageSz:  4096,
		queues:  make(map[uint16]*queue),
	}
	c.resetLocked()
	return c
}

func (c *Controller) Attach(mem virtio.GuestMemory, irq virtio.IRQController) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mem = mem
	c.irq = irq
}

func (c *Controller) ConfigureMSI(enabled bool, addr uint64, data uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msi = msiConfig{enabled: enabled, addr: addr, data: data}
}

func (c *Controller) ReadMMIO(offset uint64, size int) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch offset {
	case regCAP:
		return truncate(c.capability(), size), nil
	case regCAP + 4:
		return truncate(c.capability()>>32, size), nil
	case regVS:
		return truncate(0x00010400, size), nil
	case regINTMS:
		return truncate(uint64(c.intMask), size), nil
	case regCC:
		return truncate(uint64(c.cc), size), nil
	case regCSTS:
		return truncate(uint64(c.csts), size), nil
	case regAQA:
		return truncate(uint64(c.aqa), size), nil
	case regASQ:
		return truncate(c.asq, size), nil
	case regASQ + 4:
		return truncate(c.asq>>32, size), nil
	case regACQ:
		return truncate(c.acq, size), nil
	case regACQ + 4:
		return truncate(c.acq>>32, size), nil
	default:
		return 0, nil
	}
}

func (c *Controller) WriteMMIO(offset uint64, size int, value uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch offset {
	case regINTMS:
		c.intMask |= uint32(value)
		return c.updateIRQLocked()
	case regINTMC:
		c.intMask &^= uint32(value)
		return c.updateIRQLocked()
	case regCC:
		wasEnabled := c.cc&1 != 0
		c.cc = uint32(value)
		if c.cc&1 == 0 {
			c.csts = 0
			if wasEnabled {
				c.queues = make(map[uint16]*queue)
				c.irqHigh = false
				if c.irq != nil {
					return c.irq.SetIRQ(c.IRQ, false)
				}
			}
			return nil
		}
		mps := (c.cc >> 7) & 0xf
		c.pageSz = 4096 << mps
		c.configureAdminQueueLocked()
		c.csts = 1
	case regAQA:
		c.aqa = uint32(value)
	case regASQ:
		c.asq = (c.asq & 0xffffffff00000000) | uint64(uint32(value))
	case regASQ + 4:
		c.asq = (c.asq & 0xffffffff) | uint64(uint32(value))<<32
	case regACQ:
		c.acq = (c.acq & 0xffffffff00000000) | uint64(uint32(value))
	case regACQ + 4:
		c.acq = (c.acq & 0xffffffff) | uint64(uint32(value))<<32
	default:
		if offset >= doorbellBase {
			return c.writeDoorbellLocked(offset-doorbellBase, uint32(value))
		}
	}
	return nil
}

func (c *Controller) capability() uint64 {
	return 0x3fff | (1 << 16) | (0xf << 24) | (1 << 37)
}

func (c *Controller) resetLocked() {
	c.cc = 0
	c.csts = 0
	c.intMask = 0
	c.aqa = 0
	c.asq = 0
	c.acq = 0
	c.pageSz = 4096
	c.queues = make(map[uint16]*queue)
	c.irqHigh = false
	if c.irq != nil {
		_ = c.irq.SetIRQ(c.IRQ, false)
	}
}

func (c *Controller) configureAdminQueueLocked() {
	sqSize := uint16(c.aqa&0xfff) + 1
	cqSize := uint16((c.aqa>>16)&0xfff) + 1
	size := sqSize
	if cqSize < size {
		size = cqSize
	}
	if size == 0 {
		size = 1
	}
	q := &queue{
		id:         adminQueueID,
		size:       size,
		sqAddr:     c.asq,
		cqAddr:     c.acq,
		cqPhase:    true,
		interrupts: true,
	}
	c.cacheQueueMemoryLocked(q)
	c.queues[adminQueueID] = q
}

func (c *Controller) writeDoorbellLocked(offset uint64, value uint32) error {
	index := offset / 4
	qid := uint16(index / 2)
	q := c.queues[qid]
	if q == nil {
		return nil
	}
	if index%2 == 0 {
		q.sqTail = uint16(value)
		return c.processSubmissionQueueLocked(q)
	}
	q.advanceCompletionHeadLocked(uint16(value))
	return c.updateIRQLocked()
}

func (c *Controller) processSubmissionQueueLocked(q *queue) error {
	for q.sqHead != q.sqTail {
		raw := c.queueSubmissionEntry(q, q.sqHead)
		if raw == nil {
			var err error
			raw, err = c.mem.ReadIPA(q.sqAddr+uint64(q.sqHead)*64, 64)
			if err != nil {
				return fmt.Errorf("read nvme sq%d entry head=%d addr=%#x: %w", q.id, q.sqHead, q.sqAddr, err)
			}
		}
		cmd := parseCommand(raw)
		if q.id == adminQueueID && cmd.opcode == adminAsyncEvent {
			q.sqHead = (q.sqHead + 1) % q.size
			continue
		}
		result, status, err := c.executeCommandLocked(q, cmd)
		if err != nil {
			return err
		}
		q.sqHead = (q.sqHead + 1) % q.size
		cq := c.queues[q.cqID]
		if q.id == adminQueueID {
			cq = q
		}
		if cq == nil {
			return fmt.Errorf("nvme completion queue %d missing", q.cqID)
		}
		if err := c.writeCompletionLocked(cq, q.id, q.sqHead, cmd.cid, result, status); err != nil {
			return fmt.Errorf("write nvme completion for sq%d cq%d opcode=%#x cid=%d: %w", q.id, cq.id, cmd.opcode, cmd.cid, err)
		}
	}
	return c.updateIRQLocked()
}

func parseCommand(raw []byte) command {
	return command{
		opcode: raw[0],
		cid:    binary.LittleEndian.Uint16(raw[2:4]),
		nsid:   binary.LittleEndian.Uint32(raw[4:8]),
		prp1:   binary.LittleEndian.Uint64(raw[24:32]),
		prp2:   binary.LittleEndian.Uint64(raw[32:40]),
		cdw10:  binary.LittleEndian.Uint32(raw[40:44]),
		cdw11:  binary.LittleEndian.Uint32(raw[44:48]),
		cdw12:  binary.LittleEndian.Uint32(raw[48:52]),
		cdw13:  binary.LittleEndian.Uint32(raw[52:56]),
	}
}

func (c *Controller) executeCommandLocked(q *queue, cmd command) (uint32, uint16, error) {
	if q.id == adminQueueID {
		return c.executeAdminLocked(cmd)
	}
	return c.executeIOLocked(cmd)
}

func (c *Controller) executeAdminLocked(cmd command) (uint32, uint16, error) {
	switch cmd.opcode {
	case adminIdentify:
		data := make([]byte, 4096)
		switch cmd.cdw10 & 0xff {
		case 0:
			c.identifyNamespace(data)
		case 1:
			c.identifyController(data)
		case 2:
			binary.LittleEndian.PutUint32(data[0:4], 1)
		default:
			return 0, 0, nil
		}
		return 0, 0, c.writePRP(cmd.prp1, cmd.prp2, data)
	case adminCreateCQ:
		qid := uint16(cmd.cdw10)
		size := uint16(cmd.cdw10>>16) + 1
		q := c.queues[qid]
		if q == nil {
			q = &queue{id: qid}
			c.queues[qid] = q
		}
		q.size = size
		q.cqAddr = cmd.prp1
		q.cqPhase = true
		q.interrupts = cmd.cdw11&2 != 0
		c.cacheQueueMemoryLocked(q)
	case adminCreateSQ:
		qid := uint16(cmd.cdw10)
		size := uint16(cmd.cdw10>>16) + 1
		cqid := uint16(cmd.cdw11 >> 16)
		q := c.queues[qid]
		if q == nil {
			q = &queue{id: qid}
			c.queues[qid] = q
		}
		q.size = size
		q.sqAddr = cmd.prp1
		q.cqID = cqid
		c.cacheQueueMemoryLocked(q)
	case adminDeleteSQ, adminDeleteCQ:
		delete(c.queues, uint16(cmd.cdw10))
	case adminSetFeatures:
		if cmd.cdw10&0xff == 7 {
			return 0, 0, nil
		}
	case adminGetFeatures:
		if cmd.cdw10&0xff == 7 {
			return 0, 0, nil
		}
	case adminGetLogPage:
		return 0, 0, c.writePRP(cmd.prp1, cmd.prp2, make([]byte, ((cmd.cdw10>>16)&0xfff+1)*4))
	default:
		return 0, 0, nil
	}
	return 0, 0, nil
}

func (c *Controller) queueSubmissionEntry(q *queue, head uint16) []byte {
	offset := int(head) * 64
	if len(q.sqMem) >= offset+64 {
		return q.sqMem[offset : offset+64]
	}
	return nil
}

func (c *Controller) cacheQueueMemoryLocked(q *queue) {
	mem, ok := c.mem.(guestMemorySlicer)
	if !ok || q.size == 0 {
		return
	}
	if q.sqAddr != 0 {
		if sqMem, err := mem.SliceIPA(q.sqAddr, int(q.size)*64); err == nil {
			q.sqMem = sqMem
		}
	}
	if q.cqAddr != 0 {
		if cqMem, err := mem.SliceIPA(q.cqAddr, int(q.size)*16); err == nil {
			q.cqMem = cqMem
		}
	}
}

func (c *Controller) executeIOLocked(cmd command) (uint32, uint16, error) {
	if c.backend == nil {
		return 0, 1, nil
	}
	if cmd.opcode == ioFlush {
		return 0, 0, nil
	}
	lba := uint64(cmd.cdw10) | uint64(cmd.cdw11)<<32
	count := int((cmd.cdw12&0xffff)+1) * blockSize
	offset := int64(lba * blockSize)
	buf := c.scratchLocked(count)
	switch cmd.opcode {
	case ioRead:
		if ok, err := c.readBackendToPRP(offset, count, cmd.prp1, cmd.prp2); ok {
			if err != nil {
				return 0, 1, err
			}
			c.recordIOLocked("read", count)
			return 0, 0, nil
		}
		n, err := c.backend.ReadAt(buf, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, 1, nil
		}
		clear(buf[n:])
		c.recordIOLocked("read", count)
		return 0, 0, c.writePRP(cmd.prp1, cmd.prp2, buf)
	case ioWrite:
		if ok, err := c.writeBackendFromPRP(offset, count, cmd.prp1, cmd.prp2); ok {
			if err != nil {
				return 0, 1, err
			}
			c.recordIOLocked("write", count)
			return 0, 0, nil
		}
		if err := c.readPRP(cmd.prp1, cmd.prp2, buf); err != nil {
			return 0, 1, err
		}
		if _, err := c.backend.WriteAt(buf, offset); err != nil {
			return 0, 1, nil
		}
		c.recordIOLocked("write", count)
		return 0, 0, nil
	default:
		return 0, 1, nil
	}
}

func (c *Controller) recordIOLocked(kind string, size int) {
	if size < 0 {
		size = 0
	}
	switch kind {
	case "read":
		c.readOps++
		c.readBytes += uint64(size)
	case "write":
		c.writeOps++
		c.writeBytes += uint64(size)
	}
	if os.Getenv("CC_NVME_TRACE") == "" {
		return
	}
	now := time.Now()
	if c.lastTrace.IsZero() || now.Sub(c.lastTrace) >= 5*time.Second {
		_, _ = fmt.Fprintf(os.Stderr, "nvme io read_ops=%d read_bytes=%d write_ops=%d write_bytes=%d\n", c.readOps, c.readBytes, c.writeOps, c.writeBytes)
		c.lastTrace = now
	}
}

func (c *Controller) readBackendToPRP(offset int64, size int, prp1, prp2 uint64) (bool, error) {
	mem, ok := c.mem.(guestMemorySlicer)
	if !ok || c.backend == nil || size == 0 {
		return false, nil
	}
	if prp1 == 0 {
		return true, fmt.Errorf("nvme command missing PRP1")
	}
	if !c.prpIsContiguousLocked(prp1, prp2, size) {
		return false, nil
	}
	buf, err := mem.SliceIPA(prp1, size)
	if err != nil {
		return false, nil
	}
	n, err := c.backend.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return true, err
	}
	clear(buf[n:])
	return true, nil
}

func (c *Controller) writeBackendFromPRP(offset int64, size int, prp1, prp2 uint64) (bool, error) {
	mem, ok := c.mem.(guestMemorySlicer)
	if !ok || c.backend == nil || size == 0 {
		return false, nil
	}
	if prp1 == 0 {
		return true, fmt.Errorf("nvme command missing PRP1")
	}
	if !c.prpIsContiguousLocked(prp1, prp2, size) {
		return false, nil
	}
	buf, err := mem.SliceIPA(prp1, size)
	if err != nil {
		return false, nil
	}
	n, err := c.backend.WriteAt(buf, offset)
	if err != nil {
		return true, err
	}
	if n != len(buf) {
		return true, io.ErrShortWrite
	}
	return true, nil
}

func (c *Controller) prpIsContiguousLocked(prp1, prp2 uint64, size int) bool {
	page := c.pageSz
	if page == 0 {
		page = 4096
	}
	first := int(page - (prp1 % page))
	if first >= size {
		return true
	}
	remaining := size - first
	expected := prp1 + uint64(first)
	if remaining <= int(page) {
		return prp2 == expected
	}
	ok := true
	err := c.walkPRPListLocked(prp2, remaining, func(addr uint64, _ int) error {
		if addr != expected {
			ok = false
			return errPRPNotContiguous
		}
		expected += page
		return nil
	})
	return err == nil && ok
}

func (c *Controller) walkPRPListLocked(listAddr uint64, remaining int, fn func(addr uint64, chunk int) error) error {
	if remaining <= 0 {
		return nil
	}
	if listAddr == 0 {
		return fmt.Errorf("nvme PRP list address is zero")
	}
	page := c.pageSz
	if page == 0 {
		page = 4096
	}
	entriesPerList := int(page / 8)
	if entriesPerList < 2 {
		return fmt.Errorf("nvme page size %d too small for PRP list", page)
	}
	entriesRemaining := (remaining + int(page) - 1) / int(page)
	for entriesRemaining > 0 {
		list := c.prpListScratchLocked(int(page))
		if mem, ok := c.mem.(guestMemoryReaderInto); ok {
			if err := mem.ReadIPAInto(listAddr, list); err != nil {
				return err
			}
		} else {
			raw, err := c.mem.ReadIPA(listAddr, len(list))
			if err != nil {
				return err
			}
			copy(list, raw)
		}
		usable := entriesRemaining
		hasNextList := false
		if usable > entriesPerList {
			usable = entriesPerList - 1
			hasNextList = true
		}
		for i := 0; i < usable; i++ {
			addr := binary.LittleEndian.Uint64(list[i*8 : i*8+8])
			chunk := int(page)
			if chunk > remaining {
				chunk = remaining
			}
			if err := fn(addr, chunk); err != nil {
				return err
			}
			remaining -= chunk
			entriesRemaining--
		}
		if !hasNextList {
			return nil
		}
		listAddr = binary.LittleEndian.Uint64(list[(entriesPerList-1)*8 : entriesPerList*8])
		if listAddr == 0 {
			return fmt.Errorf("nvme chained PRP list address is zero")
		}
	}
	return nil
}

func (c *Controller) scratchLocked(size int) []byte {
	if cap(c.scratch) < size {
		c.scratch = make([]byte, size)
	}
	return c.scratch[:size]
}

func (c *Controller) identifyController(data []byte) {
	binary.LittleEndian.PutUint16(data[0:2], 0x1b36)
	binary.LittleEndian.PutUint16(data[2:4], 0x0010)
	copy(data[4:24], []byte("cc-nvme-000000000001"))
	copy(data[24:64], []byte("cc NVMe Block Device"))
	copy(data[64:72], []byte("0.1"))
	binary.LittleEndian.PutUint16(data[78:80], 1)
	binary.LittleEndian.PutUint32(data[80:84], 0x00010400)
	data[512] = 0x66
	data[513] = 0x44
	binary.LittleEndian.PutUint32(data[516:520], 1)
}

func (c *Controller) identifyNamespace(data []byte) {
	blocks := uint64(0)
	if c.backend != nil && c.backend.Size() > 0 {
		blocks = uint64(c.backend.Size()) / blockSize
	}
	binary.LittleEndian.PutUint64(data[0:8], blocks)
	binary.LittleEndian.PutUint64(data[8:16], blocks)
	binary.LittleEndian.PutUint64(data[16:24], blocks)
	data[26] = 0
	data[128+2] = 9
}

func (c *Controller) writeCompletionLocked(q *queue, sqid uint16, sqHead uint16, cid uint16, result uint32, status uint16) error {
	var raw [16]byte
	binary.LittleEndian.PutUint32(raw[0:4], result)
	binary.LittleEndian.PutUint16(raw[8:10], sqHead)
	binary.LittleEndian.PutUint16(raw[10:12], sqid)
	binary.LittleEndian.PutUint16(raw[12:14], cid)
	statusField := status << 1
	if q.cqPhase {
		statusField |= 1
	}
	binary.LittleEndian.PutUint16(raw[14:16], statusField)
	offset := int(q.cqTail) * 16
	if len(q.cqMem) >= offset+16 {
		copy(q.cqMem[offset:offset+16], raw[:])
	} else if err := c.mem.WriteIPA(q.cqAddr+uint64(q.cqTail)*16, raw[:]); err != nil {
		return fmt.Errorf("write cq%d entry tail=%d addr=%#x: %w", q.id, q.cqTail, q.cqAddr, err)
	}
	q.cqTail = (q.cqTail + 1) % q.size
	if q.cqPending < q.size {
		q.cqPending++
	}
	if q.cqTail == 0 {
		q.cqPhase = !q.cqPhase
	}
	return nil
}

func (c *Controller) updateIRQLocked() error {
	high := false
	if c.intMask&1 == 0 {
		for _, q := range c.queues {
			if q.interrupts && q.cqPending != 0 {
				high = true
				break
			}
		}
	}
	if high == c.irqHigh {
		return nil
	}
	c.irqHigh = high
	if c.irq == nil {
		return nil
	}
	if c.msi.enabled {
		if !high {
			return nil
		}
		irq, ok := c.irq.(msiIRQController)
		if !ok {
			return nil
		}
		return irq.SetMSI(c.msi.addr, c.msi.data)
	}
	return c.irq.SetIRQ(c.IRQ, high)
}

func (q *queue) advanceCompletionHeadLocked(head uint16) {
	if q.size == 0 {
		q.cqHead = head
		q.cqPending = 0
		return
	}
	oldHead := q.cqHead
	q.cqHead = head % q.size
	consumed := q.cqHead - oldHead
	if q.cqHead < oldHead {
		consumed = q.size - oldHead + q.cqHead
	}
	if consumed == 0 && q.cqPending == q.size {
		q.cqPending = 0
		return
	}
	if consumed >= q.cqPending {
		q.cqPending = 0
		return
	}
	q.cqPending -= consumed
}

func (c *Controller) readPRP(prp1, prp2 uint64, dst []byte) error {
	return c.transferPRP(prp1, prp2, dst, false)
}

func (c *Controller) writePRP(prp1, prp2 uint64, src []byte) error {
	return c.transferPRP(prp1, prp2, src, true)
}

func (c *Controller) transferPRP(prp1, prp2 uint64, data []byte, write bool) error {
	if len(data) == 0 {
		return nil
	}
	if prp1 == 0 {
		return fmt.Errorf("nvme command missing PRP1")
	}
	page := c.pageSz
	if page == 0 {
		page = 4096
	}
	off := 0
	n := int(page - (prp1 % page))
	if n > len(data) {
		n = len(data)
	}
	if err := c.transferPage(prp1, data[:n], write); err != nil {
		return err
	}
	off += n
	if off >= len(data) {
		return nil
	}
	remaining := len(data) - off
	if remaining <= int(page) {
		return c.transferPage(prp2, data[off:], write)
	}
	var runAddr uint64
	var runOff int
	var runLen int
	flushRun := func() error {
		if runLen == 0 {
			return nil
		}
		if err := c.transferPage(runAddr, data[runOff:runOff+runLen], write); err != nil {
			return err
		}
		runLen = 0
		return nil
	}
	if err := c.walkPRPListLocked(prp2, len(data)-off, func(addr uint64, chunk int) error {
		if runLen == 0 {
			runAddr = addr
			runOff = off
			runLen = chunk
		} else if addr == runAddr+uint64(runLen) {
			runLen += chunk
		} else {
			if err := flushRun(); err != nil {
				return err
			}
			runAddr = addr
			runOff = off
			runLen = chunk
		}
		off += chunk
		return nil
	}); err != nil {
		return err
	}
	return flushRun()
}

func (c *Controller) prpListScratchLocked(size int) []byte {
	if cap(c.prps) < size {
		c.prps = make([]byte, size)
	}
	return c.prps[:size]
}

func (c *Controller) transferPage(addr uint64, data []byte, write bool) error {
	if addr == 0 {
		return fmt.Errorf("nvme PRP address is zero")
	}
	if write {
		return c.mem.WriteIPA(addr, data)
	}
	if mem, ok := c.mem.(guestMemoryReaderInto); ok {
		return mem.ReadIPAInto(addr, data)
	}
	raw, err := c.mem.ReadIPA(addr, len(data))
	if err != nil {
		return err
	}
	copy(data, raw)
	return nil
}

func truncate(value uint64, size int) uint64 {
	if size >= 8 {
		return value
	}
	if size <= 0 {
		return 0
	}
	return value & ((uint64(1) << (uint(size) * 8)) - 1)
}
