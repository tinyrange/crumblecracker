package vm

import (
	"bytes"
	"testing"
)

func TestPaddedRegion_ReadAt(t *testing.T) {
	tests := []struct {
		name    string
		offset  int64
		size    int
		want    []byte
		wantErr bool
	}{
		{"read within bounds", 0, 5, []byte{1, 2, 3, 4, 5}, false},
		{"read with padding", 0, 7, []byte{1, 2, 3, 4, 5, 0, 0}, false},
		{"read fully padded", 5, 0, []byte{}, false},
		{"read out of padded bounds", 10, 1, nil, true},
		{"negative offset", -1, 5, nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := []byte{1, 2, 3, 4, 5}
			region := NewPaddedRegion(RawRegion(source), 10)

			buffer := make([]byte, tc.size)
			n, err := region.ReadAt(buffer, tc.offset)

			if (err != nil) != tc.wantErr {
				t.Errorf("ReadAt() error = %v, wantErr %v", err, tc.wantErr)
				return
			}

			if !tc.wantErr && n != len(tc.want) {
				t.Errorf("ReadAt() got = %v, want %v", n, len(tc.want))
			}

			if !bytes.Equal(buffer[:n], tc.want) {
				t.Errorf("ReadAt() got1 = %v, want %v", buffer[:n], tc.want)
			}
		})
	}
}

func TestPaddedRegion_WriteAt(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		offset     int64
		wantN      int
		wantRegion []byte
		wantErr    bool
	}{
		{"write within bounds", []byte{10, 11}, 1, 2, []byte{1, 10, 11, 4, 5}, false},
		{"write with expansion", []byte{20, 21, 22}, 3, 3, []byte{1, 2, 3, 20, 21}, false},
		{"write out of bounds", []byte{30}, 10, 0, nil, true},
		{"negative offset", []byte{40}, -1, 0, nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := []byte{1, 2, 3, 4, 5}
			region := NewPaddedRegion(RawRegion(source), 10)

			n, err := region.WriteAt(tc.data, tc.offset)

			if (err != nil) != tc.wantErr {
				t.Errorf("WriteAt() error = %v, wantErr %v", err, tc.wantErr)
				return
			}

			if n != tc.wantN {
				t.Errorf("WriteAt() got = %d, want %d", n, tc.wantN)
			}

			if tc.wantRegion != nil && !bytes.Equal([]byte(region.Region.(RawRegion)), tc.wantRegion) {
				t.Errorf("WriteAt() got1 = %v, want %v", []byte(region.Region.(RawRegion)), tc.wantRegion)
			}
		})
	}
}

func TestPaddedRegion_Size(t *testing.T) {
	region := NewPaddedRegion(RawRegion([]byte{1, 2, 3, 4, 5}), 10)

	got := region.Size()
	want := int64(10)

	if got != want {
		t.Errorf("Size() = %v, want %v", got, want)
	}
}
