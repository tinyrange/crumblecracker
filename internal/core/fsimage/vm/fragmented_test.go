package vm

import (
	"bytes"
	"reflect"
	"testing"
)

// TestFragmentedRegionReadAt tests the ReadAt method of fragmentedRegion.
func TestFragmentedRegionReadAt(t *testing.T) {
	fragRegion := newFragmentRegion(16)
	region := RawRegion(make([]byte, 16))
	copy(region, []byte("Hello, World!"))
	_ = fragRegion.mapFragment(&region, 0)

	buff := make([]byte, 5)
	n, err := fragRegion.ReadAt(buff, 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("ReadAt = %d, want 5", n)
	}
	expected := []byte("Hello")
	if !reflect.DeepEqual(buff, expected) {
		t.Errorf("ReadAt buffer = %v, want %v", buff, expected)
	}
}

// TestFragmentedRegionWriteAt tests the WriteAt method of fragmentedRegion.
func TestFragmentedRegionWriteAt(t *testing.T) {
	fragRegion := newFragmentRegion(16)
	data := []byte("Goodbye!")
	n, err := fragRegion.WriteAt(data, 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("WriteAt = %d, want %d", n, len(data))
	}

	buff := make([]byte, 8)
	n, err = fragRegion.ReadAt(buff, 0)
	if err != nil {
		t.Errorf("unexpected error during read: %v", err)
	}
	if n != 8 {
		t.Errorf("ReadAt after WriteAt = %d, want 8", n)
	}
	if !bytes.Equal(buff, data) {
		t.Errorf("ReadAt buffer after WriteAt = %v, want %v", buff, data)
	}
}

// TestFragmentedRegionMapFragment tests the mapFragment method of fragmentedRegion.
func TestFragmentedRegionMapFragment(t *testing.T) {
	fragRegion := newFragmentRegion(32)
	addRegion := RawRegion([]byte("Gophers!"))
	err := fragRegion.mapFragment(&addRegion, 8)
	if err != nil {
		t.Errorf("mapFragment error: %v", err)
	}

	buff := make([]byte, 8)
	n, err := fragRegion.ReadAt(buff, 8)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if n != len(buff) {
		t.Errorf("ReadAt = %d, want %d", n, len(buff))
	}

	t.Logf("buff %+v", buff)

	expected := []byte("Gophers!")
	if !reflect.DeepEqual(buff, expected) {
		t.Errorf("ReadAt buffer after mapFragment = %v, want %v", buff, expected)
	}
}

func TestFragmentedRegionRejectsInvalidMappingsWithoutMutation(t *testing.T) {
	region := newFragmentRegion(16)
	want := make([]byte, 16)
	fragment := RawRegion([]byte{1, 2})

	for _, test := range []struct {
		name     string
		fragment MemoryRegion
		offset   int64
	}{
		{name: "nil fragment", fragment: nil, offset: 0},
		{name: "negative offset", fragment: &fragment, offset: -1},
		{name: "past end", fragment: &fragment, offset: 15},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := region.mapFragment(test.fragment, test.offset); err == nil {
				t.Fatal("mapFragment succeeded")
			}
			got := make([]byte, len(want))
			if _, err := region.ReadAt(got, 0); err != nil {
				t.Fatalf("read region: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("region bytes = %x, want %x", got, want)
			}
		})
	}
}

func FuzzFragmentedRegionReplacement(f *testing.F) {
	f.Add(byte(8), byte(8), byte(4), byte(8))
	f.Add(byte(8), byte(8), byte(4), byte(20))
	f.Add(byte(4), byte(20), byte(8), byte(8))
	f.Add(byte(0), byte(64), byte(0), byte(64))

	f.Fuzz(func(t *testing.T, firstOffset, firstLength, secondOffset, secondLength byte) {
		const pageSize = 64
		region := newFragmentRegion(pageSize)
		want := make([]byte, pageSize)
		mapReplacementSpan(t, region, want, firstOffset, firstLength, 0x11)
		mapReplacementSpan(t, region, want, secondOffset, secondLength, 0x22)
		assertFragmentLayout(t, region, pageSize)

		got := make([]byte, pageSize)
		if _, err := region.ReadAt(got, 0); err != nil {
			t.Fatalf("read replaced region: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("replaced bytes = %x, want %x", got, want)
		}
	})
}

func mapReplacementSpan(t *testing.T, region *fragmentedRegion, want []byte, rawOffset, rawLength byte, value byte) {
	t.Helper()
	offset := int(rawOffset) % len(want)
	length := int(rawLength) % (len(want) - offset + 1)
	data := RawRegion(bytes.Repeat([]byte{value}, length))
	if err := region.mapFragment(&data, int64(offset)); err != nil {
		t.Fatalf("map offset %d length %d: %v", offset, length, err)
	}
	copy(want[offset:offset+length], data)
}

func assertFragmentLayout(t *testing.T, region *fragmentedRegion, size int64) {
	t.Helper()
	end := int64(0)
	for i, fragment := range region.fragments {
		if fragment.size <= 0 {
			t.Fatalf("fragment %d has size %d", i, fragment.size)
		}
		if fragment.start() != end {
			t.Fatalf("fragment %d starts at %d after end %d", i, fragment.start(), end)
		}
		end = fragment.end()
	}
	if end != size {
		t.Fatalf("fragment coverage ends at %d, want %d", end, size)
	}
}
