//go:build windows && amd64

package whp

import (
	"errors"
	"sync"
	"time"
)

var errGuestPoweroff = errors.New("guest requested ACPI poweroff")

const (
	i8042DataPort   = 0x60
	i8042StatusPort = 0x64

	i8042StatusOutputFull = 0x01

	i8042CmdSelfTest     = 0xaa
	i8042CmdTestKeyboard = 0xab

	cmosIndexPort = 0x70
	cmosDataPort  = 0x71

	cmosRegSeconds  = 0x00
	cmosRegMinutes  = 0x02
	cmosRegHours    = 0x04
	cmosRegWeekday  = 0x06
	cmosRegMonthDay = 0x07
	cmosRegMonth    = 0x08
	cmosRegYear     = 0x09
	cmosRegStatusA  = 0x0a
	cmosRegStatusB  = 0x0b
	cmosRegStatusD  = 0x0d
	cmosRegCentury  = 0x32

	acpiPM1EventPort   = 0x400
	acpiPM1ControlPort = 0x404
)

type I8042 struct {
	mu    sync.Mutex
	queue []byte
}

func NewI8042() *I8042 {
	return &I8042{}
}

func (c *I8042) ReadIO(port uint16, data []byte) (bool, error) {
	if c == nil {
		return false, nil
	}
	if port != i8042DataPort && port != i8042StatusPort {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var value byte
	switch port {
	case i8042DataPort:
		if len(c.queue) != 0 {
			value = c.queue[0]
			copy(c.queue, c.queue[1:])
			c.queue = c.queue[:len(c.queue)-1]
		}
	case i8042StatusPort:
		if len(c.queue) != 0 {
			value = i8042StatusOutputFull
		}
	}
	for i := range data {
		data[i] = value
	}
	return true, nil
}

func (c *I8042) WriteIO(port uint16, data []byte) (bool, error) {
	if c == nil {
		return false, nil
	}
	if port != i8042DataPort && port != i8042StatusPort {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if port == i8042StatusPort && len(data) != 0 {
		switch data[0] {
		case i8042CmdSelfTest:
			c.queue = append(c.queue, 0xfc)
		case i8042CmdTestKeyboard:
			c.queue = append(c.queue, 0x00)
		}
	}
	return true, nil
}

type CMOSRTC struct {
	mu    sync.Mutex
	index byte
	now   func() time.Time
}

func NewCMOSRTC(now func() time.Time) *CMOSRTC {
	if now == nil {
		now = time.Now
	}
	return &CMOSRTC{now: now}
}

func (r *CMOSRTC) ReadIO(port uint16, data []byte) (bool, error) {
	if r == nil {
		return false, nil
	}
	if port != cmosIndexPort && port != cmosDataPort {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	value := r.index
	if port == cmosDataPort {
		value = r.readRegisterLocked(r.index)
	}
	for i := range data {
		data[i] = value
	}
	return true, nil
}

func (r *CMOSRTC) WriteIO(port uint16, data []byte) (bool, error) {
	if r == nil {
		return false, nil
	}
	if port != cmosIndexPort && port != cmosDataPort {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if port == cmosIndexPort && len(data) != 0 {
		r.index = data[0] & 0x7f
	}
	return true, nil
}

func (r *CMOSRTC) readRegisterLocked(reg byte) byte {
	now := r.now().UTC()
	switch reg {
	case cmosRegSeconds:
		return bcdByte(now.Second())
	case cmosRegMinutes:
		return bcdByte(now.Minute())
	case cmosRegHours:
		return bcdByte(now.Hour())
	case cmosRegWeekday:
		return bcdByte(int(now.Weekday()) + 1)
	case cmosRegMonthDay:
		return bcdByte(now.Day())
	case cmosRegMonth:
		return bcdByte(int(now.Month()))
	case cmosRegYear:
		return bcdByte(now.Year() % 100)
	case cmosRegStatusA:
		return 0x26
	case cmosRegStatusB:
		return 0x02
	case cmosRegStatusD:
		return 0x80
	case cmosRegCentury:
		return bcdByte(now.Year() / 100)
	default:
		return 0
	}
}

func bcdByte(value int) byte {
	if value < 0 {
		value = 0
	}
	return byte((value/10)<<4 | (value % 10))
}

type ACPIPM struct {
	mu      sync.Mutex
	status  uint16
	enable  uint16
	control uint16
}

func NewACPIPM() *ACPIPM {
	return &ACPIPM{}
}

func (p *ACPIPM) ReadIO(port uint16, data []byte) (bool, error) {
	if p == nil {
		return false, nil
	}
	if port < acpiPM1EventPort || port >= acpiPM1ControlPort+2 {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	value := uint16(0)
	switch {
	case port >= acpiPM1EventPort && port < acpiPM1EventPort+2:
		value = p.status >> (8 * (port - acpiPM1EventPort))
	case port >= acpiPM1EventPort+2 && port < acpiPM1EventPort+4:
		value = p.enable >> (8 * (port - (acpiPM1EventPort + 2)))
	case port >= acpiPM1ControlPort && port < acpiPM1ControlPort+2:
		value = p.control >> (8 * (port - acpiPM1ControlPort))
	}
	for i := range data {
		data[i] = byte(value >> (8 * i))
	}
	return true, nil
}

func (p *ACPIPM) WriteIO(port uint16, data []byte) (bool, error) {
	if p == nil {
		return false, nil
	}
	if port < acpiPM1EventPort || port >= acpiPM1ControlPort+2 {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var value uint16
	for i, b := range data {
		value |= uint16(b) << (8 * i)
	}
	mask := uint16(0)
	for i := range data {
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
		if p.control&(1<<13) != 0 && (p.control>>10)&7 == 5 {
			return true, errGuestPoweroff
		}
	}
	return true, nil
}
