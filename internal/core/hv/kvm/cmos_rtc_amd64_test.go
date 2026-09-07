//go:build linux && amd64

package kvm

import (
	"testing"
	"time"
)

func TestCMOSRTCReadsDateTimeRegisters(t *testing.T) {
	rtc := NewCMOSRTC(func() time.Time {
		return time.Date(2026, time.June, 17, 22, 34, 56, 0, time.UTC)
	})
	tests := []struct {
		reg  byte
		want byte
	}{
		{cmosRegSeconds, 0x56},
		{cmosRegMinutes, 0x34},
		{cmosRegHours, 0x22},
		{cmosRegMonthDay, 0x17},
		{cmosRegMonth, 0x06},
		{cmosRegYear, 0x26},
		{cmosRegCentury, 0x20},
		{cmosRegStatusB, 0x02},
		{cmosRegStatusD, 0x80},
	}
	for _, tt := range tests {
		selectReg := IOExit{Port: cmosIndexPort, Size: 1, Count: 1, Write: true, Data: []byte{tt.reg}}
		if handled, err := rtc.HandleIO(selectReg); err != nil || !handled {
			t.Fatalf("select reg %#x handled=%v err=%v", tt.reg, handled, err)
		}
		read := IOExit{Port: cmosDataPort, Size: 1, Count: 1, Data: []byte{0}}
		if handled, err := rtc.HandleIO(read); err != nil || !handled {
			t.Fatalf("read reg %#x handled=%v err=%v", tt.reg, handled, err)
		}
		if got := read.Data[0]; got != tt.want {
			t.Fatalf("reg %#x = %#x, want %#x", tt.reg, got, tt.want)
		}
	}
}

func TestCMOSRTCIgnoresNMIBitInRegisterSelect(t *testing.T) {
	rtc := NewCMOSRTC(func() time.Time {
		return time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	})
	selectReg := IOExit{Port: cmosIndexPort, Size: 1, Count: 1, Write: true, Data: []byte{0x80 | cmosRegCentury}}
	if handled, err := rtc.HandleIO(selectReg); err != nil || !handled {
		t.Fatalf("select century handled=%v err=%v", handled, err)
	}
	read := IOExit{Port: cmosDataPort, Size: 1, Count: 1, Data: []byte{0}}
	if handled, err := rtc.HandleIO(read); err != nil || !handled {
		t.Fatalf("read century handled=%v err=%v", handled, err)
	}
	if got := read.Data[0]; got != 0x20 {
		t.Fatalf("century = %#x, want 0x20", got)
	}
}
