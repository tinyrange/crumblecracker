//go:build darwin && arm64

package hvf

import (
	"encoding/binary"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/nvme"
)

func TestHVFPCIHostExposesNVMeECAMAndMMIO(t *testing.T) {
	mem := newNVMeTestMemory(0x20000)
	disk := newNVMeTestDisk(1024 * 1024)
	ctrl := nvme.NewController(disk)
	ctrl.Attach(mem, &nvmeTestIRQ{})
	host := newHVFPCIHost(newHVFNVMePCIDevice(1, arm64vm.NVMeBase, arm64vm.NVMeIRQ, ctrl))

	devConfig := uint64(arm64vm.PCIConfigBase + (1 << 15))
	vendorDevice, err := host.Read(devConfig, 4)
	if err != nil {
		t.Fatal(err)
	}
	if uint16(vendorDevice) != hvfPCIVendorRedHat {
		t.Fatalf("vendor = %#x", uint16(vendorDevice))
	}
	if uint16(vendorDevice>>16) != hvfPCINVMeDeviceID {
		t.Fatalf("device = %#x", uint16(vendorDevice>>16))
	}

	class, err := host.Read(devConfig+0x08, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := byte(class >> 24); got != 0x01 {
		t.Fatalf("class = %#x", got)
	}
	if got := byte(class >> 16); got != 0x08 {
		t.Fatalf("subclass = %#x", got)
	}
	if got := byte(class >> 8); got != 0x02 {
		t.Fatalf("progIF = %#x", got)
	}

	if err := host.Write(devConfig+0x10, 4, 0xffffffff); err != nil {
		t.Fatal(err)
	}
	mask, err := host.Read(devConfig+0x10, 4)
	if err != nil {
		t.Fatal(err)
	}
	if uint32(mask) != 0xffffc000 {
		t.Fatalf("BAR0 mask = %#x", uint32(mask))
	}

	mustNVMeMMIO(t, host, regAQAForTest(), 16-1|uint64(16-1)<<16)
	mustNVMeMMIO(t, host, regASQForTest(), 0x1000)
	mustNVMeMMIO(t, host, regACQForTest(), 0x2000)
	mustNVMeMMIO(t, host, regCCForTest(), 1)
	writeNVMeTestCommand(mem, 0x1000, 0, nvmeTestCommand{opcode: 0x06, cid: 1, prp1: 0x5000, cdw10: 1})
	mustNVMeMMIO(t, host, 0x1000, 1)
	if got := string(trimZero(mem.data[0x5000+24 : 0x5000+64])); got != "cc NVMe Block Device" {
		t.Fatalf("identify model = %q", got)
	}
}

func mustNVMeMMIO(t *testing.T, host *hvfPCIHost, offset uint64, value uint64) {
	t.Helper()
	if err := host.Write(arm64vm.NVMeBase+offset, 4, value); err != nil {
		t.Fatal(err)
	}
}

func regAQAForTest() uint64 { return 0x24 }
func regASQForTest() uint64 { return 0x28 }
func regACQForTest() uint64 { return 0x30 }
func regCCForTest() uint64  { return 0x14 }

type nvmeTestMemory struct {
	data []byte
}

func newNVMeTestMemory(size int) *nvmeTestMemory {
	return &nvmeTestMemory{data: make([]byte, size)}
}

func (m *nvmeTestMemory) ReadIPA(addr uint64, size int) ([]byte, error) {
	out := make([]byte, size)
	copy(out, m.data[addr:])
	return out, nil
}

func (m *nvmeTestMemory) WriteIPA(addr uint64, data []byte) error {
	copy(m.data[addr:], data)
	return nil
}

type nvmeTestIRQ struct{}

func (*nvmeTestIRQ) SetIRQ(uint32, bool) error { return nil }

type nvmeTestDisk struct {
	data []byte
}

func newNVMeTestDisk(size int) *nvmeTestDisk {
	return &nvmeTestDisk{data: make([]byte, size)}
}

func (d *nvmeTestDisk) ReadAt(p []byte, off int64) (int, error) {
	return copy(p, d.data[off:]), nil
}

func (d *nvmeTestDisk) WriteAt(p []byte, off int64) (int, error) {
	return copy(d.data[off:], p), nil
}

func (d *nvmeTestDisk) Size() int64 {
	return int64(len(d.data))
}

type nvmeTestCommand struct {
	opcode uint8
	cid    uint16
	prp1   uint64
	cdw10  uint32
}

func writeNVMeTestCommand(mem *nvmeTestMemory, sq uint64, slot int, cmd nvmeTestCommand) {
	raw := mem.data[sq+uint64(slot)*64 : sq+uint64(slot+1)*64]
	clear(raw)
	raw[0] = cmd.opcode
	binary.LittleEndian.PutUint16(raw[2:4], cmd.cid)
	binary.LittleEndian.PutUint64(raw[24:32], cmd.prp1)
	binary.LittleEndian.PutUint32(raw[40:44], cmd.cdw10)
}

func trimZero(data []byte) []byte {
	for len(data) > 0 && data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}
	return data
}
