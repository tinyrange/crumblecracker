//go:build linux && amd64

package kvm

import "testing"

func TestI8042SelfTestFailsFast(t *testing.T) {
	kbc := NewI8042()
	handled, err := kbc.HandleIO(IOExit{Port: i8042StatusPort, Size: 1, Count: 1, Write: true, Data: []byte{i8042CmdSelfTest}})
	if err != nil || !handled {
		t.Fatalf("write self-test handled=%v err=%v", handled, err)
	}
	status := []byte{0}
	handled, err = kbc.HandleIO(IOExit{Port: i8042StatusPort, Size: 1, Count: 1, Data: status})
	if err != nil || !handled {
		t.Fatalf("read status handled=%v err=%v", handled, err)
	}
	if status[0]&i8042StatusOutputFull == 0 {
		t.Fatalf("status = %#x, want output buffer full", status[0])
	}
	data := []byte{0}
	handled, err = kbc.HandleIO(IOExit{Port: i8042DataPort, Size: 1, Count: 1, Data: data})
	if err != nil || !handled {
		t.Fatalf("read data handled=%v err=%v", handled, err)
	}
	if data[0] != 0xfc {
		t.Fatalf("self-test response = %#x, want 0xfc", data[0])
	}
}
