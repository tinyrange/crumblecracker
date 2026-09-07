package virgl

import (
	"encoding/binary"
	"math"
)

const (
	capsetVirGL               = 1
	capsetVirGL2              = 2
	capsetVersion             = 1
	capsetVersion2            = 2
	capsetV1Size              = 308
	capsetV2Size              = 1376
	capsetV2LimitsStart       = capsetV1Size
	capsetMaxTexture2D        = 16384
	capsetMaxTexture3D        = 2048
	capsetMaxArrayLayers      = 2048
	capsetMaxTextureBuffer    = 64 * 1024
	capsetMaxUniformBlockSize = 64 * 1024
	capsetMaxAnisotropy       = 16
)

func buildCapsetV1() []byte {
	data := make([]byte, capsetV1Size)
	put := func(offset int, value uint32) {
		binary.LittleEndian.PutUint32(data[offset:offset+4], value)
	}
	setFormat := func(maskOffset int, format uint32) {
		word := format / 32
		bit := format % 32
		offset := maskOffset + int(word)*4
		put(offset, binary.LittleEndian.Uint32(data[offset:offset+4])|(1<<bit))
	}

	// struct virgl_caps_v1. Keep this deliberately small: every advertised
	// format and limit must have a decoder/backend implementation.
	put(0, capsetVersion)
	const (
		samplerMaskOffset      = 4
		renderMaskOffset       = samplerMaskOffset + 64
		depthStencilMaskOffset = renderMaskOffset + 64
		vertexBufferMaskOffset = depthStencilMaskOffset + 64
		booleanSetOffset       = vertexBufferMaskOffset + 64
		glslLevelOffset        = booleanSetOffset + 4
	)
	for _, description := range textureFormatDescriptions {
		if description.sampler {
			setFormat(samplerMaskOffset, description.format)
		}
		if description.render {
			setFormat(renderMaskOffset, description.format)
		}
		if description.depthStencil {
			// Mesa derives depth-bearing EGL configs from the ordinary sampler
			// and render support masks. The legacy depthstencil mask alone is
			// not sufficient to expose those configs.
			setFormat(depthStencilMaskOffset, description.format)
		}
	}
	for _, format := range []uint32{
		8, 123, 172, 173,
		28, 29, 30, 31,
		32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47,
		48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63,
		64, 65, 66, 67, 69, 70, 71, 72, 74, 75, 76, 77, 82, 83, 84, 85,
		87, 88, 89, 90, 91, 92, 93, 94,
	} {
		setFormat(vertexBufferMaskOffset, format)
	}
	for format := virglFormatR8UInt; format <= virglFormatR32G32B32A32SInt; format++ {
		setFormat(vertexBufferMaskOffset, format)
	}
	// Independent blend state, conditional/timer/occlusion queries, seamless
	// cube sampling, primitive restart, separate blend equations, depth
	// clamping, texture LOD queries, native double-precision shaders, indirect
	// drawing, and sample shading all have native behavior paths.
	put(booleanSetOffset, (1<<0)|(1<<1)|(1<<2)|(1<<4)|(1<<6)|(1<<7)|(1<<10)|(1<<11)|(1<<12)|(1<<13)|(1<<14)|(1<<16)|(1<<22)|(1<<23)|(1<<24)|(1<<25)|(1<<26))
	put(glslLevelOffset+0, 410) // GLSL 4.10
	put(glslLevelOffset+4, capsetMaxArrayLayers)
	put(glslLevelOffset+8, 4)       // max streamout buffers
	put(glslLevelOffset+12, 1)      // max dual-source render targets
	put(glslLevelOffset+16, 8)      // max render targets
	put(glslLevelOffset+20, 4)      // max samples
	put(glslLevelOffset+24, 0x7fff) // all native primitive modes, including patches
	put(glslLevelOffset+28, capsetMaxTextureBuffer)
	put(glslLevelOffset+32, 13) // default constants plus 12 uniform blocks
	put(glslLevelOffset+36, 16) // max viewports
	put(glslLevelOffset+40, 4)  // max texture gather components
	return data
}

func buildCapsetV2() []byte {
	data := make([]byte, capsetV2Size)
	copy(data, buildCapsetV1())
	put := func(offset int, value uint32) {
		binary.LittleEndian.PutUint32(data[offset:offset+4], value)
	}
	putFloat := func(offset int, value float32) {
		put(offset, math.Float32bits(value))
	}
	setFormat := func(maskOffset int, format uint32) {
		word := format / 32
		bit := format % 32
		offset := maskOffset + int(word)*4
		put(offset, binary.LittleEndian.Uint32(data[offset:offset+4])|(1<<bit))
	}

	// struct virgl_caps_v2 extends v1. This first profile intentionally adds
	// the extensible ABI and measured limits without claiming any new command
	// family. Later GL 3.x/4.x slices can raise individual fields only after
	// their decoder, TGSI, host GL, transfer, and conformance paths exist.
	put(0, capsetVersion2)
	const (
		minAliasedPointSizeOffset = capsetV2LimitsStart
		maxAliasedPointSizeOffset = minAliasedPointSizeOffset + 4
		minSmoothPointSizeOffset  = maxAliasedPointSizeOffset + 4
		maxSmoothPointSizeOffset  = minSmoothPointSizeOffset + 4
		minAliasedLineWidthOffset = maxSmoothPointSizeOffset + 4
		maxAliasedLineWidthOffset = minAliasedLineWidthOffset + 4
		minSmoothLineWidthOffset  = maxAliasedLineWidthOffset + 4
		maxSmoothLineWidthOffset  = minSmoothLineWidthOffset + 4
		maxTextureLODBiasOffset   = maxSmoothLineWidthOffset + 4
		maxGeomOutputVertices     = maxTextureLODBiasOffset + 4
		maxGeomOutputComponents   = maxGeomOutputVertices + 4
		maxVertexOutputsOffset    = maxTextureLODBiasOffset + 4 + 8
		maxVertexAttribsOffset    = maxVertexOutputsOffset + 4
		maxShaderPatchOffset      = maxVertexAttribsOffset + 4
		maxTexture2DSizeOffset    = capsetV2LimitsStart + 176
		hostFeatureVersionOffset  = capsetV2LimitsStart + 248
		readbackFormatsOffset     = hostFeatureVersionOffset + 4
		rendererOffset            = capsetV2LimitsStart + 388
		maxAnisotropyOffset       = rendererOffset + 64
		maxShaderSamplersOffset   = maxAnisotropyOffset + 4
		multisampleFormatsOffset  = maxShaderSamplersOffset + 4
		maxConstBufferSizeOffset  = capsetV2LimitsStart + 524
		maxUniformBlockSizeOffset = capsetV2LimitsStart + 1064
	)
	putFloat(minAliasedPointSizeOffset, 1)
	putFloat(maxAliasedPointSizeOffset, 64)
	putFloat(minSmoothPointSizeOffset, 1)
	putFloat(maxSmoothPointSizeOffset, 64)
	putFloat(minAliasedLineWidthOffset, 1)
	putFloat(maxAliasedLineWidthOffset, 1)
	putFloat(minSmoothLineWidthOffset, 1)
	putFloat(maxSmoothLineWidthOffset, 1)
	putFloat(maxTextureLODBiasOffset, 16)
	put(maxGeomOutputVertices, 256)
	put(maxGeomOutputComponents, 1024)
	put(maxVertexOutputsOffset, 16)
	put(maxVertexAttribsOffset, 16)
	put(maxShaderPatchOffset, 16)
	put(capsetV2LimitsStart+56, ^uint32(7)) // minimum program texel offset: -8
	put(capsetV2LimitsStart+60, 7)          // maximum program texel offset
	put(capsetV2LimitsStart+64, ^uint32(7)) // minimum texture gather offset: -8
	put(capsetV2LimitsStart+68, 7)          // maximum texture gather offset
	put(maxTexture2DSizeOffset, capsetMaxTexture2D)
	put(maxTexture2DSizeOffset+4, capsetMaxTexture3D)
	put(maxTexture2DSizeOffset+8, capsetMaxTexture2D)
	// Version 13 lets current Mesa consume the explicitly bounded constant and
	// uniform-buffer sizes below. Earlier fields retain zero unless their
	// command family is implemented; the two legacy defaults disabled by a
	// nonzero version are restored through the matching capability bits.
	put(capsetV2LimitsStart+76, 16)                             // uniform buffer offset alignment
	put(capsetV2LimitsStart+84, (1<<2)|(1<<15)|(1<<18)|(1<<23)) // minimum samples, sRGB writes, mixed color formats, and transform feedback 3
	put(hostFeatureVersionOffset, 13)
	for stage := 0; stage < 6; stage++ {
		put(maxConstBufferSizeOffset+stage*4, capsetMaxUniformBlockSize)
	}
	put(maxUniformBlockSizeOffset, capsetMaxUniformBlockSize)
	for _, description := range textureFormatDescriptions {
		if description.readback {
			setFormat(readbackFormatsOffset, description.format)
		}
	}
	copy(data[rendererOffset:rendererOffset+64], []byte("vmsh Darwin VirGL"))
	putFloat(maxAnisotropyOffset, capsetMaxAnisotropy)
	put(maxShaderSamplersOffset, 16)
	for _, description := range textureFormatDescriptions {
		if description.render {
			setFormat(multisampleFormatsOffset, description.format)
		}
	}
	return data
}
