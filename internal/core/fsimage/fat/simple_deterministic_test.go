package fat

import (
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

// TestVolumeSerialDeterminism tests that volume serial numbers work deterministically
func TestVolumeSerialDeterminism(t *testing.T) {
	const size = 1024 * 1024 * 1024 // 1GB

	// Test with specific volume serial
	customSerial := uint32(0xABCDEF00)
	fatEpoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

	config := &DeterministicConfig{
		FixedTimestamp:       &fatEpoch,
		SortDirectoryEntries: true,
		VolumeSerial:         &customSerial,
	}

	// Create filesystem with custom serial
	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystemWithConfig(vmFS, size, config)
	if err != nil {
		t.Fatalf("Failed to create filesystem: %v", err)
	}

	// Check that volume serial was set correctly
	fs := writer.layout.Fs()
	actualSerial := fs.VolumeId()

	if actualSerial != customSerial {
		t.Errorf("Volume serial mismatch: expected 0x%08X, got 0x%08X", customSerial, actualSerial)
	}
}
