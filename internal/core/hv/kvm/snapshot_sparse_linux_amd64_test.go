//go:build linux && amd64

package kvm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSparseSnapshotMemoryPreservesGuestRAMWithoutAllocatingEmptyPages(t *testing.T) {
	const memorySize = 64 << 20
	mem, err := unix.Mmap(-1, 0, memorySize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("map guest memory: %v", err)
	}
	defer unix.Munmap(mem)

	pageSize := unix.Getpagesize()
	for _, offset := range []int{0, memorySize / 2, memorySize - pageSize} {
		for i := 0; i < pageSize; i++ {
			mem[offset+i] = byte(i*31 + offset/pageSize)
		}
	}

	path := filepath.Join(t.TempDir(), "memory.bin")
	if err := writeSparseFile(path, mem, 0o600); err != nil {
		t.Fatalf("write sparse snapshot memory: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot memory: %v", err)
	}
	if info.Size() != memorySize {
		t.Fatalf("snapshot logical size = %d, want %d", info.Size(), memorySize)
	}
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatalf("stat snapshot blocks: %v", err)
	}
	allocated := stat.Blocks * 512
	if allocated >= 1<<20 {
		t.Fatalf("snapshot allocated %d bytes for three populated pages in %d bytes of guest RAM", allocated, memorySize)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open snapshot memory: %v", err)
	}
	defer file.Close()
	restored, err := unix.Mmap(int(file.Fd()), 0, memorySize, unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("map snapshot memory: %v", err)
	}
	defer unix.Munmap(restored)
	if !bytes.Equal(restored, mem) {
		t.Fatal("restored sparse snapshot memory differs from captured guest RAM")
	}
}

func TestSparseSnapshotMemoryFallsBackForUnalignedGuestRAM(t *testing.T) {
	pageSize := unix.Getpagesize()
	storage := make([]byte, pageSize*3+1)
	mem := storage[1:]
	copy(mem[pageSize-8:], []byte("fallback-data"))

	path := filepath.Join(t.TempDir(), "memory.bin")
	if err := writeSparseFile(path, mem, 0o600); err != nil {
		t.Fatalf("write sparse snapshot memory: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot memory: %v", err)
	}
	if !bytes.Equal(restored, mem) {
		t.Fatal("restored fallback snapshot memory differs from captured guest RAM")
	}
}

func BenchmarkWriteSparseSnapshotMemory(b *testing.B) {
	const memorySize = 2 << 30
	mem, err := unix.Mmap(-1, 0, memorySize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		b.Fatalf("map guest memory: %v", err)
	}
	defer unix.Munmap(mem)

	const populated = 64 << 20
	for i := 0; i < populated; i++ {
		mem[i] = byte(i*31 + 1)
	}
	dir := b.TempDir()
	b.SetBytes(memorySize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(dir, "memory.bin")
		if err := writeSparseFile(path, mem, 0o600); err != nil {
			b.Fatalf("write sparse snapshot memory: %v", err)
		}
		b.StopTimer()
		if err := os.Remove(path); err != nil {
			b.Fatalf("remove snapshot memory: %v", err)
		}
		b.StartTimer()
	}
}
