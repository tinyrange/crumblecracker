//go:build darwin && arm64

package hvf

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	machPagePresent  = int32(0x01)
	machPagePagedOut = int32(0x10)
	machPageReusable = int32(0x800)
)

type snapshotPageQuery func(address uintptr) (int32, error)

type snapshotMemoryRange struct {
	start int
	end   int
}

// writeSparseSnapshotMemory preserves the logical guest-memory size while
// writing only pages which have meaningful contents. Mach page information
// avoids faulting untouched anonymous guest pages into the host process. If
// that information is unavailable, a correctness-first zero-page scan keeps
// the file sparse without relying on page residency.
func writeSparseSnapshotMemory(path string, data []byte, perm os.FileMode) error {
	return writeSparseSnapshotMemoryWithQuery(path, data, perm, querySnapshotMemoryPage)
}

func writeSparseSnapshotMemoryWithQuery(path string, data []byte, perm os.FileMode, query snapshotPageQuery) error {
	ranges, err := populatedSnapshotMemoryRanges(data, query)
	if err != nil {
		ranges = nonzeroSnapshotMemoryRanges(data, os.Getpagesize())
	}
	return writeSnapshotMemoryRanges(path, data, perm, ranges)
}

func writeSnapshotMemoryRanges(path string, data []byte, perm os.FileMode, ranges []snapshotMemoryRange) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	// Establish the logical length before writing populated ranges. On APFS,
	// extending the file with a write beyond EOF can allocate the intervening
	// range, while writes into a pre-truncated sparse file preserve its holes.
	if err := file.Truncate(int64(len(data))); err != nil {
		return err
	}
	for _, span := range ranges {
		if _, err := file.WriteAt(data[span.start:span.end], int64(span.start)); err != nil {
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	return nil
}

func populatedSnapshotMemoryRanges(data []byte, query snapshotPageQuery) ([]snapshotMemoryRange, error) {
	if len(data) == 0 {
		return nil, nil
	}
	pageSize := os.Getpagesize()
	base := uintptr(unsafe.Pointer(&data[0]))
	var ranges []snapshotMemoryRange
	for start := 0; start < len(data); {
		address := base + uintptr(start)
		pageEnd := start + pageSize - int(address%uintptr(pageSize))
		if pageEnd > len(data) {
			pageEnd = len(data)
		}
		disposition, err := query(address)
		if err != nil {
			return nil, err
		}
		populated := disposition&(machPagePresent|machPagePagedOut) != 0 && disposition&machPageReusable == 0
		if populated && !allZeroSnapshotMemory(data[start:pageEnd]) {
			ranges = appendSnapshotMemoryRange(ranges, start, pageEnd)
		}
		start = pageEnd
	}
	return ranges, nil
}

func nonzeroSnapshotMemoryRanges(data []byte, pageSize int) []snapshotMemoryRange {
	if pageSize <= 0 {
		pageSize = 4096
	}
	var ranges []snapshotMemoryRange
	for start := 0; start < len(data); start += pageSize {
		end := min(start+pageSize, len(data))
		if !allZeroSnapshotMemory(data[start:end]) {
			ranges = appendSnapshotMemoryRange(ranges, start, end)
		}
	}
	return ranges
}

func appendSnapshotMemoryRange(ranges []snapshotMemoryRange, start, end int) []snapshotMemoryRange {
	if len(ranges) != 0 && ranges[len(ranges)-1].end == start {
		ranges[len(ranges)-1].end = end
		return ranges
	}
	return append(ranges, snapshotMemoryRange{start: start, end: end})
}

func allZeroSnapshotMemory(data []byte) bool {
	for len(data) >= 8 {
		if *(*uint64)(unsafe.Pointer(&data[0])) != 0 {
			return false
		}
		data = data[8:]
	}
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func querySnapshotMemoryPage(address uintptr) (int32, error) {
	if err := load(); err != nil {
		return 0, err
	}
	if machVMPageQuery == nil || taskSelfTrap == nil {
		return 0, fmt.Errorf("Mach page query unavailable")
	}
	task := taskSelfTrap()
	var disposition int32
	var refCount int32
	if result := machVMPageQuery(task, uint64(address), &disposition, &refCount); result != 0 {
		return 0, fmt.Errorf("Mach page query at %#x: error %d", address, result)
	}
	return disposition, nil
}

func snapshotMemoryAllocatedBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("snapshot stat has type %T", info.Sys())
	}
	return stat.Blocks * 512, nil
}
