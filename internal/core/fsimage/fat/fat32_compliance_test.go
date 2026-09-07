package fat

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/common"
	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

// TestFAT32Compliance verifies that FAT32 filesystems meet all specification requirements
func TestFAT32Compliance(t *testing.T) {
	// Create a 1GB FAT32 filesystem
	const size = 1024 * 1024 * 1024
	vmFS := vm.NewVirtualMemory(size, 4096)

	// Create FAT writer
	writer, err := CreateFATFileSystem(vmFS, size)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	// Verify it's actually FAT32
	fs := writer.layout.Fs()
	if fs.FatType() != "FAT32" {
		t.Fatalf("Expected FAT32, got %s", fs.FatType())
	}

	// Create a simple directory structure for testing
	root, err := writer.WritableRootDirectory()
	if err != nil {
		t.Fatalf("Failed to get root directory: %v", err)
	}

	// Create a test file
	file, err := writer.AllocateNode()
	if err != nil {
		t.Fatalf("Failed to allocate file node: %v", err)
	}
	file.SetName("test.txt")
	content := "Hello FAT32 World!"
	contentData := vm.RawRegion([]byte(content))
	writer.WriteContents(file, &contentData)

	// Create a test directory
	dir, err := writer.AllocateNode()
	if err != nil {
		t.Fatalf("Failed to allocate directory node: %v", err)
	}
	dir.SetName("testdir")
	fatDir := dir.(*FATWritableNode)
	fatDir.SetDir()

	// Set up directory structure
	writer.WriteDirectory(dir, []common.WritableNode{})
	writer.WriteDirectory(root, []common.WritableNode{file, dir})

	// Finalize filesystem
	if err := writer.Finalize(); err != nil {
		t.Fatalf("Failed to finalize filesystem: %v", err)
	}

	// Now run compliance tests
	t.Run("BootSectorCompliance", func(t *testing.T) {
		testBootSectorCompliance(t, writer)
	})

	t.Run("BackupBootSectorCompliance", func(t *testing.T) {
		testBackupBootSectorCompliance(t, writer)
	})

	t.Run("FSInfoSectorCompliance", func(t *testing.T) {
		testFSInfoSectorCompliance(t, writer)
	})

	t.Run("FAT32FieldValidation", func(t *testing.T) {
		testFAT32FieldValidation(t, writer)
	})

}

// testBootSectorCompliance verifies the primary boot sector meets FAT32 requirements
func testBootSectorCompliance(t *testing.T, writer *FATWriter) {
	fs := writer.layout.Fs()

	// Test FAT32-specific boot sector fields
	if fs.SectorsPerFat16() != 0 {
		t.Error("FAT32 should have sectorsPerFat16 = 0")
	}

	if fs.SectorsPerFat32() == 0 {
		t.Error("FAT32 should have non-zero sectorsPerFat32")
	}

	if fs.RootDirectoryCluster() != 2 {
		t.Errorf("FAT32 root directory should start at cluster 2, got %d", fs.RootDirectoryCluster())
	}

	if fs.FsInfoSector() == 0 {
		t.Error("FAT32 should have FSInfo sector specified")
	}

	if fs.BackupBootSector() == 0 {
		t.Error("FAT32 should have backup boot sector specified")
	}

	// Verify boot signature
	if fs.BootSignature() != 0xAA55 {
		t.Errorf("Boot signature should be 0xAA55, got 0x%04X", fs.BootSignature())
	}

	// Verify extended signature
	if fs.Signature() != 0x29 {
		t.Errorf("Extended signature should be 0x29, got 0x%02X", fs.Signature())
	}

	// Verify system identifier (may be trimmed or padded)
	sysId := fs.SystemIdentifier()
	if sysId != "FAT32   " && sysId != "FAT32" {
		t.Errorf("System identifier should be 'FAT32' (with or without padding), got '%s'", sysId)
	}
}

// testBackupBootSectorCompliance verifies the backup boot sector is identical to primary
func testBackupBootSectorCompliance(t *testing.T, writer *FATWriter) {
	fs := writer.layout.Fs()
	storage := writer.layout.storage

	// Read primary boot sector
	primarySector := make([]byte, 512)
	_, err := storage.ReadAt(primarySector, 0)
	if err != nil {
		t.Fatalf("Failed to read primary boot sector: %v", err)
	}

	// Read backup boot sector
	backupOffset := int64(fs.BackupBootSector()) * int64(fs.BytesPerSector())
	backupSector := make([]byte, 512)
	_, err = storage.ReadAt(backupSector, backupOffset)
	if err != nil {
		t.Fatalf("Failed to read backup boot sector: %v", err)
	}

	// Compare sectors
	for i := 0; i < 512; i++ {
		if primarySector[i] != backupSector[i] {
			t.Errorf("Backup boot sector differs from primary at byte %d: primary=0x%02X, backup=0x%02X",
				i, primarySector[i], backupSector[i])
			break
		}
	}
}

// testFSInfoSectorCompliance verifies the FSInfo sector meets specification requirements
func testFSInfoSectorCompliance(t *testing.T, writer *FATWriter) {
	fs := writer.layout.Fs()
	fsInfo := writer.layout.FsInfo()

	if fsInfo == nil {
		t.Fatal("FSInfo sector not accessible")
	}

	// Test FSInfo signatures
	if fsInfo.LeadSignature() != 0x41615252 {
		t.Errorf("FSInfo lead signature should be 0x41615252, got 0x%08X", fsInfo.LeadSignature())
	}

	if fsInfo.Signature() != 0x61417272 {
		t.Errorf("FSInfo signature should be 0x61417272, got 0x%08X", fsInfo.Signature())
	}

	if fsInfo.TrailSignature() != 0xAA550000 {
		t.Errorf("FSInfo trail signature should be 0xAA550000, got 0x%08X", fsInfo.TrailSignature())
	}

	// Test that free cluster count is reasonable
	freeCount := fsInfo.LastFreeClusterCount()
	totalClusters := fs.TotalDataClusters()
	if freeCount == 0xFFFFFFFF {
		t.Log("FSInfo free cluster count is unknown (0xFFFFFFFF) - this is valid")
	} else if freeCount > totalClusters {
		t.Errorf("FSInfo free cluster count (%d) exceeds total clusters (%d)", freeCount, totalClusters)
	}

	// Test that next available cluster hint is reasonable
	nextCluster := fsInfo.AvailableClusterStart()
	if nextCluster != 0xFFFFFFFF && nextCluster < 2 {
		t.Errorf("FSInfo next available cluster (%d) should be >= 2 or 0xFFFFFFFF", nextCluster)
	}
}

// testFAT32FieldValidation verifies FAT32-specific field values
func testFAT32FieldValidation(t *testing.T, writer *FATWriter) {
	fs := writer.layout.Fs()

	// Test reserved sectors (should be >= 32 for FAT32)
	if fs.ReservedSectors() < 32 {
		t.Errorf("FAT32 should have >= 32 reserved sectors, got %d", fs.ReservedSectors())
	}

	// Test FAT version (should be 0.0)
	if fs.FatVersion() != 0x0000 {
		t.Errorf("FAT32 version should be 0x0000, got 0x%04X", fs.FatVersion())
	}

	// Test that root directory entries is 0 for FAT32
	if fs.RootDirectoryEntries() != 0 {
		t.Errorf("FAT32 root directory entries should be 0, got %d", fs.RootDirectoryEntries())
	}

	// Test total sectors field usage
	if fs.TotalSectors() != 0 {
		t.Error("FAT32 should use largeSectorCount instead of totalSectors")
	}

	if fs.LargeSectorCount() == 0 {
		t.Error("FAT32 should have non-zero largeSectorCount")
	}
}
