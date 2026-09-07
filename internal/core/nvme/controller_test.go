package nvme

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
)

type testMemory struct {
	data []byte
}

func newTestMemory(size int) *testMemory {
	return &testMemory{data: make([]byte, size)}
}

func (m *testMemory) ReadIPA(addr uint64, size int) ([]byte, error) {
	out := make([]byte, size)
	copy(out, m.data[addr:])
	return out, nil
}

func (m *testMemory) WriteIPA(addr uint64, data []byte) error {
	copy(m.data[addr:], data)
	return nil
}

type testIRQ struct {
	line    uint32
	level   bool
	msiAddr uint64
	msiData uint32
	msis    int
}

func (i *testIRQ) SetIRQ(line uint32, level bool) error {
	i.line = line
	i.level = level
	return nil
}

func (i *testIRQ) SetMSI(addr uint64, data uint32) error {
	i.msiAddr = addr
	i.msiData = data
	i.msis++
	return nil
}

type testDisk struct {
	mu   sync.Mutex
	data []byte
}

func newTestDisk(size int) *testDisk {
	return &testDisk{data: make([]byte, size)}
}

func (d *testDisk) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return copy(p, d.data[off:]), nil
}

func (d *testDisk) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return copy(d.data[off:], p), nil
}

func (d *testDisk) Size() int64 {
	return int64(len(d.data))
}

func TestControllerIdentifyAndReadWrite(t *testing.T) {
	mem := newTestMemory(0x20000)
	disk := newTestDisk(1024 * 1024)
	irq := &testIRQ{}
	ctrl := NewController(disk)
	ctrl.IRQ = 10
	ctrl.Attach(mem, irq)

	adminSQ := uint64(0x1000)
	adminCQ := uint64(0x2000)
	ioSQ := uint64(0x3000)
	ioCQ := uint64(0x4000)
	data := uint64(0x5000)

	mustWriteMMIO(t, ctrl, regAQA, 16-1|uint64(16-1)<<16)
	mustWriteMMIO(t, ctrl, regASQ, adminSQ)
	mustWriteMMIO(t, ctrl, regACQ, adminCQ)
	mustWriteMMIO(t, ctrl, regCC, 1)

	writeAdminCommand(mem, adminSQ, 0, command{opcode: adminIdentify, cid: 1, prp1: data, cdw10: 1})
	mustWriteMMIO(t, ctrl, doorbellBase, 1)
	if got := string(bytes.TrimRight(mem.data[data+24:data+64], "\x00")); got != "cc NVMe Block Device" {
		t.Fatalf("identify model = %q", got)
	}

	writeAdminCommand(mem, adminSQ, 1, command{opcode: adminCreateCQ, cid: 2, prp1: ioCQ, cdw10: 1 | uint32(16-1)<<16, cdw11: 2})
	mustWriteMMIO(t, ctrl, doorbellBase, 2)
	writeAdminCommand(mem, adminSQ, 2, command{opcode: adminCreateSQ, cid: 3, prp1: ioSQ, cdw10: 1 | uint32(16-1)<<16, cdw11: 1 << 16})
	mustWriteMMIO(t, ctrl, doorbellBase, 3)

	copy(mem.data[data:data+13], []byte("hello, nvme!\n"))
	writeIOCommand(mem, ioSQ, 0, command{opcode: ioWrite, cid: 4, nsid: 1, prp1: data, cdw12: 0})
	mustWriteMMIO(t, ctrl, doorbellBase+8, 1)
	if got := string(disk.data[:13]); got != "hello, nvme!\n" {
		t.Fatalf("disk write = %q", got)
	}
	if got := binary.LittleEndian.Uint16(mem.data[ioCQ+12 : ioCQ+14]); got != 4 {
		t.Fatalf("write completion cid = %d", got)
	}

	clear(mem.data[data : data+512])
	writeIOCommand(mem, ioSQ, 1, command{opcode: ioRead, cid: 5, nsid: 1, prp1: data, cdw12: 0})
	mustWriteMMIO(t, ctrl, doorbellBase+8, 2)
	if got := string(mem.data[data : data+13]); got != "hello, nvme!\n" {
		t.Fatalf("read data = %q", got)
	}
	if got := binary.LittleEndian.Uint16(mem.data[ioCQ+16+12 : ioCQ+16+14]); got != 5 {
		t.Fatalf("read completion cid = %d", got)
	}
	if !irq.level {
		t.Fatalf("completion interrupt was not asserted")
	}
}

func TestControllerInterruptRemainsAssertedForFullCompletionQueue(t *testing.T) {
	mem := newTestMemory(0x20000)
	disk := newTestDisk(1024 * 1024)
	irq := &testIRQ{}
	ctrl := NewController(disk)
	ctrl.IRQ = 10
	ctrl.Attach(mem, irq)

	adminSQ := uint64(0x1000)
	adminCQ := uint64(0x2000)
	ioSQ := uint64(0x3000)
	ioCQ := uint64(0x4000)
	data := uint64(0x5000)

	mustWriteMMIO(t, ctrl, regAQA, 4-1|uint64(4-1)<<16)
	mustWriteMMIO(t, ctrl, regASQ, adminSQ)
	mustWriteMMIO(t, ctrl, regACQ, adminCQ)
	mustWriteMMIO(t, ctrl, regCC, 1)

	writeAdminCommand(mem, adminSQ, 0, command{opcode: adminCreateCQ, cid: 1, prp1: ioCQ, cdw10: 1 | uint32(2-1)<<16, cdw11: 2})
	mustWriteMMIO(t, ctrl, doorbellBase, 1)
	writeAdminCommand(mem, adminSQ, 1, command{opcode: adminCreateSQ, cid: 2, prp1: ioSQ, cdw10: 1 | uint32(4-1)<<16, cdw11: 1 << 16})
	mustWriteMMIO(t, ctrl, doorbellBase, 2)
	mustWriteMMIO(t, ctrl, doorbellBase+4, 2)

	writeIOCommand(mem, ioSQ, 0, command{opcode: ioRead, cid: 3, nsid: 1, prp1: data, cdw12: 0})
	writeIOCommand(mem, ioSQ, 1, command{opcode: ioRead, cid: 4, nsid: 1, prp1: data + 512, cdw12: 0})
	mustWriteMMIO(t, ctrl, doorbellBase+8, 2)
	if !irq.level {
		t.Fatalf("interrupt level = false after completion queue wrapped full")
	}
	if got := binary.LittleEndian.Uint16(mem.data[ioCQ+12 : ioCQ+14]); got != 3 {
		t.Fatalf("first completion cid = %d", got)
	}
	if got := binary.LittleEndian.Uint16(mem.data[ioCQ+16+12 : ioCQ+16+14]); got != 4 {
		t.Fatalf("second completion cid = %d", got)
	}

	mustWriteMMIO(t, ctrl, doorbellBase+12, 1)
	if !irq.level {
		t.Fatalf("interrupt level = false after only one full-queue completion was acknowledged")
	}
	mustWriteMMIO(t, ctrl, doorbellBase+12, 0)
	if irq.level {
		t.Fatalf("interrupt level = true after all full-queue completions were acknowledged")
	}
}

func TestControllerUsesMSIWhenConfigured(t *testing.T) {
	mem := newTestMemory(0x20000)
	disk := newTestDisk(1024 * 1024)
	irq := &testIRQ{}
	ctrl := NewController(disk)
	ctrl.IRQ = 10
	ctrl.Attach(mem, irq)
	ctrl.ConfigureMSI(true, 0x8080000, 3)

	adminSQ := uint64(0x1000)
	adminCQ := uint64(0x2000)
	ioSQ := uint64(0x3000)
	ioCQ := uint64(0x4000)
	data := uint64(0x5000)

	mustWriteMMIO(t, ctrl, regAQA, 4-1|uint64(4-1)<<16)
	mustWriteMMIO(t, ctrl, regASQ, adminSQ)
	mustWriteMMIO(t, ctrl, regACQ, adminCQ)
	mustWriteMMIO(t, ctrl, regCC, 1)

	writeAdminCommand(mem, adminSQ, 0, command{opcode: adminCreateCQ, cid: 1, prp1: ioCQ, cdw10: 1 | uint32(4-1)<<16, cdw11: 2})
	mustWriteMMIO(t, ctrl, doorbellBase, 1)
	writeAdminCommand(mem, adminSQ, 1, command{opcode: adminCreateSQ, cid: 2, prp1: ioSQ, cdw10: 1 | uint32(4-1)<<16, cdw11: 1 << 16})
	mustWriteMMIO(t, ctrl, doorbellBase, 2)
	mustWriteMMIO(t, ctrl, doorbellBase+4, 2)
	irq.msis = 0

	writeIOCommand(mem, ioSQ, 0, command{opcode: ioRead, cid: 3, nsid: 1, prp1: data, cdw12: 0})
	mustWriteMMIO(t, ctrl, doorbellBase+8, 1)
	if irq.level {
		t.Fatalf("INTx level asserted while MSI enabled")
	}
	if irq.msis != 1 {
		t.Fatalf("MSI count = %d, want 1", irq.msis)
	}
	if irq.msiAddr != 0x8080000 || irq.msiData != 3 {
		t.Fatalf("MSI addr/data = %#x/%#x, want %#x/%#x", irq.msiAddr, irq.msiData, uint64(0x8080000), uint32(3))
	}
}

func TestControllerPreservesAdminQueuesAcrossDisabledCCConfiguration(t *testing.T) {
	mem := newTestMemory(0x20000)
	disk := newTestDisk(1024 * 1024)
	ctrl := NewController(disk)
	ctrl.Attach(mem, &testIRQ{})

	adminSQ := uint64(0x1000)
	adminCQ := uint64(0x2000)
	data := uint64(0x5000)

	mustWriteMMIO(t, ctrl, regAQA, 16-1|uint64(16-1)<<16)
	mustWriteMMIO(t, ctrl, regASQ, adminSQ)
	mustWriteMMIO(t, ctrl, regACQ, adminCQ)
	mustWriteMMIO(t, ctrl, regCC, 0x460000)
	mustWriteMMIO(t, ctrl, regCC, 0x460001)

	writeAdminCommand(mem, adminSQ, 0, command{opcode: adminIdentify, cid: 1, prp1: data, cdw10: 1})
	mustWriteMMIO(t, ctrl, doorbellBase, 1)
	if got := string(bytes.TrimRight(mem.data[data+24:data+64], "\x00")); got != "cc NVMe Block Device" {
		t.Fatalf("identify model = %q", got)
	}
}

func mustWriteMMIO(t *testing.T, ctrl *Controller, offset uint64, value uint64) {
	t.Helper()
	if err := ctrl.WriteMMIO(offset, 4, value); err != nil {
		t.Fatal(err)
	}
}

func writeAdminCommand(mem *testMemory, sq uint64, slot int, cmd command) {
	writeCommand(mem, sq, slot, cmd)
}

func writeIOCommand(mem *testMemory, sq uint64, slot int, cmd command) {
	writeCommand(mem, sq, slot, cmd)
}

func writeCommand(mem *testMemory, sq uint64, slot int, cmd command) {
	raw := mem.data[sq+uint64(slot)*64 : sq+uint64(slot+1)*64]
	clear(raw)
	raw[0] = cmd.opcode
	binary.LittleEndian.PutUint16(raw[2:4], cmd.cid)
	binary.LittleEndian.PutUint32(raw[4:8], cmd.nsid)
	binary.LittleEndian.PutUint64(raw[24:32], cmd.prp1)
	binary.LittleEndian.PutUint64(raw[32:40], cmd.prp2)
	binary.LittleEndian.PutUint32(raw[40:44], cmd.cdw10)
	binary.LittleEndian.PutUint32(raw[44:48], cmd.cdw11)
	binary.LittleEndian.PutUint32(raw[48:52], cmd.cdw12)
	binary.LittleEndian.PutUint32(raw[52:56], cmd.cdw13)
}
