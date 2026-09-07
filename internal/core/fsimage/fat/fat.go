package fat

import (
	"fmt"
	"io"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/common"
	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

// FileInfo represents a file or directory entry
type FileInfo struct {
	Name        string
	LongName    string
	Size        int64
	IsDirectory bool
	Cluster     uint32
	Attributes  uint8
}

// FATReader implements FileSystem interface for FAT filesystems
type FATReader struct {
	image  vm.MemoryRegion   // Original image for compatibility
	vm     *vm.VirtualMemory // Virtual memory manager (kept for compatibility)
	layout *FATLayout        // Layout system for structured access
}

func (r *FATReader) VM() *vm.VirtualMemory {
	return r.vm
}

// FATNode implements common.Node interface for FAT filesystem nodes
type FATNode struct {
	fileInfo *FileInfo
	reader   *FATReader
}

// Name returns the base name of the file or directory
func (n *FATNode) Name() string {
	if n.fileInfo.LongName != "" {
		return n.fileInfo.LongName
	}
	return n.fileInfo.Name
}

// Size returns the size of the file in bytes
func (n *FATNode) Size() int64 {
	return n.fileInfo.Size
}

// IsDir returns true if this is a directory, false if it's a file
func (n *FATNode) IsDir() bool {
	return n.fileInfo.IsDirectory
}

// FATFileHandle implements common.FileHandle interface for FAT file access
type FATFileHandle struct {
	reader io.ReaderAt
}

// ReadAt reads data from the file at a specific offset
func (h *FATFileHandle) ReadAt(p []byte, off int64) (int, error) {
	return h.reader.ReadAt(p, off)
}

// Close closes the file handle (no-op for memory regions)
func (h *FATFileHandle) Close() error {
	return nil
}

// Compile-time interface assertions
var (
	_ common.Node             = (*FATNode)(nil)
	_ common.FileHandle       = (*FATFileHandle)(nil)
	_ common.FileSystemReader = (*FATReader)(nil)
)

// NewFATReader creates a new FAT filesystem reader
func NewFATReader(image vm.MemoryRegion) (*FATReader, error) {
	// Create virtual memory instance with 4KB pages
	const pageSize = 4096
	virtualMem := vm.NewVirtualMemory(image.Size(), pageSize)

	// Map the entire image into virtual memory
	if err := virtualMem.Map(image, 0); err != nil {
		return nil, fmt.Errorf("failed to map image to virtual memory: %w", err)
	}

	// Create layout system for structured access with VM reinterpretation
	layout := NewFATLayout(virtualMem)

	reader := &FATReader{
		image:  image,
		vm:     virtualMem,
		layout: layout,
	}

	return reader, nil
}

func (r *FATReader) ListFiles(path string) ([]FileInfo, error) {
	if path == "" || path == "/" {
		// List root directory
		return r.listRootDirectory()
	}

	// Find the directory and list its contents
	return r.listSubdirectory(path)
}

func (r *FATReader) listRootDirectory() ([]FileInfo, error) {
	fs := r.layout.Fs()
	if fs.FatType() == "FAT32" {
		// For FAT32, root directory is stored in clusters starting at rootDirectoryCluster
		rootCluster := fs.RootDirectoryCluster()
		return r.listDirectoryCluster(rootCluster)
	}

	// For FAT12/16, root directory is a fixed area
	rootDirSize := uint32(fs.RootDirectoryEntries()) * 32
	rootDirOffset := fs.RootDirectoryOffset()

	// Create a ReaderAt that reads from the root directory region
	rootDirReader := io.NewSectionReader(r.vm, int64(rootDirOffset), int64(rootDirSize))

	return r.parseDirectoryEntries(rootDirReader, int64(rootDirSize))
}

func (r *FATReader) parseDirectoryEntries(reader io.ReaderAt, size int64) ([]FileInfo, error) {
	return r.parseDirectoryEntriesWithValidation(reader, size, false)
}

func (r *FATReader) parseDirectoryEntriesWithValidation(reader io.ReaderAt, size int64, isSubdirectory bool) ([]FileInfo, error) {
	var files []FileInfo
	var entryCount int

	// Parse directory entries
	for offset := int64(0); offset+32 <= size; offset += 32 {
		entryData := make([]byte, 32)
		n, err := reader.ReadAt(entryData, offset)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read directory entry at offset %d: %w", offset, err)
		}
		if n < 32 {
			break // Not enough data for a complete entry
		}

		// Check if entry is empty (first byte is 0)
		if entryData[0] == 0 {
			break // End of directory
		}

		// Skip deleted entries (first byte is 0xE5)
		if entryData[0] == 0xE5 {
			continue
		}

		// Use generated DirectoryEntry structure
		entry := (*DirectoryEntry)(entryData)

		// Skip long filename entries (attribute is 0x0F)
		if entry.Attributes() == ATTR_LFN {
			continue
		}

		// Skip volume label entries
		if entry.Attributes()&ATTR_VOLUME_ID != 0 {
			continue
		}

		// Validate directory entry ordering for special entries (only for subdirectories)
		// FAT specification mandates that subdirectories must have "." as the first entry
		// and ".." as the second entry. This is critical for filesystem integrity.
		if isSubdirectory {
			name := r.getEntryName(entry)
			if entryCount == 0 && name == "." {
				// Valid: first entry is "."
			} else if entryCount == 1 && name == ".." {
				// Valid: second entry is ".."
			} else if (entryCount == 0 && name != ".") || (entryCount == 1 && name != "..") {
				// Invalid ordering in subdirectory - this indicates a corrupted or
				// non-compliant FAT filesystem that may cause issues with other tools
				fmt.Printf("Warning: Directory entry ordering issue at position %d: expected %s, got %s\n",
					entryCount, []string{".", ".."}[entryCount], name)
			}
		}

		// Parse the entry using generated structure methods
		if fileInfo := r.parseDirectoryEntryStruct(entry); fileInfo != nil {
			files = append(files, *fileInfo)
		}
		entryCount++
	}

	return files, nil
}

// getEntryName extracts the name from a directory entry for validation
func (r *FATReader) getEntryName(entry *DirectoryEntry) string {
	nameBytes := entry.Name()
	extBytes := entry.Extension()

	name := strings.TrimSpace(string(nameBytes[:]))
	ext := strings.TrimSpace(string(extBytes[:]))

	if ext != "" && name != "." && name != ".." {
		name = name + "." + ext
	}

	// Clean up name (remove nulls and control characters)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)

	return name
}

func (r *FATReader) parseDirectoryEntryStruct(entry *DirectoryEntry) *FileInfo {
	// Extract filename (8.3 format) using generated methods
	nameBytes := entry.Name()
	extBytes := entry.Extension()

	name := strings.TrimSpace(string(nameBytes[:]))
	ext := strings.TrimSpace(string(extBytes[:]))

	if ext != "" {
		name = name + "." + ext
	}

	// Clean up name (remove nulls and control characters)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)

	if name == "" {
		return nil
	}

	// Extract attributes using generated method
	attributes := entry.Attributes()
	isDirectory := (attributes & ATTR_DIRECTORY) != 0

	// Extract cluster using generated computed field
	cluster := entry.FirstCluster()

	// Extract file size using generated method
	fileSize := entry.FileSize()

	return &FileInfo{
		Name:        name,
		LongName:    "", // TODO: Implement LFN parsing
		Size:        int64(fileSize),
		IsDirectory: isDirectory,
		Cluster:     cluster,
		Attributes:  attributes,
	}
}

func (r *FATReader) getClusterChain(startCluster uint32) ([]uint32, error) {
	if startCluster == 0 {
		return nil, nil
	}

	var chain []uint32
	current := startCluster

	eocValue := r.layout.Fs().EndOfChian()

	for current < eocValue {
		chain = append(chain, current)

		next := uint32(r.layout.FatEntry(current))

		// Check for end of chain - any value >= EOC threshold indicates end
		if next >= eocValue {
			break
		}

		if next == current {
			return nil, fmt.Errorf("circular cluster chain detected at cluster %d", current)
		}

		current = next
	}

	return chain, nil
}

func (r *FATReader) readClusterData(cluster uint32) (io.ReaderAt, error) {
	if cluster < 2 {
		return nil, fmt.Errorf("invalid cluster number: %d", cluster)
	}

	clusterRegion := r.layout.ClusterData(cluster)
	return clusterRegion, nil
}

func (r *FATReader) readClustersData(clusters []uint32) (io.ReaderAt, error) {
	if len(clusters) == 0 {
		return nil, fmt.Errorf("no clusters provided")
	}

	// For single cluster, return the cluster region directly
	if len(clusters) == 1 {
		return r.readClusterData(clusters[0])
	}

	// For multiple clusters, create a virtual memory region
	clusterSize := int64(r.layout.ClusterSize())
	totalSize := int64(len(clusters)) * clusterSize

	// Create a virtual memory space for the fragmented file
	virtualMem := vm.NewVirtualMemory(totalSize, uint32(clusterSize))

	// Map each cluster region consecutively
	var offset int64 = 0
	for _, clusterNum := range clusters {
		clusterRegion := r.layout.ClusterData(clusterNum)
		// Cast to MemoryRegion since ClusterData returns a region that implements both interfaces
		memRegion, ok := clusterRegion.(vm.MemoryRegion)
		if !ok {
			return nil, fmt.Errorf("cluster data does not implement MemoryRegion interface")
		}
		err := virtualMem.Map(memRegion, offset)
		if err != nil {
			return nil, fmt.Errorf("failed to map cluster %d at offset %d: %w", clusterNum, offset, err)
		}
		offset += clusterSize
	}

	return virtualMem, nil
}

// Subdirectory listing methods

func (r *FATReader) listSubdirectory(path string) ([]FileInfo, error) {
	// Parse path and find the directory
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return r.listRootDirectory()
	}

	// Start from root directory
	currentDir, err := r.listRootDirectory()
	if err != nil {
		return nil, fmt.Errorf("failed to list root directory: %w", err)
	}

	// Navigate through path components
	for _, part := range parts {
		if part == "" {
			continue
		}

		// Find the directory with this name
		var targetDir *FileInfo
		for i := range currentDir {
			if currentDir[i].IsDirectory && (currentDir[i].Name == part || currentDir[i].LongName == part) {
				targetDir = &currentDir[i]
				break
			}
		}

		if targetDir == nil {
			return nil, fmt.Errorf("directory not found: %s", part)
		}

		// List contents of this directory
		currentDir, err = r.listDirectoryCluster(targetDir.Cluster)
		if err != nil {
			return nil, fmt.Errorf("failed to list directory %s: %w", part, err)
		}
	}

	return currentDir, nil
}

func (r *FATReader) listDirectoryCluster(startCluster uint32) ([]FileInfo, error) {
	if startCluster == 0 {
		return nil, fmt.Errorf("invalid directory cluster: 0")
	}

	// Get cluster chain for directory
	clusters, err := r.getClusterChain(startCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster chain: %w", err)
	}

	// Read all directory data
	dirData, err := r.readClustersData(clusters)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory clusters: %w", err)
	}

	// Calculate total directory size
	clusterSize := int64(r.layout.ClusterSize())
	totalSize := int64(len(clusters)) * clusterSize

	// Parse directory entries (this is a subdirectory, so validate . and .. ordering)
	return r.parseDirectoryEntriesWithValidation(dirData, totalSize, true)
}

func (r *FATReader) ReadFile(path string) ([]byte, error) {
	// Parse path to find the file
	dir := "/"
	filename := path

	if strings.Contains(path, "/") {
		parts := strings.Split(path, "/")
		filename = parts[len(parts)-1]
		dir = strings.Join(parts[:len(parts)-1], "/")
		if dir == "" {
			dir = "/"
		}
	}

	// List directory to find the file
	files, err := r.ListFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to list directory %s: %w", dir, err)
	}

	// Find the file
	var targetFile *FileInfo
	for i := range files {
		if !files[i].IsDirectory && (files[i].Name == filename || files[i].LongName == filename) {
			targetFile = &files[i]
			break
		}
	}

	if targetFile == nil {
		return nil, fmt.Errorf("file not found: %s", filename)
	}

	// Read file data
	reader, err := r.readFileData(targetFile)
	if err != nil {
		return nil, err
	}

	// Read all data from the ReaderAt
	data := make([]byte, targetFile.Size)
	n, err := reader.ReadAt(data, 0)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read file data: %w", err)
	}

	return data[:n], nil
}

func (r *FATReader) readFileData(file *FileInfo) (io.ReaderAt, error) {
	if file.Size == 0 {
		return io.NewSectionReader(strings.NewReader(""), 0, 0), nil
	}

	if file.Cluster == 0 {
		return nil, fmt.Errorf("invalid file cluster: 0")
	}

	// Get cluster chain for file
	clusters, err := r.getClusterChain(file.Cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster chain: %w", err)
	}

	// Read all file data
	data, err := r.readClustersData(clusters)
	if err != nil {
		return nil, fmt.Errorf("failed to read file clusters: %w", err)
	}

	// Truncate to actual file size using SectionReader
	return io.NewSectionReader(data, 0, file.Size), nil
}

// FileSystemReader interface implementation

// RootDirectory returns the root directory node
func (r *FATReader) RootDirectory() (common.Node, error) {
	// Create a synthetic FileInfo for the root directory
	rootInfo := &FileInfo{
		Name:        "/",
		LongName:    "/",
		Size:        0,
		IsDirectory: true,
		Cluster:     0, // Root directory has special handling
		Attributes:  ATTR_DIRECTORY,
	}

	return &FATNode{
		fileInfo: rootInfo,
		reader:   r,
	}, nil
}

// ReadContents reads the contents of a file node
func (r *FATReader) ReadContents(node common.Node) (common.FileHandle, error) {
	fatNode, ok := node.(*FATNode)
	if !ok {
		return nil, fmt.Errorf("node is not a FATNode")
	}

	if fatNode.fileInfo.IsDirectory {
		return nil, fmt.Errorf("cannot read contents of directory")
	}

	// Use existing readFileData method
	reader, err := r.readFileData(fatNode.fileInfo)
	if err != nil {
		return nil, err
	}

	return &FATFileHandle{reader: reader}, nil
}

// ReadDirectory reads the contents of a directory node
func (r *FATReader) ReadDirectory(node common.Node, callback func(node common.Node) error) error {
	fatNode, ok := node.(*FATNode)
	if !ok {
		return fmt.Errorf("node is not a FATNode")
	}

	if !fatNode.fileInfo.IsDirectory {
		return fmt.Errorf("node is not a directory")
	}

	var files []FileInfo
	var err error

	// Handle root directory specially
	if fatNode.fileInfo.Cluster == 0 {
		files, err = r.listRootDirectory()
	} else {
		files, err = r.listDirectoryCluster(fatNode.fileInfo.Cluster)
	}

	if err != nil {
		return err
	}

	// Call callback for each file
	for _, file := range files {
		// Skip "." and ".." entries to avoid infinite loops
		if file.Name == "." || file.Name == ".." ||
			file.LongName == "." || file.LongName == ".." {
			continue
		}

		childNode := &FATNode{
			fileInfo: &file,
			reader:   r,
		}

		if err := callback(childNode); err != nil {
			return err
		}
	}

	return nil
}

// IterateNodes iterates through all nodes in the file system
func (r *FATReader) IterateNodes(callback func(node common.Node) error) error {
	// Start with root directory
	rootNode, err := r.RootDirectory()
	if err != nil {
		return err
	}

	// Recursive helper function
	var iterateRecursive func(node common.Node) error
	iterateRecursive = func(node common.Node) error {
		// Call callback for current node
		if err := callback(node); err != nil {
			return err
		}

		// If it's a directory, iterate through its children
		if node.IsDir() {
			return r.ReadDirectory(node, iterateRecursive)
		}

		return nil
	}

	// Start by iterating through the contents of root, not root itself
	return r.ReadDirectory(rootNode, iterateRecursive)
}
