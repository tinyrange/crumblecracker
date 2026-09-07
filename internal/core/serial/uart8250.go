package serial

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	UART8250Size         = 0x1000
	UART8250DefaultClock = 1843200

	uartRegisterCount = 8

	uartLCRDLAB = 1 << 7
	uartMCRLoop = 1 << 4

	uartLSRDataReady = 1 << 0
	uartLSRTHRE      = 1 << 5
	uartLSRTEMT      = 1 << 6
)

type UART8250 struct {
	mu      sync.Mutex
	base    uint64
	size    uint64
	stride  uint64
	out     io.Writer
	irq     irqController
	irqLine uint32

	dll         byte
	dlm         byte
	ier         byte
	fcr         byte
	lcr         byte
	mcr         byte
	lsr         byte
	msrStatus   byte
	msrDelta    byte
	scr         byte
	rbr         byte
	pendingIIR  byte
	fifoEnabled bool
	skipLF      bool
}

type irqController interface {
	SetIRQ(line uint32, level bool) error
}

func NewUART8250(base uint64, regShift uint32, out io.Writer) *UART8250 {
	stride := uint64(1) << regShift
	if stride == 0 {
		stride = 1
	}
	u := &UART8250{
		base:       base,
		size:       UART8250Size,
		stride:     stride,
		out:        out,
		lsr:        uartLSRTHRE | uartLSRTEMT,
		pendingIIR: 0x01,
	}
	u.updateModemStatus()
	u.updateInterrupts()
	return u
}

func (u *UART8250) AttachIRQ(irq irqController, irqLine uint32) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.irq = irq
	u.irqLine = irqLine
	u.updateInterrupts()
}

func (u *UART8250) Contains(addr uint64, size int) bool {
	if size <= 0 {
		return false
	}
	end := addr + uint64(size)
	return addr >= u.base && end > addr && end <= u.base+u.size
}

func (u *UART8250) Write(addr uint64, data []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, value := range data {
		if err := u.writeByte(addr+uint64(i), value); err != nil {
			return err
		}
	}
	return nil
}

func (u *UART8250) Read(addr uint64, data []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range data {
		value, err := u.readByte(addr + uint64(i))
		if err != nil {
			return err
		}
		data[i] = value
	}
	return nil
}

func (u *UART8250) WriteValue(addr uint64, size int, value uint64) error {
	if size <= 0 || size > 8 {
		return fmt.Errorf("invalid write size %d", size)
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], value)
	return u.Write(addr, buf[:size])
}

func (u *UART8250) ReadValue(addr uint64, size int) (uint64, error) {
	if size <= 0 || size > 8 {
		return 0, fmt.Errorf("invalid read size %d", size)
	}
	var buf [8]byte
	if err := u.Read(addr, buf[:size]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

func (u *UART8250) InjectRXByte(value byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rbr = value
	u.lsr |= uartLSRDataReady
	u.updateInterrupts()
}

func (u *UART8250) InjectRXBytes(data []byte) error {
	for _, value := range data {
		for {
			u.mu.Lock()
			if u.lsr&uartLSRDataReady == 0 {
				u.rbr = value
				u.lsr |= uartLSRDataReady
				u.updateInterrupts()
				u.mu.Unlock()
				break
			}
			u.mu.Unlock()
			time.Sleep(500 * time.Microsecond)
		}
	}
	return nil
}

func (u *UART8250) writeByte(addr uint64, value byte) error {
	reg, err := u.registerAt(addr)
	if err != nil {
		return err
	}
	u.writeRegister(reg, value)
	return nil
}

func (u *UART8250) readByte(addr uint64) (byte, error) {
	reg, err := u.registerAt(addr)
	if err != nil {
		return 0, err
	}
	return u.readRegister(reg), nil
}

func (u *UART8250) registerAt(addr uint64) (uint16, error) {
	if addr < u.base || addr >= u.base+u.size {
		return 0, fmt.Errorf("uart address %#x outside range [%#x,%#x)", addr, u.base, u.base+u.size)
	}
	offset := addr - u.base
	if offset%u.stride != 0 {
		return 0, fmt.Errorf("uart address %#x is not register aligned", addr)
	}
	reg := offset / u.stride
	if reg >= uartRegisterCount {
		return 0, fmt.Errorf("uart register index %d out of range", reg)
	}
	return uint16(reg), nil
}

func (u *UART8250) writeRegister(reg uint16, value byte) {
	switch reg {
	case 0:
		if u.lcr&uartLCRDLAB != 0 {
			u.dll = value
			return
		}
		u.lsr &^= uartLSRTHRE
		u.updateInterrupts()
		u.transmit(value)
	case 1:
		if u.lcr&uartLCRDLAB != 0 {
			u.dlm = value
			return
		}
		u.setIER(value)
	case 2:
		u.setFCR(value)
	case 3:
		u.lcr = value
	case 4:
		u.setMCR(value)
	case 7:
		u.scr = value
	}
}

func (u *UART8250) readRegister(reg uint16) byte {
	switch reg {
	case 0:
		if u.lcr&uartLCRDLAB != 0 {
			return u.dll
		}
		value := u.rbr
		u.rbr = 0
		u.lsr &^= uartLSRDataReady
		u.updateInterrupts()
		return value
	case 1:
		if u.lcr&uartLCRDLAB != 0 {
			return u.dlm
		}
		return u.ier
	case 2:
		return u.pendingIIR
	case 3:
		return u.lcr
	case 4:
		return u.mcr
	case 5:
		return u.lsr
	case 6:
		return u.modemStatus()
	case 7:
		return u.scr
	default:
		return 0
	}
}

func (u *UART8250) updateInterrupts() {
	interrupt := byte(0x01)
	switch {
	case u.ier&0x04 != 0 && (u.lsr&0x1e) != 0:
		interrupt = 0x06
	case u.ier&0x01 != 0 && u.lsr&uartLSRDataReady != 0:
		interrupt = 0x04
	case u.ier&0x02 != 0 && u.lsr&uartLSRTHRE != 0:
		interrupt = 0x02
	case u.ier&0x08 != 0 && u.msrDelta != 0:
		interrupt = 0x00
	}
	u.pendingIIR = interrupt
	if u.irq != nil && u.irqLine != 0 {
		_ = u.irq.SetIRQ(u.irqLine, interrupt != 0x01)
	}
}

func (u *UART8250) transmit(value byte) {
	if u.mcr&uartMCRLoop != 0 {
		u.rbr = value
		u.lsr |= uartLSRDataReady
		u.updateInterrupts()
		return
	}
	if u.out != nil {
		switch value {
		case '\r':
			_, _ = u.out.Write([]byte{'\n'})
			u.skipLF = true
		case '\n':
			if u.skipLF {
				u.skipLF = false
			} else {
				_, _ = u.out.Write([]byte{'\n'})
			}
		default:
			u.skipLF = false
			_, _ = u.out.Write([]byte{value})
		}
	}
	u.lsr |= uartLSRTHRE | uartLSRTEMT
	u.updateInterrupts()
}

func (u *UART8250) clearRX() {
	u.rbr = 0
	u.lsr &^= uartLSRDataReady
	u.updateInterrupts()
}

func (u *UART8250) setIER(value byte) {
	u.ier = value & 0x0f
	u.updateInterrupts()
}

func (u *UART8250) setFCR(value byte) {
	if value&0x02 != 0 {
		u.clearRX()
	}
	u.fcr = value
	u.fifoEnabled = value&0x01 != 0
}

func (u *UART8250) setMCR(value byte) {
	prev := u.mcr
	u.mcr = value & 0x1f
	if prev&uartMCRLoop != 0 && u.mcr&uartMCRLoop == 0 {
		u.clearRX()
	}
	u.updateModemStatus()
	u.updateInterrupts()
}

func (u *UART8250) modemStatus() byte {
	value := u.msrStatus | u.msrDelta
	u.msrDelta = 0
	return value
}

func (u *UART8250) updateModemStatus() {
	const (
		bitCTS = 1 << 4
		bitDSR = 1 << 5
		bitRI  = 1 << 6
		bitDCD = 1 << 7
	)
	u.msrStatus = bitCTS | bitDSR | bitDCD
	if u.mcr&0x04 != 0 {
		u.msrStatus |= bitRI
	}
}
