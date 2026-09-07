package vm

import "io"

type BitmapRegion []byte

// ReadAt implements MemoryRegion.
func (r *BitmapRegion) ReadAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(r, off); err != nil {
		return 0, err
	}
	return copy(p, (*r)[off:]), nil
}

// Size implements MemoryRegion.
func (r *BitmapRegion) Size() int64 {
	return int64(len(*r))
}

// WriteAt implements MemoryRegion.
func (r *BitmapRegion) WriteAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(r, off); err != nil {
		return 0, err
	}
	return copy((*r)[off:], p), nil
}

func (r *BitmapRegion) Get(i uint64) (bool, error) {
	if i >= uint64(r.Size()*8) {
		return false, io.EOF
	}

	index, pos := i/8, i%8

	return ((*r)[index]>>pos)&0x01 != 0, nil
}

func (r *BitmapRegion) Set(i uint64, value bool) error {
	if i >= uint64(r.Size()*8) {
		return io.EOF
	}

	index, pos := i/8, i%8

	if value {
		(*r)[index] |= 0x01 << pos
	} else {
		(*r)[index] &^= 0x01 << pos
	}

	return nil
}

func (r *BitmapRegion) SetAll(value bool) error {
	for i := range *r {
		if value {
			(*r)[i] = 0xff
		} else {
			(*r)[i] = 0x00
		}
	}
	return nil
}

// SetRange sets a range of bits efficiently using bulk operations
func (r *BitmapRegion) SetRange(start, end uint64, value bool) error {
	if start > end {
		return nil
	}

	maxBits := uint64(r.Size() * 8)
	if start >= maxBits {
		return io.EOF
	}
	if end > maxBits {
		end = maxBits
	}

	// Handle single bit case
	if start == end-1 {
		return r.Set(start, value)
	}

	startByte := start / 8
	endByte := (end - 1) / 8
	startBit := start % 8
	endBit := (end - 1) % 8

	fillByte := byte(0x00)
	if value {
		fillByte = 0xff
	}

	if startByte == endByte {
		// Range is within a single byte
		mask := byte(0)
		for i := startBit; i <= endBit; i++ {
			mask |= 1 << i
		}

		if value {
			(*r)[startByte] |= mask
		} else {
			(*r)[startByte] &^= mask
		}
		return nil
	}

	// Handle partial start byte
	if startBit != 0 {
		mask := byte(0)
		for i := startBit; i < 8; i++ {
			mask |= 1 << i
		}

		if value {
			(*r)[startByte] |= mask
		} else {
			(*r)[startByte] &^= mask
		}
		startByte++
	}

	// Handle partial end byte
	if endBit != 7 {
		mask := byte(0)
		for i := uint64(0); i <= endBit; i++ {
			mask |= 1 << i
		}

		if value {
			(*r)[endByte] |= mask
		} else {
			(*r)[endByte] &^= mask
		}
	} else {
		endByte++
	}

	// Fill complete bytes in between
	for i := startByte; i < endByte; i++ {
		(*r)[i] = fillByte
	}

	return nil
}

// FindFirstClear finds the first clear (0) bit in the range [start, end)
// Returns the bit index and true if found, or 0 and false if not found
func (r *BitmapRegion) FindFirstClear(start, end uint64) (uint64, bool) {
	maxBits := uint64(r.Size() * 8)
	if start >= maxBits {
		return 0, false
	}
	if end > maxBits {
		end = maxBits
	}
	if start >= end {
		return 0, false
	}

	// Start from the byte containing the start bit
	startByte := start / 8
	endByte := (end - 1) / 8

	// Check partial first byte
	if start%8 != 0 {
		b := (*r)[startByte]
		startBit := start % 8

		// Check bits from startBit to end of byte
		for bit := startBit; bit < 8; bit++ {
			bitIndex := startByte*8 + bit
			if bitIndex >= end {
				break
			}
			if (b>>bit)&1 == 0 {
				return bitIndex, true
			}
		}
		startByte++
	}

	// Check complete bytes
	for byteIdx := startByte; byteIdx <= endByte; byteIdx++ {
		b := (*r)[byteIdx]

		// If byte is not all 1s (0xFF), it has at least one 0 bit
		if b != 0xFF {
			// Find first 0 bit in this byte
			for bit := uint64(0); bit < 8; bit++ {
				bitIndex := byteIdx*8 + bit
				if bitIndex >= end {
					return 0, false
				}
				if bitIndex < start {
					continue
				}
				if (b>>bit)&1 == 0 {
					return bitIndex, true
				}
			}
		}
	}

	return 0, false
}

// FindFirstSet finds the first set (1) bit in the range [start, end)
// Returns the bit index and true if found, or 0 and false if not found
func (r *BitmapRegion) FindFirstSet(start, end uint64) (uint64, bool) {
	maxBits := uint64(r.Size() * 8)
	if start >= maxBits {
		return 0, false
	}
	if end > maxBits {
		end = maxBits
	}
	if start >= end {
		return 0, false
	}

	// Start from the byte containing the start bit
	startByte := start / 8
	endByte := (end - 1) / 8

	// Check partial first byte
	if start%8 != 0 {
		b := (*r)[startByte]
		startBit := start % 8

		// Check bits from startBit to end of byte
		for bit := startBit; bit < 8; bit++ {
			bitIndex := startByte*8 + bit
			if bitIndex >= end {
				break
			}
			if (b>>bit)&1 == 1 {
				return bitIndex, true
			}
		}
		startByte++
	}

	// Check complete bytes
	for byteIdx := startByte; byteIdx <= endByte; byteIdx++ {
		b := (*r)[byteIdx]

		// If byte is not all 0s (0x00), it has at least one 1 bit
		if b != 0x00 {
			// Find first 1 bit in this byte
			for bit := uint64(0); bit < 8; bit++ {
				bitIndex := byteIdx*8 + bit
				if bitIndex >= end {
					return 0, false
				}
				if bitIndex < start {
					continue
				}
				if (b>>bit)&1 == 1 {
					return bitIndex, true
				}
			}
		}
	}

	return 0, false
}

var (
	_ MemoryRegion = &BitmapRegion{}
)

func NewBitmap(size uint64) *BitmapRegion {
	ret := make(BitmapRegion, size/8)

	return &ret
}
