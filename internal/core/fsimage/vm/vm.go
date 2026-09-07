package vm

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
)

type PageList [256]MemoryRegion

// PageTable interface abstracts page table operations for different implementations
type PageTable interface {
	Get(index uint64) MemoryRegion
	Set(index uint64, region MemoryRegion)
	Clear()
	ForEach(fn func(index uint64, region MemoryRegion) bool)
}

// MultiLevelPageTable implements a multi-level page table with lazy allocation
type MultiLevelPageTable struct {
	pageListCapacity uint64
	totalPages       uint64
	topLevel         []*PageList
}

// Get retrieves a MemoryRegion at the given index
func (pt *MultiLevelPageTable) Get(index uint64) MemoryRegion {
	if index >= pt.totalPages {
		return nil
	}

	topIndex := index / pt.pageListCapacity
	pageIndex := index % pt.pageListCapacity

	if topIndex >= uint64(len(pt.topLevel)) || pt.topLevel[topIndex] == nil {
		return nil
	}

	return pt.topLevel[topIndex][pageIndex]
}

// Set stores a MemoryRegion at the given index, allocating intermediate tables as needed
func (pt *MultiLevelPageTable) Set(index uint64, region MemoryRegion) {
	if index >= pt.totalPages {
		return
	}

	topIndex := index / pt.pageListCapacity
	pageIndex := index % pt.pageListCapacity

	// Ensure we have enough top-level entries
	for uint64(len(pt.topLevel)) <= topIndex {
		pt.topLevel = append(pt.topLevel, nil)
	}

	// Allocate PageList if it doesn't exist
	if pt.topLevel[topIndex] == nil {
		pt.topLevel[topIndex] = &PageList{}
	}

	pt.topLevel[topIndex][pageIndex] = region
}

// Clear resets all page table entries
func (pt *MultiLevelPageTable) Clear() {
	for i := range pt.topLevel {
		pt.topLevel[i] = nil
	}
	pt.topLevel = pt.topLevel[:0]
}

// ForEach iterates over all non-nil regions in the page table
func (pt *MultiLevelPageTable) ForEach(fn func(index uint64, region MemoryRegion) bool) {
	for topIndex, pageList := range pt.topLevel {
		if pageList == nil {
			continue
		}

		for pageIndex, region := range pageList {
			if region == nil {
				continue
			}

			globalIndex := uint64(topIndex)*pt.pageListCapacity + uint64(pageIndex)
			if !fn(globalIndex, region) {
				return
			}
		}
	}
}

// NewMultiLevelPageTable creates a new multi-level page table
func NewMultiLevelPageTable(totalPages uint64) *MultiLevelPageTable {
	return &MultiLevelPageTable{
		pageListCapacity: 256, // Same as PageList size
		totalPages:       totalPages,
		topLevel:         make([]*PageList, 0),
	}
}

type VirtualMemory struct {
	pageSize uint32
	// The size of a page is always pageSize.
	// Any pages smaller must be fragmentedRegions and any pages larger must be split into OffsetRegions.
	pages      PageTable
	writePages PageTable
	totalSize  int64

	// stats
	totalMaps         int64
	totalMapRegions   int64
	totalMapFragments int64
}

func (vm *VirtualMemory) PageSize() uint32 { return vm.pageSize }

func (vm *VirtualMemory) mapFragment(region MemoryRegion, offset int64) error {
	vm.totalMapFragments += 1

	// slog.Info("mapFragment", "offset", offset)
	// Get the region index.
	regionIndex := offset / int64(vm.pageSize)
	regionOffset := offset % int64(vm.pageSize)

	existingRegion := vm.writePages.Get(uint64(regionIndex))
	if existingRegion == nil {
		existingRegion = vm.pages.Get(uint64(regionIndex))
	}
	if existingRegion != nil {
		// if a region already exists then check if it's a fragmentedRegion.
		if frag, ok := existingRegion.(*fragmentedRegion); ok {
			// If it's already a fragmentedRegion then map the new part.

			if err := frag.mapFragment(region, regionOffset); err != nil {
				return err
			}
			vm.pages.Set(uint64(regionIndex), frag)
			vm.writePages.Set(uint64(regionIndex), nil)
			return nil
		} else {
			// Otherwise it's something else.

			// slog.Info("map existingRegion into fragmentRegion",
			// 	"pageSize", vm.pageSize,
			// 	"regionIndex", regionIndex,
			// 	"existingRegion", existingRegion,
			// )

			newFrag := newFragmentRegion(vm.pageSize)

			// Add the existing region.
			if err := newFrag.mapFragment(NewTruncatedRegion(existingRegion, int64(vm.pageSize)), 0); err != nil {
				return errors.Join(fmt.Errorf("failed to preserve existing page fragment"), err)
			}

			// Add the new fragment last so it can overwrite the old region.
			if err := newFrag.mapFragment(region, int64(regionOffset)); err != nil {
				return errors.Join(fmt.Errorf("failed to map fragment"), err)
			}

			// slog.Info("", "newFrag", newFrag)

			vm.pages.Set(uint64(regionIndex), newFrag)
			vm.writePages.Set(uint64(regionIndex), nil)

			return nil
		}
	} else {
		newFrag := newFragmentRegion(vm.pageSize)

		if err := newFrag.mapFragment(region, int64(regionOffset)); err != nil {
			return errors.Join(fmt.Errorf("failed to map fragment"), err)
		}

		vm.pages.Set(uint64(regionIndex), newFrag)
		vm.writePages.Set(uint64(regionIndex), nil)

		return nil
	}
}

func (vm *VirtualMemory) mapRegion(region MemoryRegion, offset int64) error {
	vm.totalMapRegions += 1

	if offset%int64(vm.pageSize) != 0 {
		return fmt.Errorf("attempted to use mapRegion to map an unaligned region")
	}

	index := offset / int64(vm.pageSize)

	vm.pages.Set(uint64(index), region)
	vm.writePages.Set(uint64(index), nil)

	return nil
}

// Size implements MemoryRegion.
func (vm *VirtualMemory) Size() int64 {
	return vm.totalSize
}

// Map a memory region.
func (vm *VirtualMemory) Map(region MemoryRegion, offset int64) error {
	// Unlike physical hardware this virtual memory system can handle non-aligned pages by further subdividing pages.
	// Therefore this map function needs to handle 3 different regions.
	// A potentially oddly sized region at the start.
	// A normal series of page aligned regions in the middle.
	// A potentially oddly sized region at the end.

	vm.totalMaps += 1

	// slog.Info("map", "region", region, "offset", offset)

	if region == nil {
		return fmt.Errorf("cannot map a nil region")
	}
	regionSize := region.Size()
	if offset < 0 || regionSize < 0 || offset > vm.totalSize || regionSize > vm.totalSize-offset {
		return fmt.Errorf("mapping offset %d size %d exceeds virtual memory size %d", offset, regionSize, vm.totalSize)
	}

	var regionOffset int64 = 0

	// Check if the offset is aligned.
	if offset%int64(vm.pageSize) != 0 {
		fragmentSize := int64(vm.pageSize) - (offset % int64(vm.pageSize))
		if fragmentSize > regionSize {
			fragmentSize = regionSize
		}
		if err := vm.mapFragment(NewTruncatedRegion(region, fragmentSize), offset); err != nil {
			return errors.Join(fmt.Errorf("failed to map leading fragment"), err)
		}

		// Update the offsets.
		regionOffset += fragmentSize
		offset += fragmentSize
	}

	for {
		// If we have no full sized regions left then break.
		if (regionSize - regionOffset) < int64(vm.pageSize) {
			break
		}

		// Map a region.
		if regionOffset == 0 {
			if err := vm.mapRegion(region, offset); err != nil {
				return errors.Join(fmt.Errorf("failed to map region"), err)
			}
		} else {
			if err := vm.mapRegion(NewOffsetRegion(region, regionOffset), offset); err != nil {
				return errors.Join(fmt.Errorf("TODO: %v", err), err)
			}
		}

		// Update the offsets.
		regionOffset += int64(vm.pageSize)
		offset += int64(vm.pageSize)
	}

	// Check for the final oddly sized region.
	if (regionSize - regionOffset) > 0 {
		if err := vm.mapFragment(NewOffsetRegion(region, regionOffset), offset); err != nil {
			return errors.Join(fmt.Errorf("TODO: %v", err), err)
		}
	}

	return nil
}

// MapSlice maps a slice of a memory region directly without creating intermediate objects
func (vm *VirtualMemory) MapSlice(region MemoryRegion, srcOffset, size, dstOffset int64) error {
	// Create a temporary region that views the slice directly
	if srcOffset == 0 && size == region.Size() {
		// Optimization: if mapping the entire region, use it directly
		return vm.Map(region, dstOffset)
	}

	// Create an efficient slice view using a custom region
	sliceRegion := &directSliceRegion{
		base:   region,
		offset: srcOffset,
		size:   size,
	}

	return vm.Map(sliceRegion, dstOffset)
}

// Helper function to map a file into a given region.
// The file will be read on demand.
func (vm *VirtualMemory) MapFile(f io.ReaderAt, offset int64, size int64) (*FileRegion, error) {
	ret := &FileRegion{f: f, totalSize: size}

	if err := vm.Map(ret, offset); err != nil {
		return nil, errors.Join(fmt.Errorf("TODO"), err)
	}

	return ret, nil
}

// Copy the data at offset into newRegion and replace the pages there with newRegion.
// This function reinterprets file contents with a new datatype.
func (vm *VirtualMemory) Reinterpret(newRegion MemoryRegion, offset int64) error {
	// Copy the old data to the new structure.
	if _, err := io.Copy(
		io.NewOffsetWriter(newRegion, 0),
		io.NewSectionReader(vm, int64(offset), int64(newRegion.Size())),
	); err != nil {
		return errors.Join(fmt.Errorf("TODO: %v", err), err)
	}

	// Map the region.
	if err := vm.Map(newRegion, offset); err != nil {
		return errors.Join(fmt.Errorf("TODO: %v", err), err)
	}

	return nil
}

func (vm *VirtualMemory) DumpMap(out io.Writer) error {
	// Dump the entire memory map to out.

	var dumpErr error
	vm.pages.ForEach(func(off uint64, region MemoryRegion) bool {
		switch region := region.(type) {
		case *fragmentedRegion:
			if _, err := fmt.Fprintf(out, "%016X: fragmented\n", off*uint64(vm.pageSize)); err != nil {
				dumpErr = err
				return false
			}
			if err := region.dumpMap(out, off*uint64(vm.pageSize)); err != nil {
				dumpErr = err
				return false
			}
		default:
			regionStr := regionToString(region)

			if _, err := fmt.Fprintf(out, "%016X: %s\n", off*uint64(vm.pageSize), regionStr); err != nil {
				dumpErr = err
				return false
			}
		}
		return true
	})

	return dumpErr
}

func (vm *VirtualMemory) getRegion(offset int64, isWrite bool) (MemoryRegion, int64, error) {
	// If the offset is more than the total size then return EOF.
	if offset > vm.totalSize {
		return nil, 0, io.EOF
	}

	// Calculate the region index and region offset.
	regionIndex := offset / int64(vm.pageSize)
	regionOffset := offset % int64(vm.pageSize)

	if region := vm.writePages.Get(uint64(regionIndex)); region != nil {
		return region, regionOffset, nil
	}

	if isWrite {
		newWritePage := make(RawRegion, vm.pageSize)
		if existingRegion := vm.pages.Get(uint64(regionIndex)); existingRegion != nil {
			if _, err := existingRegion.ReadAt(newWritePage, 0); err != nil {
				return nil, 0, err
			}
		}
		vm.writePages.Set(uint64(regionIndex), &newWritePage)
		return vm.writePages.Get(uint64(regionIndex)), regionOffset, nil
	}

	region := vm.pages.Get(uint64(regionIndex))
	if region == nil {
		// Reading from unmapped region returns zeros.
		return nil, regionOffset, nil
	}
	return region, regionOffset, nil
}

// ReadAt implements io.ReaderAt.
func (vm *VirtualMemory) ReadAt(p []byte, off int64) (n int, err error) {
	// Keep looping until we've finished the entire read.
	for {
		// Get the region at offset.
		region, regionOffset, err := vm.getRegion(off, false)
		if err != nil {
			return 0, err
		}

		readSize := 0

		if region != nil {
			// If the region exists then forward the read to the region.
			end := int64(vm.pageSize)
			if int64(len(p))+regionOffset < end {
				end = int64(len(p)) + regionOffset
			}
			readSize, err = region.ReadAt(p[:end-regionOffset], regionOffset)
			if err != nil {
				return 0, err
			}
		} else {
			// Otherwise advance the read pointer by the readOffset - pageSize.
			readSize = int(vm.pageSize) - int(regionOffset)

			if readSize > len(p) {
				readSize = len(p)
			}

			if readSize != 0 {
				// Make sure to zero the data.
				copy(p, make([]byte, readSize))
			}
		}

		n += readSize
		p = p[readSize:]
		off += int64(readSize)

		if len(p) == 0 {
			break
		}
	}

	return
}

// WriteAt implements io.WriterAt.
func (vm *VirtualMemory) WriteAt(p []byte, off int64) (n int, err error) {
	// Keep looping until we've finished the entire write.
	for {
		// Get the region at offset.
		region, regionOffset, err := vm.getRegion(off, true)
		if err != nil {
			return 0, err
		}

		writeSize := 0

		if region != nil {
			// If the region exists then forward the write to the region.
			end := int64(vm.pageSize)
			if int64(len(p))+regionOffset < end {
				end = int64(len(p)) + regionOffset
			}
			writeSize, err = region.WriteAt(p[:end-regionOffset], regionOffset)
			if err != nil {
				slog.Error("VirtualMemory WriteAt Error", "len", len(p), "off", off, "regionOffset", regionOffset)
				return 0, err
			}
		} else {
			return 0, fmt.Errorf("write to unmapped page at: %X", off)
		}

		n += writeSize
		p = p[writeSize:]
		off += int64(writeSize)

		if len(p) == 0 {
			break
		}
	}

	return
}

// Large buffer pool for batching consecutive pages (default 2MB = 512 pages of 4KB)
var batchBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 2*1024*1024) // 2MB buffer for batching
	},
}

func (vm *VirtualMemory) WriteSparseTo(fh io.WriterAt) (int64, error) {
	// Early exit if VM has no mapped pages
	mlpt, ok := vm.pages.(*MultiLevelPageTable)
	writeMLPT, writeOK := vm.writePages.(*MultiLevelPageTable)
	if ok && writeOK && len(mlpt.topLevel) == 0 && len(writeMLPT.topLevel) == 0 {
		return 0, nil
	}

	var written int64

	// Get large buffer from pool for batching
	batchBuf := batchBufferPool.Get().([]byte)
	defer batchBufferPool.Put(batchBuf)

	// Calculate max pages per batch
	maxPagesPerBatch := len(batchBuf) / int(vm.pageSize)
	if maxPagesPerBatch == 0 {
		maxPagesPerBatch = 1
	}

	// Batch state
	var batchStartOffset int64
	var batchPageCount int
	var lastPageIndex uint64 = ^uint64(0) // Invalid initial value

	// Flush function for writing accumulated batch
	flushBatch := func() error {
		if batchPageCount == 0 {
			return nil
		}

		bytesToWrite := batchPageCount * int(vm.pageSize)
		n, err := fh.WriteAt(batchBuf[:bytesToWrite], batchStartOffset)
		if err != nil {
			return err
		}
		written += int64(n)
		batchPageCount = 0
		return nil
	}

	// Use ForEach to iterate only over non-nil regions (sparse optimization)
	var writeErr error
	type mappedPage struct {
		index  uint64
		region MemoryRegion
	}
	forEachMappedPage := func(fn func(pageIndex uint64, region MemoryRegion) bool) {
		mapped := make(map[uint64]MemoryRegion)
		vm.pages.ForEach(func(pageIndex uint64, region MemoryRegion) bool {
			mapped[pageIndex] = region
			return true
		})
		vm.writePages.ForEach(func(pageIndex uint64, region MemoryRegion) bool {
			mapped[pageIndex] = region
			return true
		})
		pages := make([]mappedPage, 0, len(mapped))
		for index, region := range mapped {
			pages = append(pages, mappedPage{index: index, region: region})
		}
		sort.Slice(pages, func(i, j int) bool {
			return pages[i].index < pages[j].index
		})
		for _, page := range pages {
			if !fn(page.index, page.region) {
				return
			}
		}
	}
	forEachMappedPage(func(pageIndex uint64, region MemoryRegion) bool {
		// Check if this page is consecutive with the last one
		isConsecutive := (lastPageIndex != ^uint64(0)) && (pageIndex == lastPageIndex+1)

		// If not consecutive or batch is full, flush current batch
		if !isConsecutive || batchPageCount >= maxPagesPerBatch {
			if err := flushBatch(); err != nil {
				writeErr = err
				return false
			}
			// Start new batch
			batchStartOffset = int64(pageIndex) * int64(vm.pageSize)
		}

		// Read the page into the batch buffer
		bufOffset := batchPageCount * int(vm.pageSize)
		n, err := region.ReadAt(batchBuf[bufOffset:bufOffset+int(vm.pageSize)], 0)
		if err != nil {
			writeErr = err
			return false
		}

		// Ensure we read a full page
		if n != int(vm.pageSize) {
			writeErr = fmt.Errorf("incomplete page read: expected %d, got %d", vm.pageSize, n)
			return false
		}

		batchPageCount++
		lastPageIndex = pageIndex
		return true
	})

	// Flush any remaining batch
	if writeErr == nil {
		writeErr = flushBatch()
	}

	return written, writeErr
}

func (vm *VirtualMemory) Reset() error {
	// Clear all the old pages and write pages.
	vm.pages.Clear()
	vm.writePages.Clear()

	return nil
}

func (vm *VirtualMemory) DumpStats() {
	slog.Info("vm stats",
		"totalMaps", vm.totalMaps,
		"totalMapFragments", vm.totalMapFragments,
		"totalMapRegions", vm.totalMapRegions,
		"totalNewOffsetRegionCalls", totalNewOffsetRegionCalls,
	)
}

func (vm *VirtualMemory) DebugOffset(off int64) string {
	region, regionOffset, err := vm.getRegion(off, true)
	if err != nil {
		return fmt.Sprintf("<error: %s>", err)
	}

	switch region := region.(type) {
	case *TruncatedRegion:
		switch childRegion := region.Region.(type) {
		case *PaddedRegion:
			return fmt.Sprintf("<%T:%T:%T %d>", region, region.Region, childRegion.Region, regionOffset)
		default:
			return fmt.Sprintf("<%T:%T %d>", region, region.Region, regionOffset)
		}
	default:
		return fmt.Sprintf("<%T %d>", region, regionOffset)
	}
}

type MappingKind string

const (
	MappingKindUnknown   = MappingKind("unknown")
	MappingKindNone      = MappingKind("none")
	MappingKindRaw       = MappingKind("raw")
	MappingKindBitmap    = MappingKind("bitmap")
	MappingKindGenerated = MappingKind("generated")
	MappingKindCustom    = MappingKind("custom")
)

type Mapping struct {
	Offset   int64       `json:"offset"`
	Size     int64       `json:"size"`
	Kind     MappingKind `json:"kind"`
	TypeName string      `json:"typeName,omitempty"` // Optional type name for custom regions
}

type MappingArray []Mapping

func (m MappingArray) Validate() error {
	// check that region+region.size == next region.offset
	if len(m) == 0 {
		return nil
	}
	for i := 0; i < len(m)-1; i++ {
		if m[i].Offset+m[i].Size != m[i+1].Offset {
			return fmt.Errorf("mapping %d: offset + size (%d + %d) != next offset (%d)", i, m[i].Offset, m[i].Size, m[i+1].Offset)
		}
	}
	// check that the last region's size is not zero
	if m[len(m)-1].Size <= 0 {
		return fmt.Errorf("last mapping size is zero: %v", m[len(m)-1])
	}
	return nil
}

func (vm *VirtualMemory) DumpMappings() (MappingArray, error) {
	var mappings MappingArray

	var i int64 = 0
	for {
		region, _, err := vm.getRegion(int64(i), false)
		if err != nil {
			return nil, fmt.Errorf("failed to get region at %d: %w", i, err)
		}

		if region == nil {
			// if the last region is already none then merge them together
			if len(mappings) > 0 && mappings[len(mappings)-1].Kind == MappingKindNone {
				// Merge the last mapping with the current one.
				mappings[len(mappings)-1].Size += int64(vm.pageSize)
				slog.Debug("DumpMappings", "offset", i, "size", vm.pageSize, "kind", MappingKindNone, "merged", true)
				i += int64(vm.pageSize)
				continue
			} else {
				// If the region is nil, it means it's unmapped.
				mappings = append(mappings, Mapping{
					Offset: i,
					Size:   int64(vm.pageSize),
					Kind:   MappingKindNone,
				})
				slog.Debug("DumpMappings", "offset", i, "size", vm.pageSize, "kind", MappingKindNone)
				i += int64(vm.pageSize)
				continue
			}
		}

		childMappings, size, err := vm.resolveMappingsFromRegion(region, i)
		if err != nil {
			return nil, fmt.Errorf("failed to get mapping kind for region at %d: %w", i, err)
		}

		mappings = append(mappings, childMappings...)
		slog.Debug("DumpMappings", "offset", i, "size", size, "kind", childMappings[0].Kind)
		i += size
		if i >= vm.totalSize {
			break // Reached the end of the virtual memory.
		}
	}

	if err := mappings.Validate(); err != nil {
		return nil, fmt.Errorf("failed to validate mappings: %w", err)
	}

	return mappings, nil
}

type GeneratedRegion interface {
	MemoryRegion
	TagGenerated()
}

func (vm *VirtualMemory) resolveMappingsFromRegion(region MemoryRegion, offset int64) ([]Mapping, int64, error) {
	switch region := region.(type) {
	case *RawRegion, RawRegion:
		return []Mapping{
			{
				Offset: offset,
				Size:   region.Size(),
				Kind:   MappingKindRaw,
			},
		}, region.Size(), nil
	case *fragmentedRegion:
		mappings := []Mapping{}
		size := int64(0)
		for _, frag := range region.fragments {
			childMappings, childSize, err := vm.resolveMappingsFromRegion(frag.region, offset+size)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to resolve mappings from fragmented region: %w", err)
			}
			mappings = append(mappings, childMappings...)
			size += childSize
		}
		return mappings, size, nil
	case GeneratedRegion:
		// If the region is a generated region, we can treat it as a raw region
		return []Mapping{
			{
				Offset:   offset,
				Size:     region.Size(),
				Kind:     MappingKindGenerated,
				TypeName: fmt.Sprintf("%T", region), // Include type name for generated regions
			},
		}, region.Size(), nil
	case *OffsetRegion:
		// resolve the mappings from the child region
		return vm.resolveMappingsFromRegion(region.base, offset)
	case *TruncatedRegion:
		// resolve the mappings from the child region
		childMappings, size, err := vm.resolveMappingsFromRegion(region.Region, offset)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to resolve mappings from truncated region: %w", err)
		}
		// if size is less then the MaxSize then remove any extra mappings beyond the MaxSize
		if size > region.MaxSize {
			size = region.MaxSize
			var ret []Mapping
			for _, m := range childMappings {
				if m.Size > region.MaxSize {
					m.Size = region.MaxSize
				}
				region.MaxSize -= m.Size
				if region.MaxSize <= 0 {
					break
				}
				ret = append(ret, m)
			}
			return ret, size, nil
		} else {
			return childMappings, size, nil
		}
	case *PaddedRegion:
		// resolve the mappings from the child region
		childMappings, size, err := vm.resolveMappingsFromRegion(region.Region, offset)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to resolve mappings from padded region: %w", err)
		}
		// Padded regions are padded with a raw region if they are smaller than the padded size.
		if size < region.RegionSize {
			paddingSize := region.RegionSize - size
			childMappings = append(childMappings, Mapping{
				Offset: offset + size,
				Size:   paddingSize,
				Kind:   MappingKindNone,
			})
			size += paddingSize
		}
		return childMappings, size, nil
	case *BitmapRegion:
		// Bitmap regions are treated as a raw region with a bitmap kind.
		return []Mapping{
			{
				Offset: offset,
				Size:   region.Size(),
				Kind:   MappingKindBitmap,
			},
		}, region.Size(), nil
	case GenericRegionArray:
		var mappings []Mapping
		var totalSize int64 = 0
		for _, r := range region.GetRegions() {
			childMappings, childSize, err := vm.resolveMappingsFromRegion(r, offset+totalSize)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to resolve mappings from generic region array: %w", err)
			}
			mappings = append(mappings, childMappings...)
			totalSize += childSize
		}
		return mappings, totalSize, nil
	default:
		// For any other region type, we can treat it as a custom region.
		// This is a fallback for any custom regions that don't fit the above cases.
		return []Mapping{
			{
				Offset:   offset,
				Size:     region.Size(),
				Kind:     MappingKindCustom,
				TypeName: fmt.Sprintf("%T", region), // Include type name for custom regions
			},
		}, region.Size(), nil
	}
}

var (
	_ MemoryRegion = &VirtualMemory{}
)

func NewVirtualMemory(totalSize int64, pageSize uint32) *VirtualMemory {
	if totalSize%int64(pageSize) != 0 {
		panic("totalSize%int64(pageSize) != 0")
	}

	totalPages := uint64(totalSize / int64(pageSize))

	return &VirtualMemory{
		pageSize:   pageSize,
		totalSize:  totalSize,
		pages:      NewMultiLevelPageTable(totalPages),
		writePages: NewMultiLevelPageTable(totalPages),
	}
}

// Compile-time check that VirtualMemory implements VirtualStorage
var _ VirtualStorage = (*VirtualMemory)(nil)
