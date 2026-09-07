//go:build linux && amd64

package kvm

import (
	"errors"
	"testing"
)

func TestACPIPMPersistsEnableAndControlRegisters(t *testing.T) {
	pm := NewACPIPM()
	for _, write := range []IOExit{
		{Port: acpiPM1EventPort + 2, Size: 2, Count: 1, Write: true, Data: []byte{0x30, 0x01}},
		{Port: acpiPM1ControlPort, Size: 2, Count: 1, Write: true, Data: []byte{0x01, 0x00}},
	} {
		if handled, err := pm.HandleIO(write); err != nil || !handled {
			t.Fatalf("write port %#x handled=%v err=%v", write.Port, handled, err)
		}
	}
	enable := IOExit{Port: acpiPM1EventPort + 2, Size: 2, Count: 1, Data: []byte{0, 0}}
	if handled, err := pm.HandleIO(enable); err != nil || !handled {
		t.Fatalf("read enable handled=%v err=%v", handled, err)
	}
	if got := enable.Data; got[0] != 0x30 || got[1] != 0x01 {
		t.Fatalf("enable bytes = %#v, want [0x30 0x01]", got)
	}
	control := IOExit{Port: acpiPM1ControlPort, Size: 2, Count: 1, Data: []byte{0, 0}}
	if handled, err := pm.HandleIO(control); err != nil || !handled {
		t.Fatalf("read control handled=%v err=%v", handled, err)
	}
	if got := control.Data; got[0] != 0x01 || got[1] != 0x00 {
		t.Fatalf("control bytes = %#v, want [0x01 0x00]", got)
	}
}

func TestACPIPMPoweroffWriteIsTerminal(t *testing.T) {
	pm := NewACPIPM()
	write := IOExit{Port: acpiPM1ControlPort, Size: 2, Count: 1, Write: true, Data: []byte{0x00, 0x34}}
	handled, err := pm.HandleIO(write)
	if !handled || !errors.Is(err, errGuestPoweroff) {
		t.Fatalf("poweroff handled=%v err=%v", handled, err)
	}
}

func TestACPIPMStatusWritesClearBits(t *testing.T) {
	pm := &ACPIPM{status: 0x0130}
	clear := IOExit{Port: acpiPM1EventPort, Size: 2, Count: 1, Write: true, Data: []byte{0x10, 0x01}}
	if handled, err := pm.HandleIO(clear); err != nil || !handled {
		t.Fatalf("clear status handled=%v err=%v", handled, err)
	}
	read := IOExit{Port: acpiPM1EventPort, Size: 2, Count: 1, Data: []byte{0, 0}}
	if handled, err := pm.HandleIO(read); err != nil || !handled {
		t.Fatalf("read status handled=%v err=%v", handled, err)
	}
	if got := read.Data; got[0] != 0x20 || got[1] != 0x00 {
		t.Fatalf("status bytes = %#v, want [0x20 0x00]", got)
	}
}
