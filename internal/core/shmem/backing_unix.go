//go:build !windows

package shmem

import (
	"fmt"
	"syscall"
)

func allocateBacking(size uint64) ([]byte, func() error, error) {
	if size > uint64(^uint(0)>>1) {
		return nil, nil, fmt.Errorf("shared memory region is too large for this host")
	}
	mem, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, fmt.Errorf("allocate shared memory: %w", err)
	}
	return mem, func() error { return syscall.Munmap(mem) }, nil
}
