//go:build linux && amd64

package kvm

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestReclaimGuestPageRangesPreservesPagesBetweenRanges(t *testing.T) {
	const pageSize = 4096
	mem, err := unix.Mmap(-1, 0, 3*pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer unix.Munmap(mem)
	for i := range mem {
		mem[i] = byte(i/pageSize + 1)
	}
	vm := &VM{mem: mem, lowMemLimit: uint64(len(mem))}
	if err := vm.ReclaimGuestPageRanges([][2]uint64{{0, pageSize}, {2 * pageSize, pageSize}}); err != nil {
		t.Fatalf("reclaim ranges: %v", err)
	}
	if mem[0] != 0 || mem[2*pageSize] != 0 {
		t.Fatalf("reclaimed pages were not discarded: first=%d third=%d", mem[0], mem[2*pageSize])
	}
	if mem[pageSize] != 2 {
		t.Fatalf("page between sparse ranges changed to %d", mem[pageSize])
	}
}
