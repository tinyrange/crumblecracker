//go:build windows

package shmem

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func allocateBacking(size uint64) ([]byte, func() error, error) {
	if size == 0 || size > uint64(^uint(0)>>1) {
		return nil, nil, fmt.Errorf("shared memory region size is not representable on this host")
	}
	addr, err := windows.VirtualAlloc(0, uintptr(size), windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		return nil, nil, fmt.Errorf("allocate shared memory: %w", err)
	}
	mem := unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(size))
	return mem, func() error {
		return windows.VirtualFree(addr, 0, windows.MEM_RELEASE)
	}, nil
}
