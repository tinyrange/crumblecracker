//go:build !linux && !darwin && !windows

package vm

func hostMemoryMB() uint64 { return 0 }
