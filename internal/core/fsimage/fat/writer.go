package fat

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/common"
	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

const FAT_DEBUG = false

// BootloaderConfig contains bootloader configuration for FAT filesystem
type BootloaderConfig struct {
	Type      string            // Bootloader type (syslinux, systemd-boot)
	Config    string            // Bootloader configuration content
	CoreFiles map[string][]byte // Core bootloader files (e.g., ldlinux.sys)
	BootCode  []byte            // Boot code to inject into boot sector
}

// PartitionInfo contains partition-specific information for sector calculations
type PartitionInfo struct {
	StartLBA uint64 // Absolute disk sector where partition starts
}

// DeterministicConfig provides options for deterministic filesystem generation
type DeterministicConfig struct {
	// FixedTimestamp sets a fixed timestamp for all directory entries.
	// If nil, time.Now() will be used (non-deterministic).
	// For deterministic builds, set to a fixed time like Unix epoch.
	FixedTimestamp *time.Time

	// SortDirectoryEntries ensures directory entries are written in lexicographic order.
	// This ensures consistent directory layout across multiple generations.
	SortDirectoryEntries bool

	// VolumeSerial sets a specific volume serial number.
	// If nil, a default hardcoded value will be used.
	VolumeSerial *uint32

	// Bootloader configuration for making filesystem bootable
	Bootloader *BootloaderConfig

	// Partition information for absolute sector calculations
	Partition *PartitionInfo
}

// DefaultDeterministicConfig returns a configuration for fully deterministic filesystem generation
func DefaultDeterministicConfig() *DeterministicConfig {
	// Use FAT epoch (January 1, 1980) as the fixed timestamp
	fatEpoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	volumeSerial := uint32(0x12345678) // Default deterministic serial
	return &DeterministicConfig{
		FixedTimestamp:       &fatEpoch,
		SortDirectoryEntries: true,
		VolumeSerial:         &volumeSerial,
	}
}

// FATWritableNode implements both Node and WritableNode interfaces using generated structures
type FATWritableNode struct {
	id       string
	name     string
	size     int64
	isDir    bool
	cluster  uint32
	children []*FATWritableNode
	parent   *FATWritableNode
	pending  vm.MemoryRegion
}

// Node interface implementation
func (n *FATWritableNode) Name() string {
	return n.name
}

func (n *FATWritableNode) Size() int64 {
	return n.size
}

func (n *FATWritableNode) IsDir() bool {
	return n.isDir
}

func (n *FATWritableNode) SetDir() {
	n.isDir = true
}

// WritableNode interface implementation
func (n *FATWritableNode) SetName(name string) error {
	// Validate FAT filename constraints
	if len(name) == 0 {
		return fmt.Errorf("filename cannot be empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("filename too long")
	}

	// Check for invalid characters
	invalidChars := `"*+,/:;<=>?[\]|`
	for _, char := range invalidChars {
		if strings.ContainsRune(name, char) {
			return fmt.Errorf("invalid character in filename: %c", char)
		}
	}

	n.name = name
	return nil
}

type mapped struct {
	offset int64
	size   int64
}

func (m mapped) overlaps(other mapped) bool {
	return m.offset < other.offset+other.size && other.offset < m.offset+m.size
}

// FATWriter extends FATLayout to implement FileSystemWriter interface
type FATWriter struct {
	layout      *FATLayout
	fatTable    *FATTableRegion
	nodes       map[string]*FATWritableNode
	nodeCounter uint32
	// Track short names per directory to detect collisions
	shortNames map[string]map[string]bool // directoryPath -> shortName -> exists
	// Configuration for deterministic generation
	deterministicConfig *DeterministicConfig
	mappedRegions       []mapped // Track mapped regions to avoid overlaps
	// SYSLINUX VBR patching support
	ldlinuxSysCluster uint32 // First cluster where ldlinux.sys is stored
}

// Compile-time interface assertions
var (
	_ common.Node             = (*FATWritableNode)(nil)
	_ common.WritableNode     = (*FATWritableNode)(nil)
	_ common.FileSystemWriter = (*FATWriter)(nil)
)

// CreateFATFileSystem creates a new FAT filesystem using generated structures
func CreateFATFileSystem(storage vm.VirtualStorage, size int64) (*FATWriter, error) {
	return CreateFATFileSystemWithConfig(storage, size, nil)
}

// CreateFATFileSystemWithConfig creates a new FAT filesystem with deterministic configuration
func CreateFATFileSystemWithConfig(storage vm.VirtualStorage, size int64, config *DeterministicConfig) (*FATWriter, error) {
	return CreateFATFileSystemWithTypeAndConfig(storage, size, "", config)
}

// CreateFATFileSystemWithTypeAndConfig creates a new FAT filesystem with specific FAT type and deterministic configuration
func CreateFATFileSystemWithTypeAndConfig(storage vm.VirtualStorage, size int64, fatType string, config *DeterministicConfig) (*FATWriter, error) {
	// Create layout first
	layout := NewFATLayout(storage)

	// Initialize filesystem structure using generated methods
	if err := initializeFATStructure(layout, size, fatType, config); err != nil {
		return nil, fmt.Errorf("failed to initialize FAT structure: %w", err)
	}

	// Calculate total clusters for FAT table initialization
	fs := layout.Fs()
	totalClusters := fs.TotalDataClusters() + 2 // Add reserved clusters 0 and 1

	// Create custom FAT table region
	fatTable := NewFATTableRegion(fs.FatType(), totalClusters)

	// Create writer
	writer := &FATWriter{
		layout:              layout,
		fatTable:            fatTable,
		nodes:               make(map[string]*FATWritableNode),
		nodeCounter:         0,
		shortNames:          make(map[string]map[string]bool),
		deterministicConfig: config,
	}

	// Initialize FAT tables using custom region
	if err := writer.initializeFATTables(); err != nil {
		return nil, fmt.Errorf("failed to initialize FAT tables: %w", err)
	}

	return writer, nil
}

func (w *FATWriter) mapRegion(region vm.MemoryRegion, offset int64) error {
	// Check for any overlapping regions
	if FAT_DEBUG {
		new := mapped{offset: offset, size: region.Size()}
		for _, existing := range w.mappedRegions {
			if existing.overlaps(new) {
				return fmt.Errorf("region overlaps with existing mapping at offset %d", existing.offset)
			}
		}
		w.mappedRegions = append(w.mappedRegions, new)
	}

	// slog.Info("Mapping region to FAT filesystem",
	// 	"offset", offset,
	// 	"size", region.Size(),
	// )
	return w.layout.storage.Map(region, offset)
}

// mapRegionSlice maps a slice of a memory region directly without creating intermediate objects
func (w *FATWriter) mapRegionSlice(region vm.MemoryRegion, srcOffset, size, dstOffset int64) error {
	// Check for any overlapping regions
	if FAT_DEBUG {
		new := mapped{offset: dstOffset, size: size}
		for _, existing := range w.mappedRegions {
			if existing.overlaps(new) {
				return fmt.Errorf("region overlaps with existing mapping at offset %d", existing.offset)
			}
		}
		w.mappedRegions = append(w.mappedRegions, new)
	}

	// Map the slice directly to the storage using the VM's MapSlice method
	return w.layout.storage.MapSlice(region, srcOffset, size, dstOffset)
}

// mapClustersBatch efficiently maps data to multiple clusters by batching consecutive cluster operations
func (w *FATWriter) mapClustersBatch(data vm.MemoryRegion, clusters []uint32, clusterSize, dataSize int64) error {
	var dataOffset int64 = 0

	for i := 0; i < len(clusters); {
		// Find consecutive clusters to batch together
		batchStart := i
		expectedCluster := clusters[i]

		for i < len(clusters) && clusters[i] == expectedCluster {
			i++
			expectedCluster++
		}

		batchLength := i - batchStart
		batchClusters := clusters[batchStart:i]

		// Calculate batch mapping parameters
		batchDataSize := int64(batchLength) * clusterSize
		if dataOffset+batchDataSize > dataSize {
			batchDataSize = dataSize - dataOffset
		}

		if batchLength == 1 {
			// Single cluster - use regular mapping
			cluster := batchClusters[0]
			clusterOffset := int64(w.layout.Cluster(cluster))
			mapSize := clusterSize
			if dataOffset+mapSize > dataSize {
				mapSize = dataSize - dataOffset
			}

			if err := w.mapRegionSlice(data, dataOffset, mapSize, clusterOffset); err != nil {
				return fmt.Errorf("failed to map data to cluster %d: %w", cluster, err)
			}
		} else {
			// Multiple consecutive clusters - batch map them
			firstClusterOffset := int64(w.layout.Cluster(batchClusters[0]))

			if err := w.mapRegionSlice(data, dataOffset, batchDataSize, firstClusterOffset); err != nil {
				return fmt.Errorf("failed to batch map data to clusters %v: %w", batchClusters, err)
			}
		}

		dataOffset += batchDataSize
		if dataOffset >= dataSize {
			break
		}
	}

	return nil
}

func (w *FATWriter) reinterpret(region vm.MemoryRegion, offset int64) error {
	// Check for any overlapping regions
	if FAT_DEBUG {
		new := mapped{offset: offset, size: region.Size()}
		for _, existing := range w.mappedRegions {
			if existing.overlaps(new) {
				return fmt.Errorf("region overlaps with existing mapping at offset %d", existing.offset)
			}
		}
		w.mappedRegions = append(w.mappedRegions, new)
	}
	// slog.Info("Reinterpreting region in FAT filesystem",
	// 	"offset", offset,
	// 	"size", region.Size(),
	// )
	return w.layout.storage.Reinterpret(region, offset)
}

// getTimestamp returns the appropriate timestamp based on configuration
func (w *FATWriter) getTimestamp() time.Time {
	if w.deterministicConfig != nil && w.deterministicConfig.FixedTimestamp != nil {
		return *w.deterministicConfig.FixedTimestamp
	}
	return time.Now()
}

// validateFATTypeForSize checks if the requested FAT type is suitable for the given size
func validateFATTypeForSize(fatType string, size int64) error {
	const (
		fat12MinSize = 1024                   // 1KB minimum
		fat12MaxSize = 16 * 1024 * 1024       // 16MB practical maximum for FAT12
		fat16MinSize = 1024 * 1024            // 1MB minimum
		fat16MaxSize = 2 * 1024 * 1024 * 1024 // 2GB maximum for FAT16
		fat32MinSize = 33 * 1024 * 1024       // 33MB minimum for FAT32
	)

	switch fatType {
	case "FAT12":
		if size < fat12MinSize {
			return fmt.Errorf("size %d too small for FAT12 (minimum %d)", size, fat12MinSize)
		}
		if size > fat12MaxSize {
			return fmt.Errorf("size %d too large for FAT12 (maximum %d), consider FAT16 or FAT32", size, fat12MaxSize)
		}
	case "FAT16":
		if size < fat16MinSize {
			return fmt.Errorf("size %d too small for FAT16 (minimum %d)", size, fat16MinSize)
		}
		if size > fat16MaxSize {
			return fmt.Errorf("size %d too large for FAT16 (maximum %d), consider FAT32", size, fat16MaxSize)
		}
	case "FAT32":
		if size < fat32MinSize {
			return fmt.Errorf("size %d too small for FAT32 (minimum %d), consider FAT16", size, fat32MinSize)
		}
		// FAT32 can handle very large sizes, so no upper limit check needed
	default:
		return fmt.Errorf("unknown FAT type: %s", fatType)
	}

	return nil
}

// initializeFATStructure sets up the boot sector using generated setter methods
func initializeFATStructure(layout *FATLayout, size int64, fatType string, config *DeterministicConfig) error {
	// Get filesystem structure for modification
	fs := layout.Fs()
	if fs == nil {
		return fmt.Errorf("failed to get filesystem structure")
	}

	// Calculate filesystem parameters
	const sectorSize = 512
	totalSectors := uint32(size / sectorSize)

	// Determine FAT type and parameters
	var fsType string
	var sectorsPerCluster uint8
	var reservedSectors uint16

	const (
		fat12Limit = 4 * 1024 * 1024   // 4MB
		fat16Limit = 512 * 1024 * 1024 // 512MB
	)

	// Use specified FAT type if provided, otherwise determine based on size
	if fatType != "" && fatType != "fat" {
		// Explicit FAT type specified in YAML (fat12, fat16, fat32)
		switch fatType {
		case "fat12":
			fsType = "FAT12"
			sectorsPerCluster = 1 // 512 bytes per cluster
			reservedSectors = 1
		case "fat16":
			fsType = "FAT16"
			if size <= 128*1024*1024 { // <= 128MB
				sectorsPerCluster = 4 // 2KB per cluster
			} else {
				sectorsPerCluster = 8 // 4KB per cluster
			}
			reservedSectors = 1
		case "fat32":
			fsType = "FAT32"
			// Use TinyRange-compatible parameters for bootable images
			sectorsPerCluster = 1 // 512 bytes per cluster (matches TinyRange)
			reservedSectors = 32  // 32 reserved sectors (matches TinyRange)
		default:
			return fmt.Errorf("unsupported FAT type: %s", fatType)
		}

		// Validate that the requested FAT type is feasible for the given size
		if err := validateFATTypeForSize(fsType, size); err != nil {
			return fmt.Errorf("FAT type %s not suitable for size %d: %w", fatType, size, err)
		}
	} else {
		// Fall back to size-based detection for generic "fat" or unspecified type
		if size < fat12Limit {
			fsType = "FAT12"
			sectorsPerCluster = 1 // 512 bytes per cluster
			reservedSectors = 1
		} else if size < fat16Limit {
			fsType = "FAT16"
			if size <= 128*1024*1024 { // <= 128MB
				sectorsPerCluster = 4 // 2KB per cluster
			} else {
				sectorsPerCluster = 8 // 4KB per cluster
			}
			reservedSectors = 1
		} else {
			fsType = "FAT32"
			if size <= 8*1024*1024*1024 { // <= 8GB
				sectorsPerCluster = 8 // 4KB per cluster
			} else {
				sectorsPerCluster = 16 // 8KB per cluster
			}
			reservedSectors = 32 // FAT32 uses more reserved sectors
		}
	}

	// Set boot jump instruction and OEM identifier using generated setters
	fs.SetBootJump([3]byte{0xEB, 0x3C, 0x90})
	fs.SetOemIdentifier([8]byte{'M', 'S', 'W', 'I', 'N', '4', '.', '1'})

	// Set basic BPB fields using generated setter methods
	fs.SetBytesPerSector(sectorSize)
	fs.SetSectorsPerCluster(sectorsPerCluster)
	fs.SetReservedSectors(reservedSectors)
	fs.SetFileAllocationTables(2)   // Standard to have 2 FAT copies
	fs.SetMediaDescriptorType(0xF8) // Fixed disk
	fs.SetSectorsPerTrack(63)
	fs.SetSectorsPerHead(16)
	fs.SetHiddenSectors(0)

	// Calculate and set root directory entries and total sectors
	rootDirEntries := uint16(512) // Standard for FAT12/16
	if fsType == "FAT32" {
		rootDirEntries = 0 // FAT32 root directory is stored in clusters
	}
	fs.SetRootDirectoryEntries(rootDirEntries)

	// Set total sectors
	if fsType != "FAT32" && totalSectors < 65536 {
		fs.SetTotalSectors(uint16(totalSectors))
		fs.SetLargeSectorCount(0)
	} else {
		fs.SetTotalSectors(0)
		fs.SetLargeSectorCount(totalSectors)
	}

	// Calculate and set FAT size
	rootDirSectors := uint32(0)
	if fsType != "FAT32" {
		rootDirSectors = ((uint32(rootDirEntries) * 32) + (sectorSize - 1)) / sectorSize
	}

	dataSectors := totalSectors - uint32(reservedSectors) - rootDirSectors
	totalClusters := dataSectors / uint32(sectorsPerCluster)

	var sectorsPerFAT uint32
	switch fsType {
	case "FAT12":
		sectorsPerFAT = ((totalClusters * 3 / 2) + sectorSize - 1) / sectorSize
		fs.SetSectorsPerFat16(uint16(sectorsPerFAT))
	case "FAT16":
		sectorsPerFAT = ((totalClusters * 2) + sectorSize - 1) / sectorSize
		fs.SetSectorsPerFat16(uint16(sectorsPerFAT))
	case "FAT32":
		sectorsPerFAT = ((totalClusters * 4) + sectorSize - 1) / sectorSize
		fs.SetSectorsPerFat16(0) // FAT32 uses sectorsPerFat32
		fs.SetSectorsPerFat32(sectorsPerFAT)
		fs.SetRootDirectoryCluster(2) // Root directory starts at cluster 2
		fs.SetFsInfoSector(1)
		fs.SetBackupBootSector(6)
		// FAT32 flags: set bit 7 to indicate that only one FAT is active (optional)
		fs.SetFlags(0x0000)      // Mirror FAT enabled, use both FATs
		fs.SetFatVersion(0x0000) // FAT32 version 0.0
	}

	// Set remaining fields
	fs.SetDriveNumber(0x80) // Hard disk
	fs.SetNtFlags(0)
	fs.SetSignature(0x29) // Extended boot signature

	// Set volume serial number
	volumeSerial := uint32(0x12345678) // Default hardcoded value
	if config != nil && config.VolumeSerial != nil {
		volumeSerial = *config.VolumeSerial
	}
	fs.SetVolumeId(volumeSerial)
	fs.SetVolumeLabel("NO NAME    ")

	switch fsType {
	case "FAT32":
		fs.SetSystemIdentifier("FAT32   ")
		if err := writeFATString(layout.storage, 71, 11, "NO NAME    "); err != nil {
			return err
		}
		if err := writeFATString(layout.storage, 82, 8, "FAT32   "); err != nil {
			return err
		}
	case "FAT16":
		fs.SetSystemIdentifier("FAT16   ")
		if err := writeFATString(layout.storage, 43, 11, "NO NAME    "); err != nil {
			return err
		}
		if err := writeFATString(layout.storage, 54, 8, "FAT16   "); err != nil {
			return err
		}
	default:
		fs.SetSystemIdentifier("FAT12   ")
		if err := writeFATString(layout.storage, 43, 11, "NO NAME    "); err != nil {
			return err
		}
		if err := writeFATString(layout.storage, 54, 8, "FAT12   "); err != nil {
			return err
		}
	}

	// Set boot signature
	fs.SetBootSignature(0xAA55)
	if _, err := layout.storage.WriteAt([]byte{0x55, 0xAA}, 510); err != nil {
		return fmt.Errorf("failed to write FAT boot sector trailer: %w", err)
	}

	return nil
}

func writeFATString(storage vm.VirtualStorage, offset int64, size int, value string) error {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = ' '
	}
	copy(buf, []byte(value))
	if _, err := storage.WriteAt(buf, offset); err != nil {
		return fmt.Errorf("failed to write FAT string at offset %d: %w", offset, err)
	}
	return nil
}

// initializeFATTables sets up the initial FAT tables using custom region
func (w *FATWriter) initializeFATTables() error {
	fs := w.layout.Fs()

	// Mark first two entries as reserved using custom FAT table
	if fs.FatType() == "FAT12" {
		w.fatTable.SetEntry(0, 0xFF8) // Media descriptor in first entry
		w.fatTable.SetEntry(1, FAT12_EOC)
	} else if fs.FatType() == "FAT16" {
		w.fatTable.SetEntry(0, 0xFFF8) // Media descriptor in first entry
		w.fatTable.SetEntry(1, FAT16_EOC)
	} else if fs.FatType() == "FAT32" {
		w.fatTable.SetEntry(0, 0x0FFFFFF8) // Media descriptor in first entry
		w.fatTable.SetEntry(1, FAT32_EOC)
		// For FAT32, also mark root directory cluster as end of chain
		w.fatTable.SetEntry(2, FAT32_EOC)
	}

	// Map the custom FAT table region to both primary and backup locations
	primaryFATOffset := int64(fs.ReservedSectors()) * int64(fs.BytesPerSector())
	fatSize := int64(fs.SectorsPerFat()) * int64(fs.BytesPerSector())
	backupFATOffset := primaryFATOffset + fatSize

	// Map to primary FAT location
	if err := w.mapRegion(w.fatTable, primaryFATOffset); err != nil {
		return fmt.Errorf("failed to map primary FAT table: %w", err)
	}

	// Map the same region to backup FAT location
	if err := w.mapRegion(w.fatTable, backupFATOffset); err != nil {
		return fmt.Errorf("failed to map backup FAT table: %w", err)
	}

	return nil
}

// initializeFAT32Structures sets up FAT32-specific sectors (FSInfo, backup boot sector)
func (w *FATWriter) initializeFAT32Structures() error {
	// 1. Initialize FSInfo sector
	if err := w.initializeFSInfoSector(); err != nil {
		return fmt.Errorf("failed to initialize FSInfo sector: %w", err)
	}

	// 2. Create backup boot sector (copy of primary boot sector)
	if err := w.createBackupBootSector(); err != nil {
		return fmt.Errorf("failed to create backup boot sector: %w", err)
	}

	return nil
}

// initializeFSInfoSector creates and initializes the FSInfo sector for FAT32
func (w *FATWriter) initializeFSInfoSector() error {
	fs := w.layout.Fs()

	// Calculate FSInfo sector offset
	fsInfoOffset := int64(fs.FsInfoSector()) * int64(fs.BytesPerSector())

	// Create a new FSInfo structure with proper initialization
	fsInfo := NewFSInfo()

	// The FSInfo structure from the generator should have correct signatures,
	// but let's make sure they're set properly
	fsInfo.SetLeadSignature(0x41615252)  // "RRaA"
	fsInfo.SetSignature(0x61417272)      // "rrAa"
	fsInfo.SetTrailSignature(0xAA550000) // Trail signature

	// Calculate actual free cluster count from FAT table
	// This is called during Finalize(), so all files have been written and clusters allocated
	actualFreeClusters := w.fatTable.GetFreeClusterCount()

	// Set free cluster information
	fsInfo.SetLastFreeClusterCount(actualFreeClusters)

	// Set the next available cluster hint to the current nextFree hint from FAT table
	// This helps optimization when mounting the filesystem later
	// Ensure it's at least 2 (valid data cluster)
	fsInfo.SetAvailableClusterStart(max(w.fatTable.nextFree, 2))

	// Map the FSInfo structure to the filesystem at the correct offset
	if err := w.mapRegion(fsInfo, fsInfoOffset); err != nil {
		return fmt.Errorf("failed to map FSInfo sector: %w", err)
	}

	return nil
}

// createBackupBootSector maps the primary boot sector to the backup location
func (w *FATWriter) createBackupBootSector() error {
	fs := w.layout.Fs()

	// Get the filesystem structure which is mapped to offset 0 (the primary boot sector)
	// The filesystem structure itself is the boot sector data
	bootSectorRegion := fs

	// Create a truncated region that limits the size to exactly one sector
	sectorSize := int64(fs.BytesPerSector())
	truncatedBootSector := &vm.TruncatedRegion{
		Region:  bootSectorRegion,
		MaxSize: sectorSize,
	}

	// Calculate backup boot sector offset
	backupOffset := int64(fs.BackupBootSector()) * sectorSize

	// Map the same boot sector region to the backup location
	if err := w.mapRegion(truncatedBootSector, backupOffset); err != nil {
		return fmt.Errorf("failed to map backup boot sector: %w", err)
	}

	return nil
}

// allocateCluster allocates and returns the next available cluster
func (w *FATWriter) allocateCluster() (uint32, error) {
	return w.fatTable.AllocateCluster()
}

// FileSystemWriter interface implementation

// AllocateNode allocates a new node for writing
func (w *FATWriter) AllocateNode() (common.WritableNode, error) {
	w.nodeCounter++
	id := fmt.Sprintf("node_%d", w.nodeCounter)

	node := &FATWritableNode{
		id:       id,
		name:     "",
		size:     0,
		isDir:    false,
		cluster:  0,
		children: make([]*FATWritableNode, 0),
		parent:   nil,
	}

	w.nodes[id] = node
	return node, nil
}

// WriteContents writes data to the node using layout system
func (w *FATWriter) WriteContents(node common.WritableNode, data vm.MemoryRegion) error {
	fatNode, ok := node.(*FATWritableNode)
	if !ok {
		return fmt.Errorf("node is not a FATWritableNode")
	}

	if fatNode.isDir {
		return fmt.Errorf("cannot write contents to directory")
	}

	dataSize := data.Size()
	if dataSize == 0 {
		// Empty file
		fatNode.size = 0
		fatNode.cluster = 0
		fatNode.pending = nil
		return nil
	}

	if w.shouldDeferContentAllocation() {
		fatNode.size = dataSize
		fatNode.pending = data
		return nil
	}

	return w.writeFileContents(fatNode, data)
}

func (w *FATWriter) shouldDeferContentAllocation() bool {
	return w.deterministicConfig != nil && w.deterministicConfig.SortDirectoryEntries
}

func (w *FATWriter) writeFileContents(fatNode *FATWritableNode, data vm.MemoryRegion) error {
	dataSize := data.Size()
	if dataSize == 0 {
		fatNode.size = 0
		fatNode.cluster = 0
		fatNode.pending = nil
		return nil
	}

	// Calculate number of clusters needed using layout system
	clusterSize := int64(w.layout.ClusterSize())
	clustersNeeded := (dataSize + clusterSize - 1) / clusterSize

	// Allocate clusters in batch and set up chain automatically
	firstCluster, err := w.allocateMultipleClusters(uint32(clustersNeeded))
	if err != nil {
		return fmt.Errorf("failed to allocate clusters: %w", err)
	}

	// Get the full cluster chain for mapping
	clusters := w.fatTable.GetClusterChain(firstCluster)

	// Map data directly into clusters using batch mapping for better performance
	if err := w.mapClustersBatch(data, clusters, clusterSize, dataSize); err != nil {
		return fmt.Errorf("failed to map data to clusters: %w", err)
	}

	// Update node
	fatNode.size = dataSize
	fatNode.cluster = firstCluster
	fatNode.pending = nil

	return nil
}

// WriteDirectory writes directory contents
func (w *FATWriter) WriteDirectory(node common.WritableNode, children []common.WritableNode) error {
	fatNode, ok := node.(*FATWritableNode)
	if !ok {
		return fmt.Errorf("node is not a FATWritableNode")
	}

	if !fatNode.isDir {
		return fmt.Errorf("node is not a directory")
	}

	// Allocate cluster(s) for directory if needed (except for root directory)
	if fatNode.cluster == 0 && fatNode.name != "/" {
		// Calculate how many clusters we need for this directory
		// Each cluster can hold (clusterSize / 32) directory entries
		clusterSize := int64(w.layout.ClusterSize())
		entriesPerCluster := clusterSize / 32

		// Count total entries needed: "." + ".." + children
		totalEntries := int64(len(children))
		if fatNode.name != "/" {
			totalEntries += 2 // for "." and ".." entries
		}

		clustersNeeded := (totalEntries + entriesPerCluster - 1) / entriesPerCluster
		if clustersNeeded == 0 {
			clustersNeeded = 1 // At least one cluster
		}

		// Allocate the required clusters in batch and set up chain automatically
		firstCluster, err := w.allocateMultipleClusters(uint32(clustersNeeded))
		if err != nil {
			return fmt.Errorf("failed to allocate clusters for directory: %w", err)
		}

		// Set the first cluster as the directory's cluster
		fatNode.cluster = firstCluster
	}

	// Convert children to FATWritableNodes
	fatChildren := make([]*FATWritableNode, len(children))
	for i, child := range children {
		fatChild, ok := child.(*FATWritableNode)
		if !ok {
			return fmt.Errorf("child node is not a FATWritableNode")
		}
		fatChild.parent = fatNode
		fatChildren[i] = fatChild
	}

	fatNode.children = fatChildren

	// Directory contents will be written during Finalize()
	return nil
}

// WritableRootDirectory returns the writable root directory node
func (w *FATWriter) WritableRootDirectory() (common.WritableNode, error) {
	// Check if root directory already exists
	rootID := "root"
	if root, exists := w.nodes[rootID]; exists {
		return root, nil
	}

	// Create root directory node
	root := &FATWritableNode{
		id:       rootID,
		name:     "/",
		size:     0,
		isDir:    true,
		cluster:  0, // Special handling for root
		children: make([]*FATWritableNode, 0),
		parent:   nil,
	}

	// For FAT32, root directory uses cluster 2
	fs := w.layout.Fs()
	if fs.FatType() == "FAT32" {
		root.cluster = 2
	}

	w.nodes[rootID] = root
	return root, nil
}

// Finalize finalizes the file system metadata
func (w *FATWriter) Finalize() error {
	// Get root directory
	rootNode, exists := w.nodes["root"]
	if !exists {
		return fmt.Errorf("root directory not found")
	}

	// Process bootloader configuration if provided
	if w.deterministicConfig != nil && w.deterministicConfig.Bootloader != nil {
		if err := w.processBootloader(); err != nil {
			return fmt.Errorf("failed to process bootloader: %w", err)
		}
	}

	if err := w.allocatePendingContents(rootNode); err != nil {
		return fmt.Errorf("failed to allocate file contents: %w", err)
	}

	// Write directory structures using generated structures
	if err := w.writeDirectoryStructure(rootNode); err != nil {
		return fmt.Errorf("failed to write directory structure: %w", err)
	}

	// Initialize FAT32-specific structures after everything else is complete
	fs := w.layout.Fs()
	if fs.FatType() == "FAT32" {
		if err := w.initializeFAT32Structures(); err != nil {
			return fmt.Errorf("failed to initialize FAT32 structures: %w", err)
		}
	}

	// Inject bootloader boot code after filesystem structures are complete
	if w.deterministicConfig != nil && w.deterministicConfig.Bootloader != nil && len(w.deterministicConfig.Bootloader.BootCode) > 0 {
		if err := w.injectBootCode(); err != nil {
			return fmt.Errorf("failed to inject boot code: %w", err)
		}
	}

	return nil
}

func (w *FATWriter) allocatePendingContents(node *FATWritableNode) error {
	children := w.sortedChildren(node)
	for _, child := range children {
		if child.isDir {
			if err := w.allocatePendingContents(child); err != nil {
				return err
			}
			continue
		}
		if child.pending == nil {
			continue
		}
		if err := w.writeFileContents(child, child.pending); err != nil {
			return fmt.Errorf("write deferred contents for %s: %w", child.name, err)
		}
	}
	return nil
}

// writeDirectoryStructure recursively writes directory structures using generated DirectoryEntry
func (w *FATWriter) writeDirectoryStructure(node *FATWritableNode) error {
	if !node.isDir {
		return fmt.Errorf("node is not a directory")
	}

	// Calculate directory location for mapping entries directly
	fs := w.layout.Fs()
	var directoryOffset int64

	if node.name == "/" && fs.FatType() != "FAT32" {
		// FAT12/16 root directory is in a fixed location
		directoryOffset = int64(uint32(fs.ReservedSectors())*uint32(fs.BytesPerSector())) +
			int64(uint32(fs.FileAllocationTables())*fs.SectorsPerFat()*uint32(fs.BytesPerSector()))
	} else {
		// Regular directory or FAT32 root directory
		if node.cluster == 0 {
			return fmt.Errorf("directory %s has no cluster allocated", node.name)
		}
		directoryOffset = int64(w.layout.Cluster(node.cluster))
	}

	// Get the full cluster chain for this directory
	clusterChain := w.fatTable.GetClusterChain(node.cluster)

	// Initialize multi-cluster directory writer
	clusterSize := int64(w.layout.ClusterSize())
	currentClusterIndex := 0
	var currentCluster uint32
	var currentOffset int64
	var clusterStartOffset int64

	// Handle root directory special case (cluster 0, empty chain)
	if node.name == "/" && len(clusterChain) == 0 {
		// For root directory, use the calculated directoryOffset directly
		currentOffset = directoryOffset
		clusterStartOffset = directoryOffset
		// For root directory, we don't use cluster-based calculations
	} else if len(clusterChain) > 0 {
		// For regular directories, use cluster chain
		currentCluster = clusterChain[0]
		currentOffset = int64(w.layout.Cluster(currentCluster))
		clusterStartOffset = currentOffset
	} else {
		return fmt.Errorf("directory '%s' has no cluster chain and is not root", node.name)
	}

	// Helper function to get next available offset, handling cluster boundaries
	getNextOffset := func() int64 {
		// For root directory, check if we have enough space
		if node.name == "/" && len(clusterChain) == 0 {
			// For root directory in FAT12/16, we have a fixed size
			// For now, just return current offset without incrementing
			// (createMappedDirectoryEntry will return the updated offset)
			return currentOffset
		}

		// For regular directories, check if we need to move to next cluster
		if currentOffset-clusterStartOffset >= clusterSize {
			currentClusterIndex++
			if currentClusterIndex >= len(clusterChain) {
				panic(fmt.Sprintf("Directory '%s' ran out of allocated clusters", node.name))
			}
			currentCluster = clusterChain[currentClusterIndex]
			currentOffset = int64(w.layout.Cluster(currentCluster))
			clusterStartOffset = currentOffset
		}

		// Return current offset without incrementing
		// (createMappedDirectoryEntry will return the next available offset)
		return currentOffset
	}

	// Add volume label entry for root directory
	if node.name == "/" {
		currentOffset = w.createMappedVolumeLabelEntry(getNextOffset())
	}

	// CRITICAL: FAT specification requires "." and ".." entries to be the first
	// two entries in any subdirectory (not in root directory).
	//
	// This ordering is essential for:
	// 1. Compatibility with all FAT drivers (Windows, Linux, etc.)
	// 2. Proper directory traversal and parent navigation
	// 3. File system integrity checks (fsck.fat, chkdsk, etc.)
	//
	// The "." entry must point to the directory's own cluster.
	// The ".." entry must point to the parent directory's cluster (0 for root in FAT12/16).

	// Add "." entry (current directory) - MUST be first entry in subdirectories
	if node.name != "/" {
		currentOffset = w.createMappedDirectoryEntry(getNextOffset(), ".", node.cluster, 0, true)
	}

	// Add ".." entry (parent directory) - MUST be second entry in subdirectories
	if node.name != "/" && node.parent != nil {
		parentCluster := node.parent.cluster
		if node.parent.name == "/" {
			// For directories immediately under root, '..' entry should always point to cluster 0
			// This is true for FAT12, FAT16, and FAT32
			parentCluster = 0
		}
		currentOffset = w.createMappedDirectoryEntry(getNextOffset(), "..", parentCluster, 0, true)
	}

	// Add child entries
	children := w.sortedChildren(node)

	for _, child := range children {
		// Get next available offset, handling cluster boundaries
		nextOffset := getNextOffset()
		currentOffset = w.createMappedDirectoryEntry(nextOffset, child.name, child.cluster, child.size, child.isDir)

		// Recursively write child directories
		if child.isDir {
			if err := w.writeDirectoryStructure(child); err != nil {
				return err
			}
		}
	}

	return nil
}

func (w *FATWriter) sortedChildren(node *FATWritableNode) []*FATWritableNode {
	children := node.children
	if w.deterministicConfig == nil || !w.deterministicConfig.SortDirectoryEntries {
		return children
	}
	children = make([]*FATWritableNode, len(node.children))
	copy(children, node.children)
	sort.Slice(children, func(i, j int) bool {
		return children[i].name < children[j].name
	})
	return children
}

// encodeFATTime encodes a time into FAT format
func encodeFATTime(t time.Time) uint16 {
	return uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
}

// encodeFATDate encodes a date into FAT format
func encodeFATDate(t time.Time) uint16 {
	return uint16(t.Year()-1980)<<9 | uint16(t.Month())<<5 | uint16(t.Day())
}

// createMappedVolumeLabelEntry creates and maps a volume label entry at the specified offset
func (w *FATWriter) createMappedVolumeLabelEntry(offset int64) int64 {
	var entry DirectoryEntry
	if err := w.reinterpret(&entry, offset); err != nil {
		// This should not happen in normal operation
		panic(fmt.Sprintf("failed to map volume label entry at offset %d: %v", offset, err))
	}

	// Get volume label from filesystem structure
	fs := w.layout.Fs()
	volumeLabel := fs.VolumeLabel()

	// Set the volume label name (11 bytes total, space padded)
	for i := range 11 {
		if i < len(volumeLabel) {
			entry[i] = volumeLabel[i]
		} else {
			entry[i] = ' '
		}
	}

	// Set volume ID attribute
	entry.SetAttributes(ATTR_VOLUME_ID)

	// Set time fields using configured time
	now := w.getTimestamp()
	creationTime := encodeFATTime(now)
	creationDate := encodeFATDate(now)

	entry.SetCreationTime(creationTime)
	entry.SetCreationDate(creationDate)
	entry.SetLastAccessDate(creationDate)
	entry.SetLastModificationTime(creationTime)
	entry.SetLastModificationDate(creationDate)

	// Volume label entries have no cluster or size
	entry.SetFirstClusterHigh(0)
	entry.SetFirstClusterLow(0)
	entry.SetFileSize(0)

	return offset + int64(entry.Size())
}

// ShortName represents a FAT 8.3 short name
type ShortName struct {
	Name      [8]byte
	Extension [3]byte
}

// generateShortName creates a valid FAT 8.3 short name from a long filename
func (w *FATWriter) generateShortName(longName string, isDir bool) ShortName {
	// Handle special directory entries
	if longName == "." {
		return ShortName{
			Name:      [8]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' '},
			Extension: [3]byte{' ', ' ', ' '},
		}
	}
	if longName == ".." {
		return ShortName{
			Name:      [8]byte{'.', '.', ' ', ' ', ' ', ' ', ' ', ' '},
			Extension: [3]byte{' ', ' ', ' '},
		}
	}

	// Clean and validate the long name
	cleanName := w.cleanLongName(longName)

	// Split into base name and extension
	baseName, extension := w.splitNameAndExtension(cleanName)

	// Generate initial short name
	shortName := w.createInitialShortName(baseName, extension)

	// Check for collisions and generate numeric tail if needed
	// For now, use a simple directory tracking approach
	finalShortName := w.resolveShortNameCollisions(shortName, longName, "/")

	return finalShortName
}

// cleanLongName removes invalid characters and normalizes the name
func (w *FATWriter) cleanLongName(name string) string {
	// Convert to uppercase for short name generation
	name = strings.ToUpper(name)

	// Remove leading dots and spaces, but keep trailing spaces for now
	name = strings.TrimLeft(name, ". ")
	name = strings.TrimRight(name, " ")

	// Replace invalid characters with underscores
	invalidChars := `"*+,/:;<=>?[\]|`
	for _, char := range invalidChars {
		name = strings.ReplaceAll(name, string(char), "_")
	}

	// Replace spaces with underscores (spaces not allowed in short names)
	name = strings.ReplaceAll(name, " ", "_")

	// For multiple dots, replace internal dots (except the last one) with underscores
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		// Join all but the last part with underscores, then add the extension
		baseParts := parts[:len(parts)-1]
		extension := parts[len(parts)-1]
		name = strings.Join(baseParts, "_") + "." + extension
	}

	return name
}

// splitNameAndExtension splits a filename into base name and extension
func (w *FATWriter) splitNameAndExtension(name string) (string, string) {
	// Find the last dot
	lastDot := strings.LastIndex(name, ".")
	if lastDot == -1 || lastDot == 0 {
		// No extension or dot at beginning
		return name, ""
	}

	baseName := name[:lastDot]
	extension := name[lastDot+1:]

	return baseName, extension
}

// createInitialShortName creates the initial 8.3 short name before collision resolution
func (w *FATWriter) createInitialShortName(baseName, extension string) ShortName {
	var shortName ShortName

	// Initialize with spaces
	for i := range shortName.Name {
		shortName.Name[i] = ' '
	}
	for i := range shortName.Extension {
		shortName.Extension[i] = ' '
	}

	// Copy base name (max 8 characters)
	baseLen := len(baseName)
	if baseLen > 8 {
		baseLen = 8
	}
	copy(shortName.Name[:baseLen], baseName[:baseLen])

	// Copy extension (max 3 characters)
	extLen := len(extension)
	if extLen > 3 {
		extLen = 3
	}
	copy(shortName.Extension[:extLen], extension[:extLen])

	return shortName
}

// resolveShortNameCollisions generates numeric tails to resolve name collisions
func (w *FATWriter) resolveShortNameCollisions(initialShortName ShortName, originalLongName string, directoryPath string) ShortName {
	// If the original name is already 8.3 compliant and doesn't have invalid chars,
	// we can use it directly
	if w.isValid83Name(originalLongName) {
		cleanOriginal := strings.ToUpper(originalLongName)
		baseName, extension := w.splitNameAndExtension(cleanOriginal)
		candidate := w.createInitialShortName(baseName, extension)
		if !w.shortNameExists(candidate, directoryPath) {
			w.registerShortName(candidate, directoryPath)
			return candidate
		}
	}

	// Check if the initial short name has no collision
	if !w.shortNameExists(initialShortName, directoryPath) {
		w.registerShortName(initialShortName, directoryPath)
		return initialShortName
	}

	// Generate numeric tail to resolve collision
	baseName := strings.TrimSpace(string(initialShortName.Name[:]))
	extension := strings.TrimSpace(string(initialShortName.Extension[:]))

	// Try numeric tails from ~1 to ~999999
	for i := 1; i <= 999999; i++ {
		tail := fmt.Sprintf("~%d", i)

		// Calculate available space for base name
		maxBaseLen := 8 - len(tail)
		if maxBaseLen < 1 {
			maxBaseLen = 1
		}

		// Truncate base name if necessary
		truncatedBase := baseName
		if len(truncatedBase) > maxBaseLen {
			truncatedBase = truncatedBase[:maxBaseLen]
		}

		// Create candidate short name
		candidateName := truncatedBase + tail
		candidate := w.createInitialShortName(candidateName, extension)

		// Check if this candidate is available
		if !w.shortNameExists(candidate, directoryPath) {
			w.registerShortName(candidate, directoryPath)
			return candidate
		}
	}

	// Fallback - this should never happen in practice
	w.registerShortName(initialShortName, directoryPath)
	return initialShortName
}

// isValid83Name checks if a name is already a valid 8.3 name
func (w *FATWriter) isValid83Name(name string) bool {
	// Check length constraints
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return false // Multiple dots not allowed
	}

	baseName := parts[0]
	if len(baseName) == 0 || len(baseName) > 8 {
		return false
	}

	if len(parts) == 2 {
		extension := parts[1]
		if len(extension) > 3 {
			return false
		}
	}

	// Check for invalid characters
	invalidChars := `"*+,/:;<=>?[\]| `
	for _, char := range invalidChars {
		if strings.ContainsRune(name, char) {
			return false
		}
	}

	// Check if all uppercase (FAT requirement)
	return name == strings.ToUpper(name)
}

// shortNameExists checks if a short name already exists in the given directory
func (w *FATWriter) shortNameExists(shortName ShortName, directoryPath string) bool {
	dirShortNames, exists := w.shortNames[directoryPath]
	if !exists {
		return false
	}

	shortNameStr := w.shortNameToString(shortName)
	return dirShortNames[shortNameStr]
}

// registerShortName registers a short name as used in the given directory
func (w *FATWriter) registerShortName(shortName ShortName, directoryPath string) {
	if w.shortNames[directoryPath] == nil {
		w.shortNames[directoryPath] = make(map[string]bool)
	}

	shortNameStr := w.shortNameToString(shortName)
	w.shortNames[directoryPath][shortNameStr] = true
}

// shortNameToString converts a ShortName to a string for comparison
func (w *FATWriter) shortNameToString(shortName ShortName) string {
	name := strings.TrimSpace(string(shortName.Name[:]))
	extension := strings.TrimSpace(string(shortName.Extension[:]))

	if extension == "" {
		return name
	}
	return name + "." + extension
}

// needsLFN determines if a filename requires Long File Name entries
func (w *FATWriter) needsLFN(filename string) bool {
	// Special directory entries never need LFN
	if filename == "." || filename == ".." {
		return false
	}

	// Check if filename contains lowercase letters
	if strings.ToUpper(filename) != filename {
		return true
	}

	// Check if filename contains spaces
	if strings.Contains(filename, " ") {
		return true
	}

	// Check if filename is longer than 8.3 format allows
	parts := strings.Split(filename, ".")
	if len(parts) > 2 {
		return true // Multiple dots
	}

	if len(parts) == 2 {
		// Has extension
		if len(parts[0]) > 8 || len(parts[1]) > 3 {
			return true
		}
	} else {
		// No extension
		if len(parts[0]) > 8 {
			return true
		}
	}

	// Check for invalid characters that would need replacement
	invalidChars := `"*+,/:;<=>?[\]|`
	for _, char := range invalidChars {
		if strings.ContainsRune(filename, char) {
			return true
		}
	}

	return false
}

// calculateLFNChecksum calculates the checksum for a short name used in LFN entries
func (w *FATWriter) calculateLFNChecksum(shortName ShortName) uint8 {
	var checksum uint8 = 0

	// Process all 11 bytes of the short name (8 name + 3 extension)
	for i := 0; i < 8; i++ {
		checksum = ((checksum & 1) << 7) + (checksum >> 1) + shortName.Name[i]
	}
	for i := 0; i < 3; i++ {
		checksum = ((checksum & 1) << 7) + (checksum >> 1) + shortName.Extension[i]
	}

	return checksum
}

// utf8ToUTF16LE converts a UTF-8 string to UTF-16LE byte array
func (w *FATWriter) utf8ToUTF16LE(s string) []byte {
	runes := []rune(s)
	result := make([]byte, len(runes)*2)

	for i, r := range runes {
		// For basic multilingual plane characters (BMP), just encode directly
		if r <= 0xFFFF {
			result[i*2] = byte(r & 0xFF)          // Low byte
			result[i*2+1] = byte((r >> 8) & 0xFF) // High byte
		} else {
			// For characters outside BMP, we'd need surrogate pairs
			// For now, replace with '?' (0x003F)
			result[i*2] = 0x3F
			result[i*2+1] = 0x00
		}
	}

	return result
}

// generateLFNEntries creates a sequence of LFN entries for a long filename
func (w *FATWriter) generateLFNEntries(longName string, shortNameChecksum uint8) []*LongFileNameEntry {
	// Convert filename to UTF-16LE
	utf16Name := w.utf8ToUTF16LE(longName)

	// Each LFN entry can hold 13 characters (26 bytes) of UTF-16LE data
	// name1: 5 chars (10 bytes), name2: 6 chars (12 bytes), name3: 2 chars (4 bytes)
	const charsPerEntry = 13
	const bytesPerEntry = charsPerEntry * 2 // UTF-16LE is 2 bytes per character

	// Calculate how many LFN entries we need
	entriesNeeded := (len(utf16Name) + bytesPerEntry - 1) / bytesPerEntry
	if entriesNeeded == 0 {
		entriesNeeded = 1 // At least one entry
	}

	entries := make([]*LongFileNameEntry, entriesNeeded)

	// Create entries in reverse order (LFN entries are stored backwards)
	for i := 0; i < entriesNeeded; i++ {
		entry := NewLongFileNameEntry()

		// Set order field - last entry has 0x40 bit set
		order := uint8(entriesNeeded - i)
		if i == 0 { // First entry we create is actually the last in sequence
			order |= 0x40 // Mark as last LFN entry
		}
		entry.SetOrder(order)

		// Set checksum
		entry.SetChecksum(shortNameChecksum)

		// Calculate which portion of the name this entry contains
		startOffset := i * bytesPerEntry
		endOffset := startOffset + bytesPerEntry
		if endOffset > len(utf16Name) {
			endOffset = len(utf16Name)
		}

		// Create padded UTF-16LE data for this entry
		entryData := make([]byte, bytesPerEntry)
		actualLength := endOffset - startOffset

		// Copy the actual UTF-16 data for this entry
		copy(entryData, utf16Name[startOffset:endOffset])

		// Handle padding according to VFAT specification
		if actualLength < bytesPerEntry {
			// If this is the last entry that contains the end of the filename
			if endOffset >= len(utf16Name) {
				// Add null terminator (0x0000) immediately after the string
				if actualLength < bytesPerEntry-1 {
					entryData[actualLength] = 0x00
					entryData[actualLength+1] = 0x00
					actualLength += 2
				}

				// Fill remaining bytes with 0xFFFF
				for j := actualLength; j < bytesPerEntry; j += 2 {
					if j+1 < bytesPerEntry {
						entryData[j] = 0xFF
						entryData[j+1] = 0xFF
					}
				}
			}
			// If this entry doesn't contain the end, it should be completely filled
			// with UTF-16 characters (no special padding needed)
		}

		// Split the entry data into name1, name2, name3 fields
		var name1 [10]byte
		var name2 [12]byte
		var name3 [4]byte

		copy(name1[:], entryData[0:10])  // First 5 chars (10 bytes)
		copy(name2[:], entryData[10:22]) // Next 6 chars (12 bytes)
		copy(name3[:], entryData[22:26]) // Final 2 chars (4 bytes)

		entry.SetName1(name1)
		entry.SetName2(name2)
		entry.SetName3(name3)

		entries[i] = entry
	}

	return entries
}

// createMappedDirectoryEntry creates and maps directory entries (LFN + short entry) at the specified offset
// Returns the next available offset after all entries have been written
func (w *FATWriter) createMappedDirectoryEntry(offset int64, name string, cluster uint32, size int64, isDir bool) int64 {
	currentOffset := offset

	// Generate proper 8.3 short name
	shortName := w.generateShortName(name, isDir)

	// Check if we need LFN entries
	if w.needsLFN(name) {
		// Calculate checksum for the short name
		checksum := w.calculateLFNChecksum(shortName)

		// Generate LFN entries
		lfnEntries := w.generateLFNEntries(name, checksum)

		// Write LFN entries first (they come before the short entry)
		for _, lfnEntry := range lfnEntries {
			if err := w.mapRegion(lfnEntry, currentOffset); err != nil {
				panic(fmt.Sprintf("failed to map LFN entry at offset %d: %v", currentOffset, err))
			}
			currentOffset += int64(lfnEntry.Size())
		}
	}

	// Create and map the final short directory entry
	var entry DirectoryEntry
	if err := w.mapRegion(&entry, currentOffset); err != nil {
		panic(fmt.Sprintf("failed to map directory entry at offset %d: %v", currentOffset, err))
	}

	entry.SetName(shortName.Name)
	entry.SetExtension(shortName.Extension)

	// Set attributes using generated methods
	if isDir {
		entry.SetAttributes(ATTR_DIRECTORY)
	} else {
		entry.SetAttributes(ATTR_ARCHIVE)
	}

	// Set time fields using configured time and generated methods
	now := w.getTimestamp()
	creationTime := encodeFATTime(now)
	creationDate := encodeFATDate(now)

	entry.SetCreationTime(creationTime)
	entry.SetCreationDate(creationDate)
	entry.SetLastAccessDate(creationDate)
	entry.SetLastModificationTime(creationTime)
	entry.SetLastModificationDate(creationDate)

	// Set cluster and size using generated methods
	entry.SetFirstClusterHigh(uint16(cluster >> 16))
	entry.SetFirstClusterLow(uint16(cluster))
	entry.SetFileSize(uint32(size))

	// Return offset after the short entry
	return currentOffset + int64(entry.Size())
}

// processBootloader handles bootloader file installation during filesystem creation
func (w *FATWriter) processBootloader() error {
	slog.Info("Starting bootloader processing")

	bootloader := w.deterministicConfig.Bootloader
	if bootloader == nil {
		slog.Info("No bootloader configuration found")
		return nil
	}

	slog.Info("Processing bootloader", "type", bootloader.Type, "core_files_count", len(bootloader.CoreFiles), "has_config", bootloader.Config != "")

	// Install core bootloader files
	for filename, content := range bootloader.CoreFiles {
		slog.Info("Installing bootloader core file", "filename", filename, "size", len(content))
		// Create file in root directory
		if err := w.addBootloaderFile(filename, content); err != nil {
			slog.Error("Failed to add bootloader file", "filename", filename, "error", err)
			return fmt.Errorf("failed to add bootloader file %s: %w", filename, err)
		}
		slog.Info("Successfully installed bootloader core file", "filename", filename)
	}

	// Create bootloader configuration file if specified
	if bootloader.Config != "" {
		configFile := "syslinux.cfg"
		if bootloader.Type == "systemd-boot" {
			configFile = "loader.conf"
		}

		slog.Info("Installing bootloader config file", "filename", configFile, "size", len(bootloader.Config))
		if err := w.addBootloaderFile(configFile, []byte(bootloader.Config)); err != nil {
			slog.Error("Failed to add bootloader config file", "filename", configFile, "error", err)
			return fmt.Errorf("failed to add bootloader config file %s: %w", configFile, err)
		}
		slog.Info("Successfully installed bootloader config file", "filename", configFile)
	}

	// Now that all bootloader files are installed, patch the VBR with ldlinux.sys sector location
	if bootloader.Type == "syslinux" {
		if err := w.patchVBRAfterFileInstallation(); err != nil {
			slog.Error("Failed to patch VBR with ldlinux.sys sector", "error", err)
			return fmt.Errorf("failed to patch VBR with ldlinux.sys sector: %w", err)
		}
	}

	slog.Info("Bootloader processing complete")
	return nil
}

// addBootloaderFile adds a bootloader file to the root directory
func (w *FATWriter) addBootloaderFile(filename string, content []byte) error {
	// Get root directory
	rootNode, exists := w.nodes["root"]
	if !exists {
		return fmt.Errorf("root directory not found")
	}

	// Create a new writable node for the bootloader file
	fileNode := &FATWritableNode{
		id:       fmt.Sprintf("bootloader_%s", filename),
		name:     filename,
		size:     int64(len(content)),
		isDir:    false,
		children: nil,
		parent:   rootNode,
	}

	// Allocate clusters for the file if it has content
	if len(content) > 0 {
		clusterSize := w.layout.ClusterSize()
		clustersNeeded := uint32((len(content) + int(clusterSize) - 1) / int(clusterSize))

		firstCluster, err := w.allocateMultipleClusters(clustersNeeded)
		if err != nil {
			return fmt.Errorf("failed to allocate clusters for bootloader file %s: %w", filename, err)
		}
		fileNode.cluster = firstCluster

		// Track ldlinux.sys cluster for VBR patching
		if filename == "ldlinux.sys" {
			w.ldlinuxSysCluster = firstCluster
			slog.Info("Tracking ldlinux.sys cluster for VBR patching", "cluster", firstCluster)
		}

		// Write file content to allocated clusters
		clusters := w.fatTable.GetClusterChain(firstCluster)
		offset := 0
		for _, cluster := range clusters {
			clusterOffset := int64(w.layout.Cluster(cluster))
			writeSize := min(len(content)-offset, int(clusterSize))

			// Map content directly to cluster data
			if err := w.mapContentToOffset(content[offset:offset+writeSize], clusterOffset); err != nil {
				return fmt.Errorf("failed to write bootloader file content: %w", err)
			}

			offset += writeSize
			if offset >= len(content) {
				break
			}
		}
	}

	// Add file to root directory children
	rootNode.children = append(rootNode.children, fileNode)
	w.nodes[fileNode.id] = fileNode

	return nil
}

// allocateMultipleClusters allocates multiple clusters and chains them together
func (w *FATWriter) allocateMultipleClusters(count uint32) (uint32, error) {
	if count == 0 {
		return 0, fmt.Errorf("cannot allocate zero clusters")
	}

	// Allocate first cluster
	firstCluster, err := w.fatTable.AllocateCluster()
	if err != nil {
		return 0, fmt.Errorf("failed to allocate first cluster: %w", err)
	}

	if count == 1 {
		// Mark as end of chain
		if err := w.fatTable.MarkEndOfChain(firstCluster); err != nil {
			return 0, fmt.Errorf("failed to mark cluster as end of chain: %w", err)
		}
		return firstCluster, nil
	}

	// Allocate and chain remaining clusters
	previousCluster := firstCluster
	for i := uint32(1); i < count; i++ {
		nextCluster, err := w.fatTable.AllocateCluster()
		if err != nil {
			return 0, fmt.Errorf("failed to allocate cluster %d: %w", i+1, err)
		}

		// Link previous cluster to this one
		if err := w.fatTable.SetEntry(previousCluster, nextCluster); err != nil {
			return 0, fmt.Errorf("failed to link cluster %d to %d: %w", previousCluster, nextCluster, err)
		}
		previousCluster = nextCluster
	}

	// Mark last cluster as end of chain
	if err := w.fatTable.MarkEndOfChain(previousCluster); err != nil {
		return 0, fmt.Errorf("failed to mark last cluster as end of chain: %w", err)
	}

	return firstCluster, nil
}

// mapContentToOffset maps content directly to a filesystem offset
func (w *FATWriter) mapContentToOffset(content []byte, offset int64) error {
	// Use the layout's virtual memory to write content directly
	if _, err := w.layout.storage.WriteAt(content, offset); err != nil {
		return fmt.Errorf("failed to write content at offset %d: %w", offset, err)
	}
	return nil
}

// injectBootCode injects bootloader boot code into the FAT boot sector
func (w *FATWriter) injectBootCode() error {
	bootloader := w.deterministicConfig.Bootloader
	if bootloader == nil {
		return nil
	}

	fs := w.layout.Fs()

	if bootloader.Type == "syslinux" {
		// For SYSLINUX, we need to create a proper SYSLINUX VBR
		return w.createSyslinuxVBR(bootloader)
	}

	// For other bootloaders, use the standard boot code injection
	bootCode := bootloader.BootCode
	if len(bootCode) == 0 {
		return nil
	}

	// Determine available boot code space based on FAT type
	var maxBootCodeSize int
	var skipBytes int

	if fs.IsFAT32() {
		// FAT32: boot code area size 420 bytes
		maxBootCodeSize = 420
		skipBytes = 90 // Skip to after the BPB for FAT32
	} else {
		// FAT12/16: boot code area size 448 bytes
		maxBootCodeSize = 448
		skipBytes = 62 // Skip to after the BPB for FAT12/16
	}

	// Ensure we don't exceed available space
	if len(bootCode) > maxBootCodeSize {
		bootCode = bootCode[:maxBootCodeSize]
	}

	// Skip the first bytes of SYSLINUX boot code that contain the jump instruction
	// We need to preserve the existing BPB structure
	if len(bootCode) > skipBytes {
		// Get boot sector offset (always sector 0)
		bootSectorOffset := int64(0)

		// Calculate where to inject boot code (after BPB)
		injectOffset := bootSectorOffset + int64(skipBytes)

		// Calculate how much boot code to inject
		injectSize := min(len(bootCode)-skipBytes, maxBootCodeSize-skipBytes)

		// Inject boot code directly into boot sector
		if err := w.mapContentToOffset(bootCode[skipBytes:skipBytes+injectSize], injectOffset); err != nil {
			return fmt.Errorf("failed to inject boot code: %w", err)
		}
	}

	return nil
}

// createSyslinuxVBR creates a proper SYSLINUX VBR using the VBR template
func (w *FATWriter) createSyslinuxVBR(bootloader *BootloaderConfig) error {
	// Use the VBR template from the bootloader config
	if len(bootloader.BootCode) == 0 {
		return fmt.Errorf("no SYSLINUX VBR template available")
	}

	vbrTemplate := bootloader.BootCode
	if len(vbrTemplate) < 512 {
		return fmt.Errorf("SYSLINUX VBR template too small: %d bytes", len(vbrTemplate))
	}

	// Create a copy of the VBR template to modify
	vbrSector := make([]byte, 512)
	copy(vbrSector, vbrTemplate[:512])

	// Debug: show original template bytes at 0x118
	originalTemplateBytes := fmt.Sprintf("%02x %02x %02x %02x %02x %02x %02x %02x",
		vbrTemplate[0x114], vbrTemplate[0x115], vbrTemplate[0x116], vbrTemplate[0x117],
		vbrTemplate[0x118], vbrTemplate[0x119], vbrTemplate[0x11A], vbrTemplate[0x11B])
	slog.Info("Original VBR template bytes around 0x118", "bytes", originalTemplateBytes)

	// The VBR template contains:
	// 0x00-0x02: Jump instruction (EB 58 90)
	// 0x03-0x0A: OEM name "SYSLINUX" (already correct)
	// 0x0B-0x59: BPB fields (need to preserve from current filesystem)
	// 0x5A-0x1FD: Boot code (already correct in template)
	// 0x1FE-0x1FF: Boot signature 55 AA (already correct)

	// Read current BPB from filesystem to preserve it
	currentBPB := make([]byte, 0x59-0x0B) // BPB size for FAT32
	_, err := w.layout.storage.ReadAt(currentBPB, 0x0B)
	if err != nil {
		return fmt.Errorf("failed to read current BPB: %w", err)
	}

	// Copy current BPB into VBR template, preserving filesystem metadata
	copy(vbrSector[0x0B:0x59], currentBPB)

	// Write the complete VBR sector
	if err := w.mapContentToOffset(vbrSector, 0); err != nil {
		return fmt.Errorf("failed to write SYSLINUX VBR: %w", err)
	}

	slog.Info("Installed SYSLINUX VBR with preserved BPB (sector patching deferred until ldlinux.sys installation)")
	return nil
}

// clusterToAbsoluteSector converts a cluster number to absolute disk sector
func (w *FATWriter) clusterToAbsoluteSector(cluster uint32) (uint64, error) {
	if cluster < 2 {
		return 0, fmt.Errorf("invalid cluster number: %d (clusters start at 2)", cluster)
	}

	// Get cluster byte offset within the filesystem
	clusterOffset := w.layout.Cluster(cluster)

	// Convert byte offset to sector number (512 bytes per sector)
	filesystemSector := uint64(clusterOffset / 512)

	// Add partition start LBA to get absolute disk sector
	if w.deterministicConfig != nil && w.deterministicConfig.Partition != nil {
		absoluteSector := w.deterministicConfig.Partition.StartLBA + filesystemSector
		return absoluteSector, nil
	}

	// Fallback to filesystem-relative sector if no partition info
	return filesystemSector, nil
}

// patchVBRAfterFileInstallation patches the VBR in virtual memory after ldlinux.sys installation
func (w *FATWriter) patchVBRAfterFileInstallation() error {
	if w.ldlinuxSysCluster == 0 {
		slog.Info("No ldlinux.sys cluster tracked, skipping VBR sector patching")
		return nil
	}

	// Re-create the VBR from original template and re-apply BPB preservation
	// This ensures we have the proper template with the 0xdeadbeef placeholder
	bootloader := w.deterministicConfig.Bootloader
	if bootloader == nil || len(bootloader.BootCode) == 0 {
		return fmt.Errorf("no VBR template available for patching")
	}

	vbrTemplate := bootloader.BootCode
	if len(vbrTemplate) < 512 {
		return fmt.Errorf("VBR template too small for patching: %d bytes", len(vbrTemplate))
	}

	// Create fresh VBR from template
	vbrSector := make([]byte, 512)
	copy(vbrSector, vbrTemplate[:512])

	// Re-read current BPB from filesystem to preserve it
	currentBPB := make([]byte, 0x59-0x0B) // BPB size for FAT32
	_, err := w.layout.storage.ReadAt(currentBPB, 0x0B)
	if err != nil {
		return fmt.Errorf("failed to read current BPB for patching: %w", err)
	}

	// Copy current BPB into fresh VBR template
	copy(vbrSector[0x0B:0x59], currentBPB)

	// Now patch the VBR with ldlinux.sys sector location
	if err := w.patchVBRWithLdlinuxSysSector(vbrSector); err != nil {
		return fmt.Errorf("failed to patch VBR sector: %w", err)
	}

	// Write the patched VBR sector back to virtual memory
	if err := w.mapContentToOffset(vbrSector, 0); err != nil {
		return fmt.Errorf("failed to write patched VBR sector: %w", err)
	}

	slog.Info("Successfully patched VBR with ldlinux.sys sector location after file installation")
	return nil
}

// patchVBRWithLdlinuxSysSector patches the VBR template with actual ldlinux.sys sector location
func (w *FATWriter) patchVBRWithLdlinuxSysSector(vbrSector []byte) error {
	if w.ldlinuxSysCluster == 0 {
		slog.Info("No ldlinux.sys cluster tracked, skipping VBR sector patching")
		return nil
	}

	// Convert cluster to absolute disk sector
	absoluteSector, err := w.clusterToAbsoluteSector(w.ldlinuxSysCluster)
	if err != nil {
		return fmt.Errorf("failed to convert ldlinux.sys cluster to sector: %w", err)
	}

	// SYSLINUX VBR expects sector number at offset 0x118 (0xdeadbeef placeholder)
	// Patch with little-endian 32-bit sector number
	if len(vbrSector) < 0x11C {
		return fmt.Errorf("VBR sector too small for patching (need at least 284 bytes, got %d)", len(vbrSector))
	}

	// Debug: show current and new values
	currentBytes := fmt.Sprintf("%02x %02x %02x %02x", vbrSector[0x118], vbrSector[0x119], vbrSector[0x11A], vbrSector[0x11B])

	// Write 32-bit little-endian sector number at offset 0x118
	vbrSector[0x118] = byte(absoluteSector & 0xFF)
	vbrSector[0x119] = byte((absoluteSector >> 8) & 0xFF)
	vbrSector[0x11A] = byte((absoluteSector >> 16) & 0xFF)
	vbrSector[0x11B] = byte((absoluteSector >> 24) & 0xFF)

	newBytes := fmt.Sprintf("%02x %02x %02x %02x", vbrSector[0x118], vbrSector[0x119], vbrSector[0x11A], vbrSector[0x11B])
	slog.Info("VBR sector patching details", "current_bytes", currentBytes, "new_bytes", newBytes, "offset", "0x118")

	slog.Info("Patched VBR with ldlinux.sys sector location",
		"cluster", w.ldlinuxSysCluster,
		"absolute_sector", absoluteSector,
		"partition_start_lba", func() uint64 {
			if w.deterministicConfig != nil && w.deterministicConfig.Partition != nil {
				return w.deterministicConfig.Partition.StartLBA
			}
			return 0
		}())

	return nil
}
