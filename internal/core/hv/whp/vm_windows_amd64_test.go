//go:build windows && amd64

package whp

import (
	"encoding/binary"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
)

func TestAMD64GuestMemoryRegionsLeaveDeviceHole(t *testing.T) {
	const memorySize = 12 << 30

	regions := amd64GuestMemoryRegions(memorySize)
	want := []guestMemoryRegion{
		{
			guestPhysAddr: amd64vm.MemoryBase,
			size:          3 << 30,
		},
		{
			guestPhysAddr: amd64vm.HighMemoryBase,
			memoryOffset:  3 << 30,
			size:          9 << 30,
		},
	}
	if len(regions) != len(want) {
		t.Fatalf("len(amd64GuestMemoryRegions()) = %d, want %d", len(regions), len(want))
	}
	for i := range want {
		if regions[i] != want[i] {
			t.Errorf("region %d = %#v, want %#v", i, regions[i], want[i])
		}
	}
}

func TestBootMADTEnumeratesEveryVirtualCPU(t *testing.T) {
	const cpus = 4
	madt := buildBootMADT(cpus)
	const madtHeaderSize = 8
	const localAPICEntrySize = 8
	if len(madt) < madtHeaderSize+cpus*localAPICEntrySize {
		t.Fatalf("MADT length = %d, want at least %d", len(madt), madtHeaderSize+cpus*localAPICEntrySize)
	}
	for cpu := 0; cpu < cpus; cpu++ {
		entry := madt[madtHeaderSize+cpu*localAPICEntrySize:]
		if entry[0] != 0 || entry[1] != localAPICEntrySize {
			t.Fatalf("CPU %d MADT entry header = {%d, %d}, want {0, %d}", cpu, entry[0], entry[1], localAPICEntrySize)
		}
		if entry[2] != byte(cpu) || entry[3] != byte(cpu) {
			t.Fatalf("CPU %d MADT identity = UID %d APIC %d", cpu, entry[2], entry[3])
		}
		if flags := binary.LittleEndian.Uint32(entry[4:8]); flags != 1 {
			t.Fatalf("CPU %d MADT flags = %#x, want enabled", cpu, flags)
		}
	}
}
