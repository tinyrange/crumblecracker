//go:build darwin

package virgl

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestModernTextureFormatsRoundTripNativeStorage(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	float32Bytes := func(values ...float32) []byte {
		result := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
		}
		return result
	}
	uint32Bytes := func(value uint32) []byte {
		result := make([]byte, 4)
		binary.LittleEndian.PutUint32(result, value)
		return result
	}

	tests := []struct {
		name   string
		format uint32
		pixel  []byte
	}{
		{"r8-unorm", virglFormatR8UNorm, []byte{0x7f}},
		{"rg8-unorm", virglFormatR8G8UNorm, []byte{0x20, 0xe0}},
		{"rgb8-unorm", virglFormatR8G8B8UNorm, []byte{0x20, 0x80, 0xe0}},
		{"r16-unorm", virglFormatR16UNorm, []byte{0x34, 0x12}},
		{"rg16-unorm", virglFormatR16G16UNorm, []byte{0x34, 0x12, 0xcd, 0xab}},
		{"rgb16-unorm", virglFormatR16G16B16UNorm, []byte{0x34, 0x12, 0xcd, 0xab, 0x55, 0x55}},
		{"rgba16-unorm", virglFormatR16G16B16A16UNorm, []byte{0x34, 0x12, 0xcd, 0xab, 0x55, 0x55, 0xff, 0xff}},
		{"r8-snorm", virglFormatR8SNorm, []byte{0xc0}},
		{"rg8-snorm", virglFormatR8G8SNorm, []byte{0xc0, 0x40}},
		{"rgb8-snorm", virglFormatR8G8B8SNorm, []byte{0xc0, 0x40, 0x20}},
		{"rgba8-snorm", virglFormatR8G8B8A8SNorm, []byte{0xc0, 0x40, 0x20, 0x7f}},
		{"r16-snorm", virglFormatR16SNorm, []byte{0x00, 0xc0}},
		{"rg16-snorm", virglFormatR16G16SNorm, []byte{0x00, 0xc0, 0x00, 0x40}},
		{"rgb16-snorm", virglFormatR16G16B16SNorm, []byte{0x00, 0xc0, 0x00, 0x40, 0x00, 0x20}},
		{"rgba16-snorm", virglFormatR16G16B16A16SNorm, []byte{0x00, 0xc0, 0x00, 0x40, 0x00, 0x20, 0xff, 0x7f}},
		{"r16-float", virglFormatR16Float, []byte{0x00, 0x38}},
		{"rg16-float", virglFormatR16G16Float, []byte{0x00, 0x38, 0x00, 0x3c}},
		{"rgb16-float", virglFormatR16G16B16Float, []byte{0x00, 0x38, 0x00, 0x3c, 0x00, 0x34}},
		{"rgba16-float", virglFormatR16G16B16A16Float, []byte{0x00, 0x38, 0x00, 0x3c, 0x00, 0x34, 0x00, 0x00}},
		{"r32-float", virglFormatR32Float, float32Bytes(0.5)},
		{"rg32-float", virglFormatR32G32Float, float32Bytes(0.5, 1)},
		{"rgb32-float", virglFormatR32G32B32Float, float32Bytes(0.5, 1, 0.25)},
		{"rgba32-float", virglFormatR32G32B32A32Float, float32Bytes(0.5, 1, 0.25, 0)},
		{"rgba32-uint", virglFormatR32G32B32A32UInt, []byte{1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 4, 0, 0, 0}},
		{"rgba32-sint", virglFormatR32G32B32A32SInt, []byte{0xff, 0xff, 0xff, 0xff, 2, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff, 4, 0, 0, 0}},
		{"r8-uint", virglFormatR8UInt, []byte{7}},
		{"rg8-sint", virglFormatR8G8SInt, []byte{0xff, 2}},
		{"rgb8-uint", virglFormatR8G8B8UInt, []byte{1, 2, 3}},
		{"rgba8-sint", virglFormatR8G8B8A8SInt, []byte{0xff, 2, 0xfd, 4}},
		{"r16-uint", virglFormatR16UInt, []byte{1, 2}},
		{"rg16-sint", virglFormatR16G16SInt, []byte{0xff, 0xff, 2, 0}},
		{"rgb16-uint", virglFormatR16G16B16UInt, []byte{1, 0, 2, 0, 3, 0}},
		{"rgba16-sint", virglFormatR16G16B16A16SInt, []byte{0xff, 0xff, 2, 0, 0xfd, 0xff, 4, 0}},
		{"r32-uint", virglFormatR32UInt, []byte{1, 0, 0, 0}},
		{"rg32-sint", virglFormatR32G32SInt, []byte{0xff, 0xff, 0xff, 0xff, 2, 0, 0, 0}},
		{"rgb32-uint", virglFormatR32G32B32UInt, []byte{1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0}},
		{"rgb8-srgb", virglFormatR8G8B8SRGB, []byte{0x20, 0x40, 0x80}},
		{"abgr8-srgb", virglFormatA8B8G8R8SRGB, []byte{0x20, 0x40, 0x80, 0xff}},
		{"bgra8-srgb", virglFormatB8G8R8A8SRGB, []byte{0x20, 0x40, 0x80, 0xff}},
		{"bgrx8-srgb", virglFormatB8G8R8X8SRGB, []byte{0x20, 0x40, 0x80, 0xff}},
		{"rgba8-srgb", virglFormatR8G8B8A8SRGB, []byte{0x20, 0x40, 0x80, 0xff}},
		{"rgbx8-srgb", virglFormatR8G8B8X8SRGB, []byte{0x20, 0x40, 0x80, 0xff}},
		{"rgb10-a2-unorm", virglFormatR10G10B10A2UNorm, uint32Bytes(0x955aa155)},
		{"bgr10-a2-unorm", virglFormatB10G10R10A2UNorm, uint32Bytes(0x955aa155)},
		{"rgb10-a2-uint", virglFormatR10G10B10A2UInt, uint32Bytes(0xd55aa155)},
		{"bgr10-a2-uint", virglFormatB10G10R10A2UInt, uint32Bytes(0xd55aa155)},
		{"r11g11b10-float", virglFormatR11G11B10Float, uint32Bytes(0x781e03c0)},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			description := virtio.GPUResource3D{
				ID: uint32(index + 1), Target: 2, Format: test.format,
				Width: 1, Height: 1, Depth: 1, ArraySize: 1,
			}
			if err := host.createResource(description); err != nil {
				t.Fatal(err)
			}
			transfer := virtio.GPUTransfer3D{
				ResourceID: description.ID,
				Stride:     uint32(len(test.pixel)),
				Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
			}
			if err := host.transferToHost(&resource{description: description, data: append([]byte(nil), test.pixel...)}, transfer); err != nil {
				t.Fatal(err)
			}
			readback := &resource{description: description, data: make([]byte, len(test.pixel))}
			if err := host.transferFromHost(readback, transfer); err != nil {
				t.Fatal(err)
			}
			if string(readback.data) != string(test.pixel) {
				t.Fatalf("native round trip = %v, want %v", readback.data, test.pixel)
			}
		})
	}
}

func TestIntegerMultisampleFormatsUseLayeredNativeStorage(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	formats := []uint32{
		virglFormatR8UInt,
		virglFormatR8G8UInt,
		virglFormatR8G8B8UInt,
		virglFormatR8G8B8A8UInt,
		virglFormatR8SInt,
		virglFormatR8G8SInt,
		virglFormatR8G8B8SInt,
		virglFormatR8G8B8A8SInt,
		virglFormatR16UInt,
		virglFormatR16G16UInt,
		virglFormatR16G16B16UInt,
		virglFormatR16G16B16A16UInt,
		virglFormatR16SInt,
		virglFormatR16G16SInt,
		virglFormatR16G16B16SInt,
		virglFormatR16G16B16A16SInt,
		virglFormatR32UInt,
		virglFormatR32G32UInt,
		virglFormatR32G32B32UInt,
		virglFormatR32G32B32A32UInt,
		virglFormatR32SInt,
		virglFormatR32G32SInt,
		virglFormatR32G32B32SInt,
		virglFormatR32G32B32A32SInt,
		virglFormatR10G10B10A2UInt,
		virglFormatB10G10R10A2UInt,
	}
	for index, format := range formats {
		description := virtio.GPUResource3D{
			ID: uint32(index + 1), Target: 2, Format: format,
			Width: 1, Height: 1, Depth: 1, ArraySize: 1, Samples: 4,
		}
		if err := host.createResource(description); err != nil {
			t.Fatalf("format %d: %v", format, err)
		}
		resource := host.resources[description.ID]
		if !resource.emulatedIntegerMSAA || resource.textureTarget != glTexture2DArray {
			t.Fatalf("format %d storage: emulated=%t target=%#x", format, resource.emulatedIntegerMSAA, resource.textureTarget)
		}
	}
}

func TestSharedExponentTextureUploadsToSamplerNativeStorage(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR9G9B9E5Float, Bind: 1 << 3,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	pixel := []byte{0x01, 0x01, 0x01, 0x81}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID, Stride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}
	if err := host.transferToHost(&resource{description: description, data: pixel}, transfer); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if glError := host.gl.getError(); glError != 0 {
			return fmt.Errorf("shared-exponent texture upload produced GL error %#x", glError)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedExponent2DReadbackPreservesPackedSubrectangle(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR9G9B9E5Float, Bind: 1 << 3,
		Width: 3, Height: 2, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	upload := make([]byte, 3*2*4)
	for index := range 6 {
		// A common exponent with exact, distinct mantissas in each channel.
		binary.LittleEndian.PutUint32(upload[index*4:],
			15<<27|uint32(index+3)<<18|uint32(index+2)<<9|uint32(index+1))
	}
	// Preserve the single populated channel used by GL_RED client uploads.
	binary.LittleEndian.PutUint32(upload[4:], 15<<27|72)
	if err := host.transferToHost(&resource{description: description, data: upload}, virtio.GPUTransfer3D{
		ResourceID: description.ID, Stride: 3 * 4,
		Box: virtio.GPUBox{Width: 3, Height: 2, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, 24)}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID, Offset: 4, Stride: 12,
		Box: virtio.GPUBox{X: 1, Width: 2, Height: 2, Depth: 1},
	}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, len(readback.data))
	copy(want[4:12], upload[4:12])
	copy(want[16:24], upload[16:24])
	if string(readback.data) != string(want) {
		t.Fatalf("shared-exponent 2D subrectangle = %x, want %x", readback.data, want)
	}
}

func TestDepth32FloatPreservesNativePrecision(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatZ32Float,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 4)
	binary.LittleEndian.PutUint32(want, math.Float32bits(0.1234567))
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Stride:     4,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}
	if err := host.transferToHost(&resource{description: description, data: append([]byte(nil), want...)}, transfer); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, 4)}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	if string(readback.data) != string(want) {
		t.Fatalf("depth32f round trip bits = %#x, want %#x", binary.LittleEndian.Uint32(readback.data), binary.LittleEndian.Uint32(want))
	}
}

func TestDepth24X8UsesGuestScaledIntegerRepresentation(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 1, Format: virglFormatZ24X8UNorm,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 4)
	binary.LittleEndian.PutUint32(want, 0x00200000)
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID, Stride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}
	if err := host.transferToHost(&resource{description: description, data: append([]byte(nil), want...)}, transfer); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		var framebuffer uint32
		host.gl.genFramebuffers(1, &framebuffer)
		defer host.gl.deleteFramebuffers(1, &framebuffer)
		host.gl.bindFramebuffer(glReadFramebuffer, framebuffer)
		host.gl.framebufferTexture1D(glReadFramebuffer, glDepthAttachment, glTexture1D, host.resources[description.ID].texture, 0)
		if status := host.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("depth framebuffer status %#x", status)
		}
		pixel := make([]byte, 4)
		host.gl.readPixels(0, 0, 1, 1, glDepthComponent, glFloat, glPointer(pixel))
		if got := math.Float32frombits(binary.LittleEndian.Uint32(pixel)); math.Abs(float64(got-0.125)) > 1e-6 {
			return fmt.Errorf("native depth = %g, want 0.125", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, 4)}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(readback.data); got != 0x00200000 {
		t.Fatalf("guest depth bits = %#x, want %#x", got, uint32(0x00200000))
	}
}

func TestDepth24Stencil8ReordersGuestPackedChannels(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 1, Format: virglFormatZ24UNormS8UInt,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 4)
	binary.LittleEndian.PutUint32(want, 0x5a200000)
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID, Stride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}
	if err := host.transferToHost(&resource{description: description, data: append([]byte(nil), want...)}, transfer); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		var framebuffer uint32
		host.gl.genFramebuffers(1, &framebuffer)
		defer host.gl.deleteFramebuffers(1, &framebuffer)
		host.gl.bindFramebuffer(glReadFramebuffer, framebuffer)
		host.gl.framebufferTexture1D(glReadFramebuffer, glDepthStencilAttachment, glTexture1D, host.resources[description.ID].texture, 0)
		if status := host.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("depth/stencil framebuffer status %#x", status)
		}
		depth := make([]byte, 4)
		host.gl.readPixels(0, 0, 1, 1, glDepthComponent, glFloat, glPointer(depth))
		if got := math.Float32frombits(binary.LittleEndian.Uint32(depth)); math.Abs(float64(got-0.125)) > 1e-6 {
			return fmt.Errorf("native depth = %g, want 0.125", got)
		}
		stencil := []byte{0}
		host.gl.readPixels(0, 0, 1, 1, glStencilIndex, glUnsignedByte, glPointer(stencil))
		if stencil[0] != 0x5a {
			return fmt.Errorf("native stencil = %#x, want %#x", stencil[0], byte(0x5a))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, 4)}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(readback.data); got != 0x5a200000 {
		t.Fatalf("guest packed depth/stencil = %#x, want %#x", got, uint32(0x5a200000))
	}
}

func TestDepth32FloatStencil8RoundTripsNativeStorage(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatZ32FloatS8X24UInt,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 8)
	binary.LittleEndian.PutUint32(want, math.Float32bits(0.1234567))
	binary.LittleEndian.PutUint32(want[4:], 0x5a)
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID, Stride: 8,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}
	if err := host.transferToHost(&resource{description: description, data: append([]byte(nil), want...)}, transfer); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, 8)}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	if string(readback.data) != string(want) {
		t.Fatalf("depth32f-stencil8 round trip = %#x, want %#x", readback.data, want)
	}
}
