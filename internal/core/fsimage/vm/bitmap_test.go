package vm

import "testing"

func TestBitmapRegion_ReadAt(t *testing.T) {
	tests := []struct {
		name    string
		offset  int64
		want    []byte
		wantN   int
		wantErr bool
	}{
		{"read from start", 0, []byte{0xab}, 1, false},
		{"read with offset", 1, []byte{0xcd}, 1, false},
		{"read beyond size", 3, []byte{0x00}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bitmap := BitmapRegion([]byte{0xab, 0xcd, 0xef})

			buf := make([]byte, 1)
			gotN, err := bitmap.ReadAt(buf, tt.offset)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReadAt() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotN != tt.wantN {
				t.Errorf("ReadAt() gotN = %v, want %v", gotN, tt.wantN)
			}
			if string(buf) != string(tt.want) {
				t.Errorf("ReadAt() got = %v, want %v", buf, tt.want)
			}
		})
	}
}

func TestBitmapRegion_WriteAt(t *testing.T) {
	tests := []struct {
		name    string
		offset  int64
		data    []byte
		want    []byte
		wantN   int
		wantErr bool
	}{
		{"write at start", 0, []byte{0xff}, []byte{0xff, 0x00, 0x00}, 1, false},
		{"write at offset", 1, []byte{0xee}, []byte{0x00, 0xee, 0x00}, 1, false},
		{"write beyond size", 3, []byte{0xdd}, nil, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bitmap := &BitmapRegion{0x00, 0x00, 0x00}

			gotN, err := bitmap.WriteAt(tt.data, tt.offset)
			if (err != nil) != tt.wantErr {
				t.Errorf("WriteAt() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotN != tt.wantN {
				t.Errorf("WriteAt() gotN = %v, want %v", gotN, tt.wantN)
			}
			if tt.want != nil && string(*bitmap) != string(tt.want) {
				t.Errorf("WriteAt() got = %v, want %v", *bitmap, tt.want)
			}
		})
	}
}

func TestBitmapRegion_Size(t *testing.T) {
	bitmap := &BitmapRegion{0x00, 0x01, 0x02, 0x03}
	wantSize := int64(4)
	if gotSize := bitmap.Size(); gotSize != wantSize {
		t.Errorf("Size() = %v, want %v", gotSize, wantSize)
	}
}

func TestBitmapRegion_Get(t *testing.T) {
	tests := []struct {
		name    string
		i       uint64
		want    bool
		wantErr bool
	}{
		{"bit 0 - false", 0, true, false},
		{"bit 1 - true", 1, false, false},
		{"bit 7 - false", 7, false, false},
		{"out of bounds", 8, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bitmap := &BitmapRegion{0x55} // In binary: 01010101

			got, err := bitmap.Get(tt.i)
			if (err != nil) != tt.wantErr {
				t.Errorf("Get() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Get() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBitmapRegion_Set(t *testing.T) {
	tests := []struct {
		name    string
		i       uint64
		value   bool
		want    []byte
		wantErr bool
	}{
		{"set false", 0, false, []byte{0x00}, false},
		{"set true", 1, true, []byte{0x02}, false},
		{"out of bounds", 8, true, []byte{0x00}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bitmap := &BitmapRegion{0x00}

			err := bitmap.Set(tt.i, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("Set() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && string(*bitmap) != string(tt.want) {
				t.Errorf("Set() got = %v, want %v", *bitmap, tt.want)
			}
		})
	}
}

func TestBitmapRegion_SetAll(t *testing.T) {
	tests := []struct {
		name  string
		value bool
		want  []byte
	}{
		{"set all to false", false, []byte{0x00, 0x00}},
		{"set all to true", true, []byte{0xff, 0xff}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bitmap := &BitmapRegion{0x00, 0x00}

			err := bitmap.SetAll(tt.value)
			if err != nil {
				t.Errorf("SetAll() error = %v", err)
			}
			if string(*bitmap) != string(tt.want) {
				t.Errorf("SetAll() got = %v, want %v", *bitmap, tt.want)
			}
		})
	}
}
