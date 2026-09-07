package vm

import (
	"fmt"
	"io"
	"sort"
)

// regionFragment is effectively an extent.
// Reads from beyond size and before offset are illegal for the underlying region.
type regionFragment struct {
	region MemoryRegion
	offset int64
	size   int64
}

func (f regionFragment) String() string {
	return fmt.Sprintf("%08X-%08X", f.start(), f.end())
}

func (f regionFragment) start() int64 { return f.offset }
func (f regionFragment) end() int64   { return f.offset + f.size }

// Return a new region starting at off.
func (f regionFragment) offsetAt(off int64) *regionFragment {
	if off < 0 || off > f.size {
		return nil
	}

	return &regionFragment{
		region: NewOffsetRegion(f.region, off),
		offset: f.offset + off,
		size:   f.size - off,
	}
}

// Return a new region ending at off.
func (f regionFragment) cutAt(off int64) *regionFragment {
	if off < 0 || off > f.size {
		return nil
	}

	return &regionFragment{
		region: f.region,
		offset: f.offset,
		size:   off,
	}
}

// This is a implementation detail so the struct is private.
type fragmentedRegion struct {
	// This array should always be sorted according to offset.
	fragments []*regionFragment
	totalSize uint64
}

func (f *fragmentedRegion) String() string {
	s := "[ "

	for _, region := range f.fragments {
		s += fmt.Sprintf("%+v %08x-%08x ", region.region, region.offset, region.offset+int64(region.size))
	}

	s += "]"

	return s
}

// findFragmentsForRange uses binary search to find fragments that overlap with the given range [off, off+len)
// Returns the starting index and count of overlapping fragments
func (f *fragmentedRegion) findFragmentsForRange(off int64, length int64) (startIdx, count int) {
	if len(f.fragments) == 0 {
		return 0, 0
	}

	rangeEnd := off + length

	// Find the first fragment that might overlap (binary search for first fragment ending after 'off')
	startIdx = sort.Search(len(f.fragments), func(i int) bool {
		return f.fragments[i].end() > off
	})

	if startIdx >= len(f.fragments) {
		return startIdx, 0 // No fragments overlap
	}

	// Count consecutive fragments that overlap with our range
	count = 0
	for i := startIdx; i < len(f.fragments); i++ {
		frag := f.fragments[i]
		if frag.start() >= rangeEnd {
			break // Fragment starts after our range ends
		}
		count++
	}

	return startIdx, count
}

// ReadAt implements MemoryRegion.
func (f *fragmentedRegion) ReadAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(f, off); err != nil {
		return 0, err
	}
	if int64(len(p)) > f.Size()-off {
		p = p[:int(f.Size()-off)]
	}
	if len(p) == 0 {
		return 0, nil
	}

	clear(p)

	// Use binary search to find overlapping fragments
	startIdx, count := f.findFragmentsForRange(off, int64(len(p)))
	requestStart := off

	for i := 0; i < count; i++ {
		frag := f.fragments[startIdx+i]

		readStart := frag.start()
		if readStart < requestStart {
			readStart = requestStart
		}
		readEnd := frag.end()
		requestEnd := requestStart + int64(len(p))
		if readEnd > requestEnd {
			readEnd = requestEnd
		}
		if readStart >= readEnd {
			continue
		}

		dstStart := readStart - requestStart
		fragOffset := readStart - frag.start()
		readSize := readEnd - readStart
		childN, childErr := frag.region.ReadAt(p[dstStart:dstStart+readSize], fragOffset)
		if childErr != nil && childErr != io.EOF {
			return n, childErr
		}
		if int64(childN) < readSize && childErr == io.EOF {
			continue
		}
	}

	return len(p), nil
}

// Size implements MemoryRegion.
func (f *fragmentedRegion) Size() int64 {
	return int64(f.totalSize)
}

// WriteAt implements MemoryRegion.
func (f *fragmentedRegion) WriteAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(f, off); err != nil {
		return 0, err
	}

	// Use binary search to find overlapping fragments
	startIdx, count := f.findFragmentsForRange(off, int64(len(p)))

	for i := 0; i < count; i++ {
		frag := f.fragments[startIdx+i]

		// If the fragment doesn't overlap with the write area then skip it.
		if (frag.offset + int64(frag.size)) <= off {
			continue
		}

		fragOffset := int64(off) - frag.offset

		var childN int

		if len(p) > int(frag.size) {
			childN, err = frag.region.WriteAt(p[:frag.size], int64(fragOffset))
		} else {
			childN, err = frag.region.WriteAt(p, int64(fragOffset))
		}

		n += childN
		if err != nil {
			return
		}

		p = p[childN:]
		off = int64(frag.offset) + int64(frag.size)

		if len(p) == 0 {
			break
		}
	}

	return
}

func (f *fragmentedRegion) mapFragment(frag MemoryRegion, off int64) error {
	if frag == nil {
		return fmt.Errorf("fragment is nil")
	}
	limit := int64(f.totalSize)
	if off < 0 || off > limit {
		return fmt.Errorf("fragment offset %d is outside [0,%d]", off, limit)
	}
	size := frag.Size()
	if size < 0 || size > limit-off {
		return fmt.Errorf("fragment size %d at offset %d exceeds region size %d", size, off, limit)
	}
	if size == 0 {
		return nil
	}
	newFragment := &regionFragment{
		region: frag,
		offset: off,
		size:   size,
	}

	newFrags := make([]*regionFragment, 0, len(f.fragments)+1)
	inserted := false
	for _, old := range f.fragments {
		if old.end() <= newFragment.start() || old.start() >= newFragment.end() {
			if !inserted && newFragment.end() <= old.start() {
				newFrags = append(newFrags, newFragment)
				inserted = true
			}
			newFrags = append(newFrags, old)
			continue
		}

		if old.start() < newFragment.start() {
			left := old.cutAt(newFragment.start() - old.start())
			if left == nil {
				return fmt.Errorf("invalid left fragment split")
			}
			newFrags = append(newFrags, left)
		}
		if !inserted {
			newFrags = append(newFrags, newFragment)
			inserted = true
		}
		if old.end() > newFragment.end() {
			right := old.offsetAt(newFragment.end() - old.start())
			if right == nil {
				return fmt.Errorf("invalid right fragment split")
			}
			newFrags = append(newFrags, right)
		}
	}
	if !inserted {
		newFrags = append(newFrags, newFragment)
	}
	f.fragments = newFrags

	return nil
}

func regionToString(region MemoryRegion) string {
	switch region := region.(type) {
	case *OffsetRegion:
		regionStr := regionToString(region.base)
		if regionStr != "" {
			return fmt.Sprintf("%s offset=%016X", regionStr, region.offset)
		} else {
			return ""
		}
	case *PaddedRegion:
		regionStr := regionToString(region.Region)
		if regionStr != "" {
			return fmt.Sprintf("%s padded=%016X", regionStr, region.RegionSize)
		} else {
			return ""
		}
	default:
		return fmt.Sprintf("%+v", region)
	}
}

func (f *fragmentedRegion) dumpMap(out io.Writer, off uint64) error {
	for _, frag := range f.fragments {
		str := regionToString(frag.region)

		if str != "" {
			line := fmt.Sprintf("  %016X: %s", off+uint64(frag.offset), regionToString(frag.region))

			if _, err := fmt.Fprintf(out, "%s\n", line); err != nil {
				return err
			}
		}
	}

	return nil
}

func newFragmentRegion(pageSize uint32) *fragmentedRegion {
	return &fragmentedRegion{
		fragments: []*regionFragment{
			{region: make(RawRegion, pageSize), offset: 0, size: int64(pageSize)},
		},
		totalSize: uint64(pageSize),
	}
}

var (
	_ MemoryRegion = &fragmentedRegion{}
)
