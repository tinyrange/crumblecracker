package vm

import (
	"fmt"
	"io"
	"testing"
)

func TestRegionArrayReadAt(t *testing.T) {
	tests := []struct {
		name   string
		offset int64
		length int
		want   string
		err    error
	}{
		{"basic", 0, 11, "Hello World", nil},
		{"partial read", 4, 5, "o Wor", nil},
		{"midway read", 6, 5, "World", nil},
		{"final character", 11, 1, "!", nil},
		{"full read", 0, 12, "Hello World!", nil},
		{"error", -1, 0, "", fmt.Errorf("off < 0")},
		{"EOF", 12, 0, "", io.EOF},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockData := [][]byte{
				[]byte("Hello "),
				[]byte("World"),
				[]byte("!"),
			}

			regions := NewRegionArray[RawRegion]()
			for _, d := range mockData {
				regions.Append(RawRegion(d))
			}

			b := make([]byte, tt.length)
			n, err := regions.ReadAt(b, tt.offset)
			if err != tt.err && err != nil && tt.err != nil && err.Error() != tt.err.Error() {
				t.Errorf("ReadAt(%d, %d): unexpected error: got %v, want %v", tt.offset, tt.length, err, tt.err)
			}

			if tt.err == nil && n != tt.length {
				t.Errorf("ReadAt(%d, %d): unexpected read length: got %d, want %d", tt.offset, tt.length, n, tt.length)
			}

			got := string(b[:n])
			if got != tt.want {
				t.Errorf("ReadAt(%d, %d): unexpected data: got %s, want %s", tt.offset, tt.length, got, tt.want)
			}
		})
	}
}

func TestRegionArrayWriteAt(t *testing.T) {
	tests := []struct {
		name   string
		offset int64
		data   string
		want   []string // Expected data in each region after write
		err    error
	}{
		{"Basic Test", 0, "Hello World!", []string{"Hello ", "World", "!"}, nil},
		{"Writing starting from the second region", 6, "World!", []string{"\x00\x00\x00\x00\x00\x00", "World", "!"}, nil},
		{"Overwriting partial data", 0, "Hi", []string{"Hi\x00\x00\x00\x00", "\x00\x00\x00\x00\x00", "\x00"}, nil},
		{"Offset Less than zero", -1, "", []string{"\x00\x00\x00\x00\x00\x00", "\x00\x00\x00\x00\x00", "\x00"}, fmt.Errorf("off < 0")},
		{"Trying to write past end", 12, "A", []string{"\x00\x00\x00\x00\x00\x00", "\x00\x00\x00\x00\x00", "\x00"}, io.EOF},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockData := [][]byte{
				make([]byte, 6), // Placeholder for "Hello "
				make([]byte, 5), // Placeholder for "World"
				make([]byte, 1), // Placeholder for "!"
			}

			regions := NewRegionArray[RawRegion]()
			for _, d := range mockData {
				regions.Append(RawRegion(d))
			}

			n, err := regions.WriteAt([]byte(tt.data), tt.offset)
			if err != tt.err && (err != nil && tt.err != nil && err.Error() != tt.err.Error()) {
				t.Errorf("WriteAt(`%s`, %d): unexpected error: got %v, want %v", tt.data, tt.offset, err, tt.err)
			}
			if err == nil && n != len(tt.data) {
				t.Errorf("WriteAt(`%s`, %d): unexpected write length: got %d, want %d", tt.data, tt.offset, n, len(tt.data))
			}

			for i, wantData := range tt.want {
				if got := string(regions.Get(i)); got != wantData {
					t.Errorf("Region %d: got `%s`, want `%s` after WriteAt(`%s`, %d)", i, got, wantData, tt.data, tt.offset)
				}
			}
		})
	}
}

func TestRegionArraySize(t *testing.T) {
	mockData := [][]byte{
		[]byte("Hello "),
		[]byte("World"),
		[]byte("!"),
	}

	totalSize := int64(0)
	for _, d := range mockData {
		totalSize += int64(len(d))
	}

	regions := NewRegionArray[RawRegion]()
	for _, d := range mockData {
		regions.Append(RawRegion(d))
	}

	if size := regions.Size(); size != totalSize {
		t.Errorf("Size(): got %d, want %d", size, totalSize)
	}
}
