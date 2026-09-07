package virgl

// VirGL format identifiers are protocol values from Mesa 26.1.6's
// src/virtio/virtio-gpu/virgl_hw.h. They are deliberately not Gallium's
// internal PIPE_FORMAT values, whose numbering can change between releases.
const (
	virglFormatB8G8R8A8UNorm     uint32 = 1
	virglFormatB8G8R8X8UNorm     uint32 = 2
	virglFormatR10G10B10A2UNorm  uint32 = 8
	virglFormatZ16UNorm          uint32 = 16
	virglFormatZ32Float          uint32 = 18
	virglFormatZ24UNormS8UInt    uint32 = 19
	virglFormatZ24X8UNorm        uint32 = 21
	virglFormatS8UInt            uint32 = 23
	virglFormatR32Float          uint32 = 28
	virglFormatR32G32Float       uint32 = 29
	virglFormatR32G32B32Float    uint32 = 30
	virglFormatR32G32B32A32Float uint32 = 31
	virglFormatR16UNorm          uint32 = 48
	virglFormatR16G16UNorm       uint32 = 49
	virglFormatR16G16B16UNorm    uint32 = 50
	virglFormatR16G16B16A16UNorm uint32 = 51
	virglFormatR16SNorm          uint32 = 56
	virglFormatR16G16SNorm       uint32 = 57
	virglFormatR16G16B16SNorm    uint32 = 58
	virglFormatR16G16B16A16SNorm uint32 = 59
	virglFormatR8UNorm           uint32 = 64
	virglFormatR8G8UNorm         uint32 = 65
	virglFormatR8G8B8UNorm       uint32 = 66
	virglFormatR8G8B8A8UNorm     uint32 = 67
	virglFormatX8B8G8R8UNorm     uint32 = 68
	virglFormatR8SNorm           uint32 = 74
	virglFormatR8G8SNorm         uint32 = 75
	virglFormatR8G8B8SNorm       uint32 = 76
	virglFormatR8G8B8A8SNorm     uint32 = 77
	virglFormatR16Float          uint32 = 91
	virglFormatR16G16Float       uint32 = 92
	virglFormatR16G16B16Float    uint32 = 93
	virglFormatR16G16B16A16Float uint32 = 94
	virglFormatR8G8B8SRGB        uint32 = 97
	virglFormatA8B8G8R8SRGB      uint32 = 98
	virglFormatB8G8R8A8SRGB      uint32 = 100
	virglFormatB8G8R8X8SRGB      uint32 = 101
	virglFormatR8G8B8A8SRGB      uint32 = 104
	virglFormatA8B8G8R8UNorm     uint32 = 121
	virglFormatR11G11B10Float    uint32 = 124
	virglFormatR9G9B9E5Float     uint32 = 125
	virglFormatZ32FloatS8X24UInt uint32 = 126
	virglFormatB10G10R10A2UNorm  uint32 = 131
	virglFormatR8G8B8X8UNorm     uint32 = 134
	virglFormatR8G8B8X8SRGB      uint32 = 230
	virglFormatR32G32B32A32UInt  uint32 = 196
	virglFormatR8UInt            uint32 = 177
	virglFormatR8G8UInt          uint32 = 178
	virglFormatR8G8B8UInt        uint32 = 179
	virglFormatR8G8B8A8UInt      uint32 = 180
	virglFormatR8SInt            uint32 = 181
	virglFormatR8G8SInt          uint32 = 182
	virglFormatR8G8B8SInt        uint32 = 183
	virglFormatR8G8B8A8SInt      uint32 = 184
	virglFormatR16UInt           uint32 = 185
	virglFormatR16G16UInt        uint32 = 186
	virglFormatR16G16B16UInt     uint32 = 187
	virglFormatR16G16B16A16UInt  uint32 = 188
	virglFormatR16SInt           uint32 = 189
	virglFormatR16G16SInt        uint32 = 190
	virglFormatR16G16B16SInt     uint32 = 191
	virglFormatR16G16B16A16SInt  uint32 = 192
	virglFormatR32UInt           uint32 = 193
	virglFormatR32G32UInt        uint32 = 194
	virglFormatR32G32B32UInt     uint32 = 195
	virglFormatR32SInt           uint32 = 197
	virglFormatR32G32SInt        uint32 = 198
	virglFormatR32G32B32SInt     uint32 = 199
	virglFormatR32G32B32A32SInt  uint32 = 200
	virglFormatB10G10R10A2UInt   uint32 = 225
	virglFormatR10G10B10A2UInt   uint32 = 253
)

type textureFormatDescription struct {
	format        uint32
	bytesPerPixel uint64
	sampler       bool
	render        bool
	depthStencil  bool
	readback      bool
}

var textureFormatDescriptions = []textureFormatDescription{
	{virglFormatB8G8R8A8UNorm, 4, true, true, false, true},
	{virglFormatB8G8R8X8UNorm, 4, true, true, false, true},
	{virglFormatR10G10B10A2UNorm, 4, true, true, false, true},
	{virglFormatZ16UNorm, 2, true, true, true, false},
	{virglFormatZ32Float, 4, true, true, true, false},
	{virglFormatZ24UNormS8UInt, 4, true, true, true, false},
	{virglFormatZ24X8UNorm, 4, true, true, true, false},
	{virglFormatS8UInt, 1, true, true, true, false},
	{virglFormatR32Float, 4, true, true, false, true},
	{virglFormatR32G32Float, 8, true, true, false, true},
	{virglFormatR32G32B32Float, 12, true, true, false, true},
	{virglFormatR32G32B32A32Float, 16, true, true, false, true},
	{virglFormatR16UNorm, 2, true, true, false, true},
	{virglFormatR16G16UNorm, 4, true, true, false, true},
	{virglFormatR16G16B16UNorm, 6, true, true, false, true},
	{virglFormatR16G16B16A16UNorm, 8, true, true, false, true},
	{virglFormatR16SNorm, 2, true, true, false, true},
	{virglFormatR16G16SNorm, 4, true, true, false, true},
	{virglFormatR16G16B16SNorm, 6, true, true, false, true},
	{virglFormatR16G16B16A16SNorm, 8, true, true, false, true},
	{virglFormatR8UNorm, 1, true, true, false, true},
	{virglFormatR8G8UNorm, 2, true, true, false, true},
	{virglFormatR8G8B8UNorm, 3, true, true, false, true},
	{virglFormatR8G8B8A8UNorm, 4, true, true, false, true},
	{virglFormatX8B8G8R8UNorm, 4, true, true, false, true},
	{virglFormatR8SNorm, 1, true, true, false, true},
	{virglFormatR8G8SNorm, 2, true, true, false, true},
	{virglFormatR8G8B8SNorm, 3, true, true, false, true},
	{virglFormatR8G8B8A8SNorm, 4, true, true, false, true},
	{virglFormatR16Float, 2, true, true, false, true},
	{virglFormatR16G16Float, 4, true, true, false, true},
	{virglFormatR16G16B16Float, 6, true, true, false, true},
	{virglFormatR16G16B16A16Float, 8, true, true, false, true},
	{virglFormatR8G8B8SRGB, 3, true, true, false, true},
	{virglFormatA8B8G8R8SRGB, 4, true, true, false, true},
	{virglFormatB8G8R8A8SRGB, 4, true, true, false, true},
	{virglFormatB8G8R8X8SRGB, 4, true, true, false, true},
	{virglFormatR8G8B8A8SRGB, 4, true, true, false, true},
	{virglFormatR8G8B8X8SRGB, 4, true, true, false, true},
	{virglFormatA8B8G8R8UNorm, 4, true, true, false, true},
	{virglFormatR11G11B10Float, 4, true, true, false, true},
	{virglFormatR9G9B9E5Float, 4, true, false, false, false},
	{virglFormatZ32FloatS8X24UInt, 8, true, true, true, false},
	{virglFormatB10G10R10A2UNorm, 4, true, true, false, true},
	{virglFormatR8G8B8X8UNorm, 4, true, true, false, true},
	{virglFormatR32G32B32A32UInt, 16, true, true, false, true},
	{virglFormatR32G32B32A32SInt, 16, true, true, false, true},
	{virglFormatR8UInt, 1, true, true, false, true},
	{virglFormatR8G8UInt, 2, true, true, false, true},
	{virglFormatR8G8B8UInt, 3, true, true, false, true},
	{virglFormatR8G8B8A8UInt, 4, true, true, false, true},
	{virglFormatR8SInt, 1, true, true, false, true},
	{virglFormatR8G8SInt, 2, true, true, false, true},
	{virglFormatR8G8B8SInt, 3, true, true, false, true},
	{virglFormatR8G8B8A8SInt, 4, true, true, false, true},
	{virglFormatR16UInt, 2, true, true, false, true},
	{virglFormatR16G16UInt, 4, true, true, false, true},
	{virglFormatR16G16B16UInt, 6, true, true, false, true},
	{virglFormatR16G16B16A16UInt, 8, true, true, false, true},
	{virglFormatR16SInt, 2, true, true, false, true},
	{virglFormatR16G16SInt, 4, true, true, false, true},
	{virglFormatR16G16B16SInt, 6, true, true, false, true},
	{virglFormatR16G16B16A16SInt, 8, true, true, false, true},
	{virglFormatR32UInt, 4, true, true, false, true},
	{virglFormatR32G32UInt, 8, true, true, false, true},
	{virglFormatR32G32B32UInt, 12, true, true, false, true},
	{virglFormatR32SInt, 4, true, true, false, true},
	{virglFormatR32G32SInt, 8, true, true, false, true},
	{virglFormatR32G32B32SInt, 12, true, true, false, true},
	{virglFormatB10G10R10A2UInt, 4, true, true, false, true},
	{virglFormatR10G10B10A2UInt, 4, true, true, false, true},
}

func describeTextureFormat(format uint32) (textureFormatDescription, bool) {
	for _, description := range textureFormatDescriptions {
		if description.format == format {
			return description, true
		}
	}
	return textureFormatDescription{}, false
}

func textureFormatBytes(format uint32) uint64 {
	description, ok := describeTextureFormat(format)
	if !ok {
		return 0
	}
	return description.bytesPerPixel
}

func isIntegerTextureFormat(format uint32) bool {
	return (format >= virglFormatR8UInt && format <= virglFormatR32G32B32A32SInt) ||
		format == virglFormatR10G10B10A2UInt || format == virglFormatB10G10R10A2UInt
}

func isSignedIntegerTextureFormat(format uint32) bool {
	return (format >= virglFormatR8SInt && format <= virglFormatR8G8B8A8SInt) ||
		(format >= virglFormatR16SInt && format <= virglFormatR16G16B16A16SInt) ||
		(format >= virglFormatR32SInt && format <= virglFormatR32G32B32A32SInt)
}
