package fat

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FATTableRegion implements vm.MemoryRegion for FAT table operations
type FATTableRegion struct {
	fatType        string   // "FAT12", "FAT16", or "FAT32"
	entries        []uint32 // Unified storage for all FAT entry values
	totalSize      int64    // Size in bytes
	eocValue       uint32   // End-of-chain value for this FAT type
	freeValue      uint32   // Free cluster value (always 0)
	allocatedValue uint32   // Temporarily allocated cluster marker
	nextFree       uint32   // Hint for next free cluster
}

// NewFATTableRegion creates a new FAT table region for the specified type and cluster count
func NewFATTableRegion(fatType string, totalClusters uint32) *FATTableRegion {
	var eocValue uint32
	var allocatedValue uint32
	var entrySize int

	switch fatType {
	case "FAT12":
		eocValue = FAT12_EOC
		allocatedValue = 0xFFF // Max 12-bit value as temporary marker
		entrySize = 3          // 12 bits = 1.5 bytes per entry, but we pack 2 entries in 3 bytes
	case "FAT16":
		eocValue = FAT16_EOC
		allocatedValue = 0xFFFF // Max 16-bit value as temporary marker
		entrySize = 2
	case "FAT32":
		eocValue = FAT32_EOC
		allocatedValue = 0x0FFFFFFF // Max 28-bit value as temporary marker
		entrySize = 4
	default:
		panic(fmt.Sprintf("unsupported FAT type: %s", fatType))
	}

	// Calculate total size in bytes
	var totalSize int64
	if fatType == "FAT12" {
		// FAT12: 2 entries per 3 bytes, round up
		totalSize = int64((totalClusters*3 + 1) / 2)
	} else {
		totalSize = int64(totalClusters) * int64(entrySize)
	}

	return &FATTableRegion{
		fatType:        fatType,
		entries:        make([]uint32, totalClusters),
		totalSize:      totalSize,
		eocValue:       eocValue,
		freeValue:      0,
		allocatedValue: allocatedValue,
		nextFree:       2, // Clusters 0 and 1 are reserved
	}
}

// MemoryRegion interface implementation

func (f *FATTableRegion) Size() int64 {
	return f.totalSize
}

func (f *FATTableRegion) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= f.totalSize {
		return 0, io.EOF
	}

	maxRead := len(p)
	if off+int64(maxRead) > f.totalSize {
		maxRead = int(f.totalSize - off)
	}

	// Convert internal entries to the appropriate byte format
	switch f.fatType {
	case "FAT12":
		return f.readFAT12(p, off, maxRead)
	case "FAT16":
		return f.readFAT16(p, off, maxRead)
	case "FAT32":
		return f.readFAT32(p, off, maxRead)
	default:
		return 0, fmt.Errorf("unsupported FAT type: %s", f.fatType)
	}
}

func (f *FATTableRegion) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= f.totalSize {
		return 0, io.ErrUnexpectedEOF
	}

	maxWrite := len(p)
	if off+int64(maxWrite) > f.totalSize {
		maxWrite = int(f.totalSize - off)
	}

	// Parse bytes and update internal entries
	switch f.fatType {
	case "FAT12":
		return f.writeFAT12(p, off, maxWrite)
	case "FAT16":
		return f.writeFAT16(p, off, maxWrite)
	case "FAT32":
		return f.writeFAT32(p, off, maxWrite)
	default:
		return 0, fmt.Errorf("unsupported FAT type: %s", f.fatType)
	}
}

// High-level FAT operations

// SetEntry sets the FAT entry for the specified cluster
func (f *FATTableRegion) SetEntry(cluster uint32, value uint32) error {
	if cluster >= uint32(len(f.entries)) {
		return fmt.Errorf("cluster %d out of range", cluster)
	}

	// Mask value appropriately for FAT type
	switch f.fatType {
	case "FAT12":
		value &= 0xFFF
	case "FAT16":
		value &= 0xFFFF
	case "FAT32":
		value &= 0x0FFFFFFF // 28 bits used in FAT32
	}

	f.entries[cluster] = value

	return nil
}

// GetEntry returns the FAT entry for the specified cluster
func (f *FATTableRegion) GetEntry(cluster uint32) uint32 {
	if cluster >= uint32(len(f.entries)) {
		return f.eocValue // Return EOC for out-of-range clusters
	}
	return f.entries[cluster]
}

// MarkEndOfChain marks the cluster as end-of-chain
func (f *FATTableRegion) MarkEndOfChain(cluster uint32) error {
	return f.SetEntry(cluster, f.eocValue)
}

// MarkFree marks the cluster as free
func (f *FATTableRegion) MarkFree(cluster uint32) error {
	return f.SetEntry(cluster, f.freeValue)
}

// getAllocatedMarker returns the temporary allocated marker for this FAT type
func (f *FATTableRegion) getAllocatedMarker() uint32 {
	return f.allocatedValue
}

// IsAllocated checks if the cluster is temporarily allocated (not yet in chain)
func (f *FATTableRegion) IsAllocated(cluster uint32) bool {
	if cluster >= uint32(len(f.entries)) {
		return false
	}
	return f.entries[cluster] == f.allocatedValue
}

// IsFree checks if the cluster is free
func (f *FATTableRegion) IsFree(cluster uint32) bool {
	if cluster >= uint32(len(f.entries)) {
		return false
	}
	return f.entries[cluster] == f.freeValue
}

// GetFreeClusterCount returns the number of free clusters
func (f *FATTableRegion) GetFreeClusterCount() uint32 {
	var freeCount uint32
	// Start from cluster 2 (0 and 1 are reserved)
	for i := uint32(2); i < uint32(len(f.entries)); i++ {
		if f.entries[i] == f.freeValue {
			freeCount++
		}
	}
	return freeCount
}

// GetAllocatedClusterCount returns the number of allocated (but not yet in chain) clusters
func (f *FATTableRegion) GetAllocatedClusterCount() uint32 {
	var allocatedCount uint32
	for i := uint32(2); i < uint32(len(f.entries)); i++ {
		if f.entries[i] == f.allocatedValue {
			allocatedCount++
		}
	}
	return allocatedCount
}

// AllocateCluster finds and marks the next free cluster
func (f *FATTableRegion) AllocateCluster() (uint32, error) {
	// Search for a free cluster starting from hint
	for cluster := f.nextFree; cluster < uint32(len(f.entries)); cluster++ {
		if f.entries[cluster] == f.freeValue {
			// Immediately mark as allocated to prevent double-allocation
			f.entries[cluster] = f.getAllocatedMarker()
			f.nextFree = cluster + 1
			return cluster, nil
		}
	}

	// Search from beginning if needed
	for cluster := uint32(2); cluster < f.nextFree; cluster++ {
		if f.entries[cluster] == f.freeValue {
			// Immediately mark as allocated to prevent double-allocation
			f.entries[cluster] = f.getAllocatedMarker()
			f.nextFree = cluster + 1
			return cluster, nil
		}
	}

	return 0, fmt.Errorf("no free clusters available")
}

// IsEndOfChain checks if the cluster value represents end-of-chain
func (f *FATTableRegion) IsEndOfChain(value uint32) bool {
	switch f.fatType {
	case "FAT12":
		return value >= FAT12_EOC
	case "FAT16":
		return value >= FAT16_EOC
	case "FAT32":
		return value >= FAT32_EOC
	default:
		return false
	}
}

// GetClusterChain returns the complete cluster chain starting from the given cluster
func (f *FATTableRegion) GetClusterChain(startCluster uint32) []uint32 {
	if startCluster < 2 {
		return nil
	}

	var chain []uint32
	current := startCluster

	for !f.IsEndOfChain(current) && current >= 2 {
		chain = append(chain, current)
		next := f.GetEntry(current)

		// Prevent infinite loops
		if next == current {
			break
		}
		current = next
	}

	return chain
}

// FAT12 specific byte conversion methods

func (f *FATTableRegion) readFAT12(p []byte, off int64, maxRead int) (n int, err error) {
	// FAT12 packs two 12-bit entries into 3 bytes
	bytesRead := 0

	for bytesRead < maxRead {
		byteIndex := off + int64(bytesRead)
		entryPairIndex := byteIndex / 3 * 2 // Each 3-byte group contains 2 entries

		if entryPairIndex >= int64(len(f.entries)-1) {
			break
		}

		// Get the two entries for this 3-byte group
		entry0 := f.entries[entryPairIndex] & 0xFFF
		entry1 := f.entries[entryPairIndex+1] & 0xFFF

		// Pack into 3 bytes: [entry0_low] [entry0_high|entry1_low] [entry1_high]
		byte0 := uint8(entry0 & 0xFF)
		byte1 := uint8((entry0>>8)&0x0F) | uint8((entry1&0x0F)<<4)
		byte2 := uint8((entry1 >> 4) & 0xFF)

		// Write the appropriate byte based on offset within the 3-byte group
		switch byteIndex % 3 {
		case 0:
			p[bytesRead] = byte0
		case 1:
			p[bytesRead] = byte1
		case 2:
			p[bytesRead] = byte2
		}

		bytesRead++
	}

	return bytesRead, nil
}

func (f *FATTableRegion) writeFAT12(p []byte, off int64, maxWrite int) (n int, err error) {
	// FAT12 writes are complex due to 12-bit packing
	// For simplicity, we'll update entries by reconstructing from bytes
	bytesWritten := 0

	for bytesWritten < maxWrite {
		byteIndex := off + int64(bytesWritten)
		entryPairIndex := byteIndex / 3 * 2

		if entryPairIndex >= int64(len(f.entries)-1) {
			break
		}

		// Read the current 3-byte group
		var bytes [3]uint8
		if byteIndex%3 == 0 {
			// Starting new 3-byte group, may need to preserve other bytes
			entry0 := f.entries[entryPairIndex] & 0xFFF
			entry1 := f.entries[entryPairIndex+1] & 0xFFF
			bytes[0] = uint8(entry0 & 0xFF)
			bytes[1] = uint8((entry0>>8)&0x0F) | uint8((entry1&0x0F)<<4)
			bytes[2] = uint8((entry1 >> 4) & 0xFF)
		}

		// Update the specific byte
		bytes[byteIndex%3] = p[bytesWritten]

		// Unpack back to entries
		entry0 := uint32(bytes[0]) | (uint32(bytes[1]&0x0F) << 8)
		entry1 := (uint32(bytes[1]) >> 4) | (uint32(bytes[2]) << 4)

		f.entries[entryPairIndex] = entry0 & 0xFFF
		f.entries[entryPairIndex+1] = entry1 & 0xFFF

		bytesWritten++
	}

	return bytesWritten, nil
}

// FAT16 specific byte conversion methods

func (f *FATTableRegion) readFAT16(p []byte, off int64, maxRead int) (n int, err error) {
	bytesRead := 0

	for bytesRead < maxRead-1 { // Read 2 bytes at a time
		entryIndex := (off + int64(bytesRead)) / 2

		if entryIndex >= int64(len(f.entries)) {
			break
		}

		value := f.entries[entryIndex] & 0xFFFF
		binary.LittleEndian.PutUint16(p[bytesRead:], uint16(value))
		bytesRead += 2
	}

	return bytesRead, nil
}

func (f *FATTableRegion) writeFAT16(p []byte, off int64, maxWrite int) (n int, err error) {
	bytesWritten := 0

	for bytesWritten < maxWrite-1 { // Write 2 bytes at a time
		entryIndex := (off + int64(bytesWritten)) / 2

		if entryIndex >= int64(len(f.entries)) {
			break
		}

		value := binary.LittleEndian.Uint16(p[bytesWritten:])
		f.entries[entryIndex] = uint32(value)
		bytesWritten += 2
	}

	return bytesWritten, nil
}

// FAT32 specific byte conversion methods

func (f *FATTableRegion) readFAT32(p []byte, off int64, maxRead int) (n int, err error) {
	bytesRead := 0

	for bytesRead < maxRead-3 { // Read 4 bytes at a time
		entryIndex := (off + int64(bytesRead)) / 4

		if entryIndex >= int64(len(f.entries)) {
			break
		}

		value := f.entries[entryIndex] & 0x0FFFFFFF
		binary.LittleEndian.PutUint32(p[bytesRead:], value)
		bytesRead += 4
	}

	return bytesRead, nil
}

func (f *FATTableRegion) writeFAT32(p []byte, off int64, maxWrite int) (n int, err error) {
	bytesWritten := 0

	for bytesWritten < maxWrite-3 { // Write 4 bytes at a time
		entryIndex := (off + int64(bytesWritten)) / 4

		if entryIndex >= int64(len(f.entries)) {
			break
		}

		value := binary.LittleEndian.Uint32(p[bytesWritten:])
		f.entries[entryIndex] = value & 0x0FFFFFFF // Mask to 28 bits
		bytesWritten += 4
	}

	return bytesWritten, nil
}
