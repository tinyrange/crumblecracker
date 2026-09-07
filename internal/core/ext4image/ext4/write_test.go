package ext4

import (
	"errors"
	"io"
	"testing"

	vm "github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

type writerAtFunc func([]byte, int64) (int, error)

func (f writerAtFunc) WriteAt(p []byte, off int64) (int, error) {
	return f(p, off)
}

func TestWriteFullAtReturnsBackingStoreFailures(t *testing.T) {
	backingErr := errors.New("backing store unavailable")
	for _, test := range []struct {
		name    string
		writer  io.WriterAt
		wantErr error
	}{
		{
			name: "write error",
			writer: writerAtFunc(func([]byte, int64) (int, error) {
				return 0, backingErr
			}),
			wantErr: backingErr,
		},
		{
			name: "short write",
			writer: writerAtFunc(func(p []byte, _ int64) (int, error) {
				return len(p) - 1, nil
			}),
			wantErr: io.ErrShortWrite,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := writeFullAt(test.writer, []byte("metadata"), 104)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("writeFullAt error = %v, want wrapped %v", err, test.wantErr)
			}
		})
	}
}

func TestBlockGroupAllocateBlocksUsesFinalFreeBlock(t *testing.T) {
	bitmap := vm.NewBitmap(8)
	if err := bitmap.Set(0, true); err != nil {
		t.Fatalf("reserve block zero: %v", err)
	}
	desc := &BlockGroupDescriptor{}
	desc.SetFreeBlocksCount(1)
	group := &BlockGroup{
		num:            0,
		desc:           desc,
		blockBitmap:    bitmap,
		blockCount:     2,
		firstFreeBlock: 1,
	}

	extent, err := group.allocateBlocks(1)
	if err != nil {
		t.Fatalf("allocate final free block: %v", err)
	}
	if extent.StartBlock != 1 || extent.Length != 1 {
		t.Fatalf("allocated extent = %+v", extent)
	}
	used, err := bitmap.Get(1)
	if err != nil {
		t.Fatalf("read allocated block: %v", err)
	}
	if !used || desc.FreeBlocksCount() != 0 {
		t.Fatalf("allocated bit = %t, free blocks = %d", used, desc.FreeBlocksCount())
	}
}
