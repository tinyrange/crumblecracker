//go:build linux && amd64

package kvm

import (
	"os"
	"strings"
)

func linuxKVMHostKernelArgs() []string {
	if !hostCPUHasFlag("hypervisor") {
		return nil
	}
	return []string{"noapic"}
}

func hostCPUHasFlag(flag string) bool {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return false
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	return cpuInfoHasFlag(string(data), flag)
}

func cpuInfoHasFlag(cpuinfo, flag string) bool {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return false
	}
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "flags" {
			continue
		}
		for _, got := range strings.Fields(value) {
			if got == flag {
				return true
			}
		}
	}
	return false
}
