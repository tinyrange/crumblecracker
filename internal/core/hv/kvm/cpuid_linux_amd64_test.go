//go:build linux && amd64

package kvm

import (
	"testing"
	"unsafe"
)

func TestSetCPUIDKVMHypervisorFrequency(t *testing.T) {
	cpuid := newTestCPUID(t)

	setCPUIDKVMHypervisorFrequency(cpuid, 2_918_287)

	base := findCPUIDEntry(t, cpuid, 0x40000000, 0)
	if base.Eax < 0x40000010 {
		t.Fatalf("hypervisor max leaf = %#x, want at least 0x40000010", base.Eax)
	}
	if got := cpuidString12(base); got != "KVMKVMKVM\x00\x00\x00" {
		t.Fatalf("hypervisor vendor = %q", got)
	}
	freq := findCPUIDEntry(t, cpuid, 0x40000010, 0)
	if freq.Eax != 2_918_287 {
		t.Fatalf("TSC frequency leaf eax = %d, want 2918287", freq.Eax)
	}
}

func newTestCPUID(t *testing.T) *kvmCPUID2 {
	t.Helper()
	size := unsafe.Sizeof(kvmCPUID2{}) + unsafe.Sizeof(kvmCPUIDEntry2{})*cpuidMaxEntries
	buf := make([]byte, size)
	cpuid := (*kvmCPUID2)(unsafe.Pointer(&buf[0]))
	cpuid.Nr = 0
	return cpuid
}

func findCPUIDEntry(t *testing.T, cpuid *kvmCPUID2, function, index uint32) kvmCPUIDEntry2 {
	t.Helper()
	for _, entry := range cpuidEntries(cpuid) {
		if entry.Function == function && entry.Index == index {
			return entry
		}
	}
	t.Fatalf("missing CPUID leaf %#x index %#x", function, index)
	return kvmCPUIDEntry2{}
}

func cpuidString12(entry kvmCPUIDEntry2) string {
	var out [12]byte
	putLE32(out[0:], entry.Ebx)
	putLE32(out[4:], entry.Ecx)
	putLE32(out[8:], entry.Edx)
	return string(out[:])
}

func putLE32(out []byte, value uint32) {
	out[0] = byte(value)
	out[1] = byte(value >> 8)
	out[2] = byte(value >> 16)
	out[3] = byte(value >> 24)
}
