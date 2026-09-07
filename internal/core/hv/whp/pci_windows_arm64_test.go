//go:build windows && arm64

package whp

import (
	"encoding/binary"
	"testing"
)

func TestArm64PCIDeviceMSICapability(t *testing.T) {
	dev := &arm64PCIDevice{
		VendorID: 0x1b36,
		DeviceID: 0x0010,
		MSI:      true,
	}
	var cfg [4096]byte
	dev.buildConfig(cfg[:])

	if got := cfg[0x34]; got != arm64PCICapMSIOffset {
		t.Fatalf("capability pointer = %#x, want %#x", got, arm64PCICapMSIOffset)
	}
	if got := cfg[arm64PCICapMSIOffset]; got != 0x05 {
		t.Fatalf("capability ID = %#x, want %#x", got, byte(0x05))
	}
	if got := binary.LittleEndian.Uint16(cfg[arm64PCICapMSIOffset+2 : arm64PCICapMSIOffset+4]); got != 1<<7 {
		t.Fatalf("MSI control = %#x, want %#x", got, uint16(1<<7))
	}

	dev.writeConfigByte(arm64PCICapMSIOffset+4, 0x00)
	dev.writeConfigByte(arm64PCICapMSIOffset+5, 0x00)
	dev.writeConfigByte(arm64PCICapMSIOffset+6, 0x08)
	dev.writeConfigByte(arm64PCICapMSIOffset+7, 0x08)
	dev.writeConfigByte(arm64PCICapMSIOffset+12, 0x03)
	dev.writeConfigByte(arm64PCICapMSIOffset+2, 0x01)

	if !dev.MSIEnabled {
		t.Fatalf("MSIEnabled = false")
	}
	if dev.MSIAddress != 0x08080000 || dev.MSIData != 3 {
		t.Fatalf("MSI address/data = %#x/%#x, want %#x/%#x", dev.MSIAddress, dev.MSIData, uint64(0x08080000), uint32(3))
	}
}
