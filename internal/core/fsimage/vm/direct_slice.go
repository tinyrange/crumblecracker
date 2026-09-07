package vm

// directSliceRegion provides a direct view into a slice of another memory region
// without creating intermediate objects like OffsetRegion + TruncatedRegion chains
type directSliceRegion struct {
	base   MemoryRegion
	offset int64
	size   int64
}

// ReadAt implements MemoryRegion
func (d *directSliceRegion) ReadAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(d, off); err != nil {
		return 0, err
	}

	// Limit read to the slice size
	if int64(len(p)) > d.size-off {
		p = p[:d.size-off]
	}

	return d.base.ReadAt(p, d.offset+off)
}

// WriteAt implements MemoryRegion
func (d *directSliceRegion) WriteAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(d, off); err != nil {
		return 0, err
	}

	// Limit write to the slice size
	if int64(len(p)) > d.size-off {
		p = p[:d.size-off]
	}

	return d.base.WriteAt(p, d.offset+off)
}

// Size implements MemoryRegion
func (d *directSliceRegion) Size() int64 {
	return d.size
}

var (
	_ MemoryRegion = &directSliceRegion{}
)
