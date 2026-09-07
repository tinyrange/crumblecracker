package virgl

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestCapsetExposesRenderableDepthFormatsForEGLConfigs(t *testing.T) {
	capset := buildCapsetV1()
	const (
		samplerOffset = 4
		renderOffset  = samplerOffset + 64
		depthOffset   = renderOffset + 64
	)
	for _, format := range []uint32{virglFormatZ16UNorm, virglFormatZ32Float, virglFormatZ24UNormS8UInt, virglFormatZ24X8UNorm, virglFormatS8UInt, virglFormatZ32FloatS8X24UInt} {
		for _, offset := range []int{samplerOffset, renderOffset, depthOffset} {
			word := binary.LittleEndian.Uint32(capset[offset+int(format/32)*4:])
			if word&(1<<(format%32)) == 0 {
				t.Fatalf("format %d is absent from mask at byte %d", format, offset)
			}
		}
	}
}

func TestCapsetAdvertisesOnlyImplementedModernTextureFormats(t *testing.T) {
	capset := buildCapsetV1()
	const (
		samplerOffset = 4
		renderOffset  = samplerOffset + 64
	)
	for _, format := range []uint32{
		virglFormatR8UNorm, virglFormatR8G8UNorm,
		virglFormatR8G8B8UNorm,
		virglFormatR8SNorm, virglFormatR8G8SNorm, virglFormatR8G8B8A8SNorm,
		virglFormatR8G8B8SNorm,
		virglFormatR16UNorm, virglFormatR16G16UNorm, virglFormatR16G16B16UNorm, virglFormatR16G16B16A16UNorm,
		virglFormatR16SNorm, virglFormatR16G16SNorm, virglFormatR16G16B16SNorm, virglFormatR16G16B16A16SNorm,
		virglFormatR16Float, virglFormatR16G16Float, virglFormatR16G16B16A16Float,
		virglFormatR32Float, virglFormatR32G32Float, virglFormatR32G32B32A32Float,
		virglFormatA8B8G8R8SRGB, virglFormatB8G8R8A8SRGB, virglFormatB8G8R8X8SRGB,
		virglFormatR8G8B8A8SRGB, virglFormatR8G8B8X8SRGB,
		virglFormatR10G10B10A2UNorm, virglFormatB10G10R10A2UNorm,
		virglFormatR11G11B10Float,
		virglFormatR32G32B32A32UInt, virglFormatR32G32B32A32SInt,
		virglFormatR10G10B10A2UInt, virglFormatB10G10R10A2UInt,
	} {
		for _, offset := range []int{samplerOffset, renderOffset} {
			word := binary.LittleEndian.Uint32(capset[offset+int(format/32)*4:])
			if word&(1<<(format%32)) == 0 {
				t.Fatalf("format %d is absent from mask at byte %d", format, offset)
			}
		}
	}
	format := virglFormatR9G9B9E5Float
	samplerWord := binary.LittleEndian.Uint32(capset[samplerOffset+int(format/32)*4:])
	renderWord := binary.LittleEndian.Uint32(capset[renderOffset+int(format/32)*4:])
	if samplerWord&(1<<(format%32)) == 0 || renderWord&(1<<(format%32)) != 0 {
		t.Fatalf("sampler-only format %d masks: sampler=%#x render=%#x", format, samplerWord, renderWord)
	}
}

func TestCapsetAdvertisesImplementedArrayTexturesAndMultipleRenderTargets(t *testing.T) {
	capset := buildCapsetV1()
	const (
		glslLevelOffset        = 4 + 64*4 + 4
		maxArrayLayersOffset   = 4 + 64*4 + 4 + 4
		maxStreamoutOffset     = 4 + 64*4 + 4 + 8
		maxDualSourceOffset    = 4 + 64*4 + 4 + 12
		maxRenderTargetsOffset = 4 + 64*4 + 4 + 16
		maxSamplesOffset       = 4 + 64*4 + 4 + 20
		maxUniformBlocksOffset = 4 + 64*4 + 4 + 32
		maxGatherOffset        = 4 + 64*4 + 4 + 40
	)
	if got := binary.LittleEndian.Uint32(capset[glslLevelOffset:]); got != 410 {
		t.Fatalf("GLSL level = %d, want 410", got)
	}
	if got := binary.LittleEndian.Uint32(capset[maxArrayLayersOffset:]); got != capsetMaxArrayLayers {
		t.Fatalf("maximum array texture layers = %d, want %d", got, capsetMaxArrayLayers)
	}
	if got := binary.LittleEndian.Uint32(capset[maxStreamoutOffset:]); got != 4 {
		t.Fatalf("maximum streamout buffers = %d, want 4", got)
	}
	if got := binary.LittleEndian.Uint32(capset[maxDualSourceOffset:]); got != 1 {
		t.Fatalf("maximum dual-source render targets = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(capset[maxRenderTargetsOffset:]); got != 8 {
		t.Fatalf("maximum render targets = %d, want 8", got)
	}
	if got := binary.LittleEndian.Uint32(capset[maxSamplesOffset:]); got != 4 {
		t.Fatalf("maximum samples = %d, want 4", got)
	}
	if got := binary.LittleEndian.Uint32(capset[maxUniformBlocksOffset:]); got != 13 {
		t.Fatalf("constant-buffer slots = %d, want 13", got)
	}
	if got := binary.LittleEndian.Uint32(capset[maxGatherOffset:]); got != 4 {
		t.Fatalf("texture gather components = %d, want 4", got)
	}
}

func TestCapsetAdvertisesImplementedVertexFormats(t *testing.T) {
	capset := buildCapsetV1()
	const vertexBufferOffset = 4 + 64*3
	for _, format := range []uint32{
		8, 123, 172, 173,
		28, 29, 30, 31,
		32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47,
		48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63,
		64, 65, 66, 67, 69, 70, 71, 72, 74, 75, 76, 77, 82, 83, 84, 85,
		87, 88, 89, 90, 91, 92, 93, 94,
	} {
		word := binary.LittleEndian.Uint32(capset[vertexBufferOffset+int(format/32)*4:])
		if word&(1<<(format%32)) == 0 {
			t.Fatalf("implemented vertex format %d is absent", format)
		}
	}
}

func TestCapsetV2AdvertisesNativeMultisampleFormats(t *testing.T) {
	capset := buildCapsetV2()
	const multisampleFormatsOffset = capsetV2LimitsStart + 460
	for _, format := range []uint32{
		virglFormatR8UNorm,
		virglFormatR8G8B8A8UNorm,
		virglFormatZ24UNormS8UInt,
		virglFormatR8UInt,
		virglFormatR32G32B32A32SInt,
		virglFormatR10G10B10A2UInt,
	} {
		word := binary.LittleEndian.Uint32(capset[multisampleFormatsOffset+int(format/32)*4:])
		if word&(1<<(format%32)) == 0 {
			t.Fatalf("multisample format %d is absent", format)
		}
	}
}

func TestCapsetV2AdvertisesCoreTextureOffsetRanges(t *testing.T) {
	capset := buildCapsetV2()
	for name, offset := range map[string]int{
		"program texel":  56,
		"texture gather": 64,
	} {
		minOffset := int32(binary.LittleEndian.Uint32(capset[capsetV2LimitsStart+offset:]))
		maxOffset := int32(binary.LittleEndian.Uint32(capset[capsetV2LimitsStart+offset+4:]))
		if minOffset != -8 || maxOffset != 7 {
			t.Fatalf("%s offset range = %d..%d, want -8..7", name, minOffset, maxOffset)
		}
	}
}

func TestCapsetV2AdvertisesMultipleTransformFeedbackStreams(t *testing.T) {
	capset := buildCapsetV2()
	const capabilityBitsOffset = capsetV2LimitsStart + 84
	bits := binary.LittleEndian.Uint32(capset[capabilityBitsOffset:])
	if bits&(1<<23) == 0 {
		t.Fatalf("transform-feedback-3 capability bit is absent from %#x", bits)
	}
}

func TestCapsetAdvertisesImplementedGL3StateFamilies(t *testing.T) {
	capset := buildCapsetV1()
	const booleanSetOffset = 4 + 64*4
	bits := binary.LittleEndian.Uint32(capset[booleanSetOffset:])
	for _, bit := range []uint32{0, 1, 2, 4, 6, 7, 10, 11, 12, 13, 14, 16, 22, 23, 24, 25, 26} {
		if bits&(1<<bit) == 0 {
			t.Fatalf("implemented capability bit %d is absent from %#x", bit, bits)
		}
	}
}

func TestCapsetAdvertisesBoundedTextureBufferSize(t *testing.T) {
	capset := buildCapsetV1()
	const maxTextureBufferSizeOffset = 4 + 64*4 + 4 + 28
	if got := binary.LittleEndian.Uint32(capset[maxTextureBufferSizeOffset:]); got != capsetMaxTextureBuffer {
		t.Fatalf("max texture-buffer elements = %d, want %d", got, capsetMaxTextureBuffer)
	}
}

func TestCapsetV2PublishesBoundedUniformBufferLimits(t *testing.T) {
	capset := buildCapsetV2()
	if got := len(capset); got != capsetV2Size {
		t.Fatalf("capset v2 size = %d, want %d", got, capsetV2Size)
	}
	if got := binary.LittleEndian.Uint32(capset[0:4]); got != capsetVersion2 {
		t.Fatalf("capset v2 max version = %d, want %d", got, capsetVersion2)
	}
	const (
		maxTexture2DSizeOffset    = capsetV2LimitsStart + 176
		hostFeatureVersionOffset  = capsetV2LimitsStart + 248
		maxConstBufferSizeOffset  = capsetV2LimitsStart + 524
		maxUniformBlockSizeOffset = capsetV2LimitsStart + 1064
		rendererOffset            = capsetV2LimitsStart + 388
		maxAnisotropyOffset       = rendererOffset + 64
	)
	if got := binary.LittleEndian.Uint32(capset[maxTexture2DSizeOffset:]); got != capsetMaxTexture2D {
		t.Fatalf("capset v2 max 2D texture size = %d, want %d", got, capsetMaxTexture2D)
	}
	if got := binary.LittleEndian.Uint32(capset[maxTexture2DSizeOffset+4:]); got != capsetMaxTexture3D {
		t.Fatalf("capset v2 max 3D texture size = %d, want %d", got, capsetMaxTexture3D)
	}
	if got := binary.LittleEndian.Uint32(capset[maxTexture2DSizeOffset+8:]); got != capsetMaxTexture2D {
		t.Fatalf("capset v2 max cube texture size = %d, want %d", got, capsetMaxTexture2D)
	}
	if got := binary.LittleEndian.Uint32(capset[hostFeatureVersionOffset:]); got != 13 {
		t.Fatalf("capset v2 host feature check version = %d, want 13", got)
	}
	for stage := 0; stage < 6; stage++ {
		if got := binary.LittleEndian.Uint32(capset[maxConstBufferSizeOffset+stage*4:]); got != capsetMaxUniformBlockSize {
			t.Fatalf("stage %d constant-buffer size = %d, want %d", stage, got, capsetMaxUniformBlockSize)
		}
	}
	if got := binary.LittleEndian.Uint32(capset[maxUniformBlockSizeOffset:]); got != capsetMaxUniformBlockSize {
		t.Fatalf("uniform-block size = %d, want %d", got, capsetMaxUniformBlockSize)
	}
	if got := string(capset[rendererOffset : rendererOffset+17]); got != "vmsh Darwin VirGL" {
		t.Fatalf("capset v2 renderer = %q", got)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(capset[maxAnisotropyOffset:])); got != capsetMaxAnisotropy {
		t.Fatalf("capset v2 max anisotropy = %v, want %v", got, capsetMaxAnisotropy)
	}
}

func TestRendererPublishesBothVirGLCapsetGenerations(t *testing.T) {
	capsets := NewRenderer(nil).Capsets()
	if len(capsets) != 2 {
		t.Fatalf("renderer capset count = %d, want 2", len(capsets))
	}
	if capsets[0].ID != capsetVirGL || capsets[0].Version != capsetVersion {
		t.Fatalf("legacy capset identity = (%d, %d)", capsets[0].ID, capsets[0].Version)
	}
	if capsets[1].ID != capsetVirGL2 || capsets[1].Version != capsetVersion2 {
		t.Fatalf("v2 capset identity = (%d, %d)", capsets[1].ID, capsets[1].Version)
	}
}
