package kvm

import (
	"io"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
)

type uartIRQRecorder struct {
	line  uint32
	level bool
}

func (r *uartIRQRecorder) SetIRQ(line uint32, level bool) error {
	r.line = line
	r.level = level
	return nil
}

func TestAMD64UARTSignalsTransmitInterrupt(t *testing.T) {
	irq := &uartIRQRecorder{}
	uart := newAMD64UART(irq, io.Discard)

	if err := uart.WriteValue(amd64vm.COM1Base+1, 1, 0x02); err != nil {
		t.Fatalf("enable transmit interrupt: %v", err)
	}
	if irq.line != amd64vm.COM1IRQ || !irq.level {
		t.Fatalf("UART interrupt = line %d level %t, want line %d asserted", irq.line, irq.level, amd64vm.COM1IRQ)
	}
}
