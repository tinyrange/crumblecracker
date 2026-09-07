package fat

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/common"
	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

// TestDeterministicGeneration verifies that filesystems generate identical bytes with same input
func TestDeterministicGeneration(t *testing.T) {
	const size = 1024 * 1024 * 1024 // 1GB - forces FAT32

	config := DefaultDeterministicConfig()
	filesystem1 := createTestFilesystem(t, size, config)
	filesystem2 := createTestFilesystem(t, size, config)

	if !compareBytesEqual(filesystem1, filesystem2) {
		t.Error("Deterministic filesystems should be byte-identical but differ")
	}
}

// TestCustomDeterministicConfig tests different deterministic configurations
func TestCustomDeterministicConfig(t *testing.T) {
	const size = 1024 * 1024 * 1024 // 1GB

	customTime := time.Date(2023, 12, 25, 10, 30, 0, 0, time.UTC)
	volumeSerial := uint32(0xDEADBEEF)
	config := &DeterministicConfig{
		FixedTimestamp:       &customTime,
		SortDirectoryEntries: true,
		VolumeSerial:         &volumeSerial,
	}

	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystemWithConfig(vmFS, size, config)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}
	if got := writer.getTimestamp(); !got.Equal(customTime) {
		t.Fatalf("timestamp = %s, want %s", got, customTime)
	}
	if got := writer.layout.Fs().VolumeId(); got != volumeSerial {
		t.Fatalf("volume serial = %#x, want %#x", got, volumeSerial)
	}
}

// TestDirectoryEntrySorting verifies that directory entries are sorted deterministically
func TestDirectoryEntrySorting(t *testing.T) {
	const size = 1024 * 1024 * 1024 // 1GB

	config := DefaultDeterministicConfig()

	// Create filesystem with files added in different orders
	filesystem1 := createTestFilesystemWithFiles(t, size, config, []string{"zebra.txt", "alpha.txt", "beta.txt"})
	filesystem2 := createTestFilesystemWithFiles(t, size, config, []string{"alpha.txt", "zebra.txt", "beta.txt"})
	filesystem3 := createTestFilesystemWithFiles(t, size, config, []string{"beta.txt", "alpha.txt", "zebra.txt"})

	// All should be identical due to sorting
	hash1 := sha256.Sum256(filesystem1)
	hash2 := sha256.Sum256(filesystem2)
	hash3 := sha256.Sum256(filesystem3)

	if hash1 != hash2 || hash2 != hash3 {
		t.Error("Filesystems with same files but different creation order should be identical when sorted")
	}
}

// createTestFilesystem creates a test filesystem with some content
func createTestFilesystem(t *testing.T, size int64, config *DeterministicConfig) []byte {
	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystemWithConfig(vmFS, size, config)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	// Create some test content
	root, err := writer.WritableRootDirectory()
	if err != nil {
		t.Fatalf("Failed to get root directory: %v", err)
	}

	// Create a file
	fileNode, err := writer.AllocateNode()
	if err != nil {
		t.Fatalf("Failed to allocate file node: %v", err)
	}

	if err := fileNode.SetName("test.txt"); err != nil {
		t.Fatalf("Failed to set file name: %v", err)
	}

	content := vm.RawRegion([]byte("Hello, deterministic world!"))
	if err := writer.WriteContents(fileNode, &content); err != nil {
		t.Fatalf("Failed to write file contents: %v", err)
	}

	// Create a directory
	dirNode, err := writer.AllocateNode()
	if err != nil {
		t.Fatalf("Failed to allocate directory node: %v", err)
	}

	if err := dirNode.SetName("testdir"); err != nil {
		t.Fatalf("Failed to set directory name: %v", err)
	}

	fatNode := dirNode.(*FATWritableNode)
	fatNode.SetDir()

	// Write directory structure
	if err := writer.WriteDirectory(dirNode, []common.WritableNode{}); err != nil {
		t.Fatalf("Failed to write directory: %v", err)
	}

	if err := writer.WriteDirectory(root, []common.WritableNode{fileNode, dirNode}); err != nil {
		t.Fatalf("Failed to write root directory: %v", err)
	}

	// Finalize
	if err := writer.Finalize(); err != nil {
		t.Fatalf("Failed to finalize filesystem: %v", err)
	}

	// Extract bytes
	data := make([]byte, size)
	if _, err := vmFS.ReadAt(data, 0); err != nil {
		t.Fatalf("Failed to read filesystem data: %v", err)
	}

	return data
}

// createTestFilesystemWithFiles creates a filesystem with specific files
func createTestFilesystemWithFiles(t *testing.T, size int64, config *DeterministicConfig, filenames []string) []byte {
	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystemWithConfig(vmFS, size, config)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	root, err := writer.WritableRootDirectory()
	if err != nil {
		t.Fatalf("Failed to get root directory: %v", err)
	}

	var children []common.WritableNode

	// Create files in the specified order
	for i, filename := range filenames {
		fileNode, err := writer.AllocateNode()
		if err != nil {
			t.Fatalf("Failed to allocate file node %d: %v", i, err)
		}

		if err := fileNode.SetName(filename); err != nil {
			t.Fatalf("Failed to set file name %s: %v", filename, err)
		}

		content := vm.RawRegion([]byte("content of " + filename))
		if err := writer.WriteContents(fileNode, &content); err != nil {
			t.Fatalf("Failed to write contents for %s: %v", filename, err)
		}

		children = append(children, fileNode)
	}

	if err := writer.WriteDirectory(root, children); err != nil {
		t.Fatalf("Failed to write root directory: %v", err)
	}

	if err := writer.Finalize(); err != nil {
		t.Fatalf("Failed to finalize filesystem: %v", err)
	}

	// Extract bytes
	data := make([]byte, size)
	if _, err := vmFS.ReadAt(data, 0); err != nil {
		t.Fatalf("Failed to read filesystem data: %v", err)
	}

	return data
}

// compareBytesEqual compares two byte slices for exact equality
func compareBytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
