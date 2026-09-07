package virgl

import (
	"fmt"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

type transferBacking []byte

func (b transferBacking) Size() uint64 { return uint64(len(b)) }

func (b transferBacking) ReadAt(offset uint64, destination []byte) error {
	end := offset + uint64(len(destination))
	if end < offset || end > uint64(len(b)) {
		return fmt.Errorf("read %d..%d exceeds %d", offset, end, len(b))
	}
	copy(destination, b[offset:end])
	return nil
}

func (b transferBacking) WriteAt(offset uint64, source []byte) error {
	end := offset + uint64(len(source))
	if end < offset || end > uint64(len(b)) {
		return fmt.Errorf("write %d..%d exceeds %d", offset, end, len(b))
	}
	copy(b[offset:end], source)
	return nil
}

func TestZeroTextureTransferStrideUsesFullMipWidth(t *testing.T) {
	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 4, Height: 4, Depth: 1, ArraySize: 1,
	}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{X: 1, Y: 1, Width: 2, Height: 2, Depth: 1},
	}

	got, err := transferDataSize(description, transfer)
	if err != nil {
		t.Fatal(err)
	}
	// Two RGBA pixels from the first row, one full four-pixel image stride,
	// then two pixels from the final row.
	if want := uint64(2*4 + 4*4); got != want {
		t.Fatalf("zero-stride partial texture transfer size = %d, want %d", got, want)
	}
}

func TestPartialTextureTransferGathersRowsFromFullGuestStride(t *testing.T) {
	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 4, Height: 2, Depth: 1, ArraySize: 1,
	}
	backing := transferBacking{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
	}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{X: 1, Width: 2, Height: 2, Depth: 1},
		Offset:     4,
		Backing:    backing,
	}

	data, normalized, err := stageTransferData(description, transfer)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		4, 5, 6, 7, 8, 9, 10, 11,
		20, 21, 22, 23, 24, 25, 26, 27,
	}
	if string(data) != string(want) {
		t.Fatalf("staged partial texture rows = %v, want %v", data, want)
	}
	if normalized.Offset != 0 || normalized.Stride != 8 || normalized.LayerStride != 16 {
		t.Fatalf("normalized transfer = offset %d stride %d layer stride %d, want 0, 8, 16",
			normalized.Offset, normalized.Stride, normalized.LayerStride)
	}
}

func TestStencilTransferPreservesOneBytePixelsAndGuestRows(t *testing.T) {
	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatS8UInt,
		Width: 4, Height: 2, Depth: 1, ArraySize: 1,
	}
	backing := transferBacking{0, 1, 2, 3, 4, 5, 6, 7}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{X: 1, Width: 2, Height: 2, Depth: 1},
		Offset:     1,
		Backing:    backing,
	}

	data, normalized, err := stageTransferData(description, transfer)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := data, []byte{1, 2, 5, 6}; string(got) != string(want) {
		t.Fatalf("staged stencil rows = %v, want %v", got, want)
	}
	if normalized.Stride != 2 || normalized.LayerStride != 4 {
		t.Fatalf("normalized stencil transfer stride/layer stride = %d/%d, want 2/4",
			normalized.Stride, normalized.LayerStride)
	}
}

func TestModernTextureTransfersUseProtocolFormatByteWidths(t *testing.T) {
	tests := []struct {
		name          string
		format        uint32
		bytesPerPixel uint64
	}{
		{"r8", virglFormatR8UNorm, 1},
		{"rg8", virglFormatR8G8UNorm, 2},
		{"r16f", virglFormatR16Float, 2},
		{"rg16f", virglFormatR16G16Float, 4},
		{"rgba16f", virglFormatR16G16B16A16Float, 8},
		{"r32f", virglFormatR32Float, 4},
		{"rg32f", virglFormatR32G32Float, 8},
		{"rgba32f", virglFormatR32G32B32A32Float, 16},
		{"rgba32ui", virglFormatR32G32B32A32UInt, 16},
		{"rgba32i", virglFormatR32G32B32A32SInt, 16},
		{"z32f-s8", virglFormatZ32FloatS8X24UInt, 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			description := virtio.GPUResource3D{
				ID: 1, Target: 2, Format: test.format,
				Width: 4, Height: 2, Depth: 1, ArraySize: 1,
			}
			transfer := virtio.GPUTransfer3D{
				ResourceID: description.ID,
				Box:        virtio.GPUBox{X: 1, Width: 2, Height: 2, Depth: 1},
			}
			layout, err := describeTextureTransfer(description, transfer)
			if err != nil {
				t.Fatal(err)
			}
			if want := uint64(2) * test.bytesPerPixel; layout.rowBytes != want {
				t.Fatalf("row bytes = %d, want %d", layout.rowBytes, want)
			}
			if want := uint64(description.Width) * test.bytesPerPixel; layout.stride != want {
				t.Fatalf("full guest stride = %d, want %d", layout.stride, want)
			}
		})
	}
}

func TestArrayTextureTransferPreservesLayerStride(t *testing.T) {
	description := virtio.GPUResource3D{
		ID: 1, Target: 7, Format: 67,
		Width: 2, Height: 2, Depth: 1, ArraySize: 4,
	}
	backing := make(transferBacking, 48)
	for index := range backing {
		backing[index] = byte(index)
	}
	transfer := virtio.GPUTransfer3D{
		ResourceID:  description.ID,
		Box:         virtio.GPUBox{X: 1, Width: 1, Height: 2, Depth: 2},
		Offset:      4,
		Stride:      8,
		LayerStride: 24,
		Backing:     backing,
	}

	data, normalized, err := stageTransferData(description, transfer)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		4, 5, 6, 7, 12, 13, 14, 15,
		28, 29, 30, 31, 36, 37, 38, 39,
	}
	if string(data) != string(want) {
		t.Fatalf("staged array texture layers = %v, want %v", data, want)
	}
	if normalized.Offset != 0 || normalized.Stride != 4 || normalized.LayerStride != 8 {
		t.Fatalf("normalized array transfer = offset %d stride %d layer stride %d, want 0, 4, 8",
			normalized.Offset, normalized.Stride, normalized.LayerStride)
	}

	destination := make(transferBacking, len(backing))
	transfer.Backing = destination
	if err := commitTransferFromHost(description, transfer, data); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{4, 12, 28, 36} {
		if got := destination[offset : offset+4]; string(got) != string(backing[offset:offset+4]) {
			t.Fatalf("committed array bytes at %d = %v, want %v", offset, got, backing[offset:offset+4])
		}
	}
}
