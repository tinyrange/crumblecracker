package vm

import (
	"testing"
)

func TestDirectSliceRegion(t *testing.T) {
	// Create a test source region
	source := make(RawRegion, 100)
	for i := range source {
		source[i] = byte(i)
	}

	// Create a slice region
	slice := &directSliceRegion{
		base:   &source,
		offset: 20,
		size:   30,
	}

	// Test size
	if slice.Size() != 30 {
		t.Errorf("Expected size 30, got %d", slice.Size())
	}

	// Test reading
	buf := make([]byte, 10)
	n, err := slice.ReadAt(buf, 5)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 10 {
		t.Errorf("Expected to read 10 bytes, got %d", n)
	}

	// Verify data (offset 20 + read offset 5 = byte value 25)
	for i, b := range buf {
		expected := byte(25 + i)
		if b != expected {
			t.Errorf("At index %d: expected %d, got %d", i, expected, b)
		}
	}
}

func TestMapSlice(t *testing.T) {
	vm := NewVirtualMemory(1024, 64) // 1KB with 64-byte pages

	// Create source data
	source := make(RawRegion, 200)
	for i := range source {
		source[i] = byte(i)
	}

	// Map a slice of the source data
	err := vm.MapSlice(&source, 50, 100, 128)
	if err != nil {
		t.Fatalf("MapSlice failed: %v", err)
	}

	// Verify the data was mapped correctly
	buf := make([]byte, 50)
	n, err := vm.ReadAt(buf, 128)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 50 {
		t.Errorf("Expected to read 50 bytes, got %d", n)
	}

	// Verify data matches source slice
	for i, b := range buf {
		expected := byte(50 + i)
		if b != expected {
			t.Errorf("At index %d: expected %d, got %d", i, expected, b)
		}
	}
}
