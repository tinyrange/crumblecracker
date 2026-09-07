//go:build linux

package kvm

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const snapshotMergeableEnv = "CCX3_KVM_SNAPSHOT_MERGEABLE"

// Thirty-two MiB caps a 256 MiB guest at eight chunks. This keeps process-wide
// VMA tree updates cheap at several thousand clones while still excluding
// ballooned regions and avoiding a whole-snapshot KSM scan.
const snapshotMergeChunkSize = 32 << 20

func adviseSnapshotMemoryMergeable(mem []byte) error {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(snapshotMergeableEnv))) {
	case "1", "true", "yes", "on":
		return adviseSnapshotPrivateChunksMergeable(mem)
	}
	return nil
}

// adviseSnapshotPrivateChunksMergeable marks only populated chunks containing
// private COW pages. Snapshot memory is predominantly already-shared file
// pages; marking the entire mapping makes KSM walk hundreds of unused MiB per
// VM and prevents it from keeping up with large clone fleets.
func adviseSnapshotPrivateChunksMergeable(mem []byte) error {
	if len(mem) == 0 {
		return nil
	}
	pageSize := unix.Getpagesize()
	base := uintptr(unsafe.Pointer(&mem[0]))
	pageCount := (len(mem) + pageSize - 1) / pageSize
	file, err := os.Open("/proc/self/pagemap")
	if err != nil {
		return fmt.Errorf("open self pagemap: %w", err)
	}
	defer file.Close()

	const entriesPerRead = 4096
	entries := make([]byte, entriesPerRead*8)
	chunks := make([]bool, (len(mem)+snapshotMergeChunkSize-1)/snapshotMergeChunkSize)
	for firstPage := 0; firstPage < pageCount; firstPage += entriesPerRead {
		count := min(entriesPerRead, pageCount-firstPage)
		data := entries[:count*8]
		offset := int64((base/uintptr(pageSize))+uintptr(firstPage)) * 8
		if _, err := file.ReadAt(data, offset); err != nil && err != io.EOF {
			return fmt.Errorf("read self pagemap: %w", err)
		}
		for i := 0; i < count; i++ {
			entry := binary.LittleEndian.Uint64(data[i*8:])
			const present = uint64(1) << 63
			const fileOrSharedAnon = uint64(1) << 61
			if entry&present != 0 && entry&fileOrSharedAnon == 0 {
				byteOffset := (firstPage + i) * pageSize
				chunks[byteOffset/snapshotMergeChunkSize] = true
			}
		}
	}

	for start := 0; start < len(chunks); {
		for start < len(chunks) && !chunks[start] {
			start++
		}
		if start == len(chunks) {
			break
		}
		end := start + 1
		for end < len(chunks) && chunks[end] {
			end++
		}
		lo := start * snapshotMergeChunkSize
		hi := min(end*snapshotMergeChunkSize, len(mem))
		if err := unix.Madvise(mem[lo:hi], unix.MADV_MERGEABLE); err != nil {
			return fmt.Errorf("mark snapshot private memory mergeable: %w", err)
		}
		start = end
	}
	return nil
}
