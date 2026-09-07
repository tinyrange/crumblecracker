package vm

import (
	"fmt"
	"io"
)

type FileRegion struct {
	f         io.ReaderAt
	totalSize int64
}

func (f *FileRegion) String() string {
	return fmt.Sprintf("FileRegion{file=%+v, totalSize=%d}", f.f, f.totalSize)
}

// ReadAt implements MemoryRegion.
func (f *FileRegion) ReadAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(f, off); err != nil {
		return 0, err
	}

	n, err = f.f.ReadAt(p, off)
	if err == io.EOF {
		err = nil
	}
	return
}

// Size implements MemoryRegion.
func (f *FileRegion) Size() int64 {
	return f.totalSize
}

// WriteAt implements MemoryRegion.
func (f *FileRegion) WriteAt(p []byte, off int64) (n int, err error) {
	if err := boundsCheck(f, off); err != nil {
		return 0, err
	}

	return 0, fmt.Errorf("region is read only")
}

var (
	_ MemoryRegion = &FileRegion{}
)
