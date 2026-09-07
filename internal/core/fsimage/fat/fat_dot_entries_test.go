package fat

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/common"
	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

// TestDirectoryEntryOrdering verifies that "." and ".." entries are correctly
// placed as the first two entries in subdirectories, and that root directories
// do not have these entries.
func TestDirectoryEntryOrdering(t *testing.T) {
	// Create a 10MB FAT filesystem for testing
	const size = 10 * 1024 * 1024
	vmFS := vm.NewVirtualMemory(size, 4096)

	// Create FAT writer
	writer, err := CreateFATFileSystem(vmFS, size)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	// Get root directory
	root, err := writer.WritableRootDirectory()
	if err != nil {
		t.Fatalf("Failed to get root directory: %v", err)
	}

	// Create a subdirectory
	subDir, err := writer.AllocateNode()
	if err != nil {
		t.Fatalf("Failed to allocate subdirectory node: %v", err)
	}

	if err := subDir.SetName("testdir"); err != nil {
		t.Fatalf("Failed to set subdirectory name: %v", err)
	}

	fatSubDir := subDir.(*FATWritableNode)
	fatSubDir.SetDir()

	// Create a file in the subdirectory
	subFile, err := writer.AllocateNode()
	if err != nil {
		t.Fatalf("Failed to allocate file node: %v", err)
	}

	if err := subFile.SetName("test.txt"); err != nil {
		t.Fatalf("Failed to set file name: %v", err)
	}

	content := "Test file content"
	contentData := vm.RawRegion([]byte(content))
	if err := writer.WriteContents(subFile, &contentData); err != nil {
		t.Fatalf("Failed to write file contents: %v", err)
	}

	// Set up directory structure
	if err := writer.WriteDirectory(subDir, []common.WritableNode{subFile}); err != nil {
		t.Fatalf("Failed to write subdirectory: %v", err)
	}

	if err := writer.WriteDirectory(root, []common.WritableNode{subDir}); err != nil {
		t.Fatalf("Failed to write root directory: %v", err)
	}

	if err := writer.Finalize(); err != nil {
		t.Fatalf("Failed to finalize filesystem: %v", err)
	}

	// Create reader to test directory structure
	reader, err := NewFATReader(vmFS)
	if err != nil {
		t.Fatalf("Failed to create FAT reader: %v", err)
	}

	// Test 1: Root directory should not have "." and ".." entries
	rootFiles, err := reader.listRootDirectory()
	if err != nil {
		t.Fatalf("Failed to list root directory: %v", err)
	}

	for _, file := range rootFiles {
		if file.Name == "." || file.Name == ".." {
			t.Errorf("Root directory should not contain %s entry", file.Name)
		}
	}

	// Test 2: Subdirectory should have "." and ".." as first two entries
	// Debug: Print all root directory entries
	t.Logf("Root directory contains %d entries:", len(rootFiles))
	for i, file := range rootFiles {
		t.Logf("  [%d] %s (isDir: %t)", i, file.Name, file.IsDirectory)
	}

	// Find the subdirectory
	var subDirInfo *FileInfo
	for i := range rootFiles {
		if rootFiles[i].IsDirectory && (rootFiles[i].Name == "testdir" || rootFiles[i].Name == "TESTDIR") {
			subDirInfo = &rootFiles[i]
			break
		}
	}

	if subDirInfo == nil {
		t.Fatal("Subdirectory 'testdir' not found")
	}

	// List subdirectory contents
	subDirFiles, err := reader.listDirectoryCluster(subDirInfo.Cluster)
	if err != nil {
		t.Fatalf("Failed to list subdirectory: %v", err)
	}

	// Check that subdirectory entries are in correct order
	if len(subDirFiles) < 3 {
		t.Fatalf("Expected at least 3 entries in subdirectory (., .., test.txt), got %d", len(subDirFiles))
	}

	expectedOrder := []string{".", "..", "TEST.TXT"}
	for i, expected := range expectedOrder {
		if i >= len(subDirFiles) {
			t.Errorf("Missing entry at position %d: expected %s", i, expected)
			continue
		}
		if subDirFiles[i].Name != expected {
			t.Errorf("Entry at position %d: expected %s, got %s", i, expected, subDirFiles[i].Name)
		}
	}

	// Test 3: Verify cluster references
	dotEntry := subDirFiles[0]
	dotDotEntry := subDirFiles[1]

	if dotEntry.Cluster != subDirInfo.Cluster {
		t.Errorf("'.' entry cluster reference incorrect: expected %d, got %d",
			subDirInfo.Cluster, dotEntry.Cluster)
	}

	if dotDotEntry.Cluster != 0 {
		t.Errorf("'..' entry cluster reference incorrect: expected 0 (root), got %d",
			dotDotEntry.Cluster)
	}
}
