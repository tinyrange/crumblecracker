package virtio

import (
	"encoding/binary"
	"testing"
)

func TestVsockPokeRaisesVringIRQ(t *testing.T) {
	irq := &recordingIRQ{}
	dev := &Vsock{IRQ: 17, irq: irq}
	if err := dev.Poke(); err != nil {
		t.Fatalf("Poke: %v", err)
	}
	if len(irq.levels) != 1 || !irq.levels[0] {
		t.Fatalf("IRQ levels = %v, want [true]", irq.levels)
	}
	if !dev.irqHigh || dev.interruptStatus&vsockInterruptVring == 0 {
		t.Fatalf("vring IRQ was not raised: high=%t status=%#x", dev.irqHigh, dev.interruptStatus)
	}
}

func TestVsockPokePreservesPendingIRQ(t *testing.T) {
	irq := &recordingIRQ{}
	dev := &Vsock{IRQ: 17, irq: irq, interruptStatus: vsockInterruptVring}
	if err := dev.Poke(); err != nil {
		t.Fatalf("Poke: %v", err)
	}
	if len(irq.levels) != 1 || !irq.levels[0] {
		t.Fatalf("IRQ levels = %v, want [true]", irq.levels)
	}
}

func TestVsockPokeRepulsesAssertedPendingIRQ(t *testing.T) {
	irq := &recordingIRQ{}
	dev := &Vsock{IRQ: 17, irq: irq, interruptStatus: vsockInterruptVring, irqHigh: true}
	if err := dev.Poke(); err != nil {
		t.Fatalf("Poke: %v", err)
	}
	if len(irq.levels) != 2 || irq.levels[0] || !irq.levels[1] {
		t.Fatalf("IRQ levels = %v, want [false true]", irq.levels)
	}
	if !dev.irqHigh || dev.interruptStatus == 0 {
		t.Fatalf("pending IRQ was not preserved: high=%t status=%#x", dev.irqHigh, dev.interruptStatus)
	}
}

func TestVsockPokeDeliversPacketQueuedBeforeRXBuffer(t *testing.T) {
	mem := make(testGuestMemory, 0x2000)
	irq := &recordingIRQ{}
	dev := &Vsock{IRQ: 17, irq: irq, mem: mem}
	dev.resetLocked()
	dev.irq = irq
	dev.mem = mem
	queue := &dev.queues[vsockQueueRX]
	queue.ready = true
	queue.size = 1
	queue.descAddr = 0x100
	queue.availAddr = 0x200
	queue.usedAddr = 0x300
	writeDesc(mem, queue.descAddr, 0x1000, 128, descFWrite, 0)
	dev.queueRxPacketLocked([]byte("control-message"))

	// The guest makes a buffer available after the backend's first delivery
	// attempt, without a queue notification reaching the device.
	binary.LittleEndian.PutUint16(mem[queue.availAddr+2:], 1)
	if err := dev.Poke(); err != nil {
		t.Fatalf("Poke: %v", err)
	}
	if len(dev.pendingRx) != 0 {
		t.Fatalf("pending packets = %d, want 0", len(dev.pendingRx))
	}
	if queue.usedIdx != 1 || binary.LittleEndian.Uint16(mem[queue.usedAddr+2:]) != 1 {
		t.Fatalf("used indexes = device %d guest %d, want 1", queue.usedIdx, binary.LittleEndian.Uint16(mem[queue.usedAddr+2:]))
	}
	if got := string(mem[0x1000 : 0x1000+len("control-message")]); got != "control-message" {
		t.Fatalf("delivered payload = %q", got)
	}
}

type recordingIRQ struct {
	levels []bool
}

func (i *recordingIRQ) SetIRQ(_ uint32, level bool) error {
	i.levels = append(i.levels, level)
	return nil
}
