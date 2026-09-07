//go:build linux && amd64

package kvm

import (
	"sync"
	"time"
)

const (
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
)

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

func (r *CMOSRTC) HandleIO(ioExit IOExit) (bool, error) {
	if ioExit.Port != cmosIndexPort && ioExit.Port != cmosDataPort {
		return false, nil
	}
	if ioExit.Size == 0 || ioExit.Count == 0 {
		return true, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := uint32(0); i < ioExit.Count; i++ {
		off := uint64(i) * uint64(ioExit.Size)
		if ioExit.Write {
			if ioExit.Port == cmosIndexPort {
				r.index = ioExit.Data[off] & 0x7f
			}
			continue
		}
		value := byte(0xff)
		switch ioExit.Port {
		case cmosIndexPort:
			value = r.index
		case cmosDataPort:
			value = r.readRegisterLocked(r.index)
		}
		for j := uint8(0); j < ioExit.Size; j++ {
			ioExit.Data[off+uint64(j)] = value
		}
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
