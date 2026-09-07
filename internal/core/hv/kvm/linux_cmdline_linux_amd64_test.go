//go:build linux && amd64

package kvm

import "testing"

func TestCPUInfoHasFlag(t *testing.T) {
	cpuinfo := "processor: 0\nflags\t\t: fpu vme hypervisor svm\n"
	if !cpuInfoHasFlag(cpuinfo, "hypervisor") {
		t.Fatal("expected hypervisor flag")
	}
	if cpuInfoHasFlag(cpuinfo, "visor") {
		t.Fatal("matched partial flag")
	}
	if cpuInfoHasFlag(cpuinfo, "") {
		t.Fatal("matched empty flag")
	}
}
