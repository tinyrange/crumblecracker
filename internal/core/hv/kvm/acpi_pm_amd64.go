//go:build linux && amd64

package kvm

import (
	"errors"
	"sync"
)

const (
	acpiPM1EventPort   = 0x400
	acpiPM1ControlPort = 0x404
)

var errGuestPoweroff = errors.New("guest requested ACPI poweroff")

type ACPIPM struct {
	mu      sync.Mutex
	status  uint16
	enable  uint16
	control uint16
}

func NewACPIPM() *ACPIPM {
	return &ACPIPM{}
}

func (p *ACPIPM) HandleIO(ioExit IOExit) (bool, error) {
	if ioExit.Port < acpiPM1EventPort || ioExit.Port >= acpiPM1ControlPort+2 {
		return false, nil
	}
	if ioExit.Size == 0 || ioExit.Count == 0 {
		return true, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := uint32(0); i < ioExit.Count; i++ {
		off := uint64(i) * uint64(ioExit.Size)
		port := ioExit.Port + uint16(off)
		start := int(off)
		end := start + int(ioExit.Size)
		if ioExit.Write {
			p.write(port, ioExit.Data[start:end])
			if p.control&(1<<13) != 0 && (p.control>>10)&7 == 5 {
				return true, errGuestPoweroff
			}
			continue
		}
		p.read(ioExit.Data[start:end], port)
	}
	return true, nil
}

func (p *ACPIPM) read(dst []byte, port uint16) {
	value := uint16(0)
	switch {
	case port >= acpiPM1EventPort && port < acpiPM1EventPort+2:
		value = p.status >> (8 * (port - acpiPM1EventPort))
	case port >= acpiPM1EventPort+2 && port < acpiPM1EventPort+4:
		value = p.enable >> (8 * (port - (acpiPM1EventPort + 2)))
	case port >= acpiPM1ControlPort && port < acpiPM1ControlPort+2:
		value = p.control >> (8 * (port - acpiPM1ControlPort))
	}
	for i := range dst {
		dst[i] = byte(value >> (8 * i))
	}
}

func (p *ACPIPM) write(port uint16, src []byte) {
	var value uint16
	for i, b := range src {
		value |= uint16(b) << (8 * i)
	}
	mask := uint16(0)
	for i := range src {
		mask |= uint16(0xff) << (8 * i)
	}
	switch {
	case port >= acpiPM1EventPort && port < acpiPM1EventPort+2:
		shift := 8 * (port - acpiPM1EventPort)
		shiftedMask := mask << shift
		p.status &^= (value << shift) & shiftedMask
	case port >= acpiPM1EventPort+2 && port < acpiPM1EventPort+4:
		shift := 8 * (port - (acpiPM1EventPort + 2))
		shiftedMask := mask << shift
		p.enable = (p.enable &^ shiftedMask) | ((value << shift) & shiftedMask)
	case port >= acpiPM1ControlPort && port < acpiPM1ControlPort+2:
		shift := 8 * (port - acpiPM1ControlPort)
		shiftedMask := mask << shift
		p.control = (p.control &^ shiftedMask) | ((value << shift) & shiftedMask)
	}
}
