//go:build linux && amd64

package kvm

import (
	"encoding/binary"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/nvme"
)

func TestNVMePCIDeviceConfigUsesMemoryBAR(t *testing.T) {
	ctrl := nvme.NewController(nil)
	dev := NewNVMePCIDevice(1, 0xfeb00000, 10, ctrl)
	bus := NewPCIBus(dev)

	var cfg [4]byte
	bus.readConfigAt(0, 1, 0, 0x08, cfg[:])
	if class := cfg[3]; class != 0x01 {
		t.Fatalf("class = %#x", class)
	}
	if subclass := cfg[2]; subclass != 0x08 {
		t.Fatalf("subclass = %#x", subclass)
	}
	if progIF := cfg[1]; progIF != 0x02 {
		t.Fatalf("progIF = %#x", progIF)
	}

	bus.readConfigAt(0, 1, 0, 0x10, cfg[:])
	if bar := binary.LittleEndian.Uint32(cfg[:]); bar != 0xfeb00000 {
		t.Fatalf("BAR0 = %#x", bar)
	}

	bus.writeConfigAt(0, 1, 0, 0x10, []byte{0xff, 0xff, 0xff, 0xff})
	bus.readConfigAt(0, 1, 0, 0x10, cfg[:])
	if mask := binary.LittleEndian.Uint32(cfg[:]); mask != 0xffffc000 {
		t.Fatalf("BAR0 probe mask = %#x", mask)
	}

	newBAR := []byte{0x00, 0x00, 0xa0, 0xfe}
	bus.writeConfigAt(0, 1, 0, 0x10, newBAR)
	if dev.MMIOBAR != 0xfea00000 {
		t.Fatalf("device MMIOBAR = %#x", dev.MMIOBAR)
	}
}

func writePCIAddress(t *testing.T, bus *PCIBus, busNo, devNo, fnNo uint8, reg uint8) {
	t.Helper()
	value := uint32(1<<31) | uint32(busNo)<<16 | uint32(devNo)<<11 | uint32(fnNo)<<8 | uint32(reg&0xfc)
	var data [4]byte
	binary.LittleEndian.PutUint32(data[:], value)
	writePort(t, bus, pciConfigAddressPort, data[:])
}

func readPort(t *testing.T, bus *PCIBus, port uint16, size uint8) []byte {
	t.Helper()
	data := make([]byte, size)
	handled, err := bus.HandleIO(IOExit{Port: port, Data: data, Size: size, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatalf("port %#x was not handled", port)
	}
	return data
}

func writePort(t *testing.T, bus *PCIBus, port uint16, data []byte) {
	t.Helper()
	handled, err := bus.HandleIO(IOExit{Port: port, Data: data, Size: uint8(len(data)), Count: 1, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatalf("port %#x was not handled", port)
	}
}
