//go:build darwin

package virgl

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	glArrayBuffer                        = 0x8892
	glElementArrayBuffer                 = 0x8893
	glUniformBuffer                      = 0x8a11
	glUniformBlockDataSize               = 0x8a40
	glDrawIndirectBuffer                 = 0x8f3f
	glStreamDraw                         = 0x88e0
	glStaticDraw                         = 0x88e4
	glTexture1D                          = 0x0de0
	glTexture2D                          = 0x0de1
	glTexture3D                          = 0x806f
	glTexture1DArray                     = 0x8c18
	glTexture2DArray                     = 0x8c1a
	glTextureCubeMap                     = 0x8513
	glTextureCubeMapPositiveX            = 0x8515
	glTextureCubeMapArray                = 0x9009
	glTextureBuffer                      = 0x8c2a
	glTextureRectangle                   = 0x84f5
	glTexture2DMultisample               = 0x9100
	glTexture2DMultisampleArray          = 0x9102
	glTexture0                           = 0x84c0
	glR8                                 = 0x8229
	glRG8                                = 0x822b
	glRGB8                               = 0x8051
	glR16                                = 0x822a
	glRG16                               = 0x822c
	glRGB16                              = 0x8054
	glRGBA16                             = 0x805b
	glR8UI                               = 0x8232
	glRG8UI                              = 0x8238
	glRGB8UI                             = 0x8d7d
	glRGBA8UI                            = 0x8d7c
	glR8I                                = 0x8231
	glRG8I                               = 0x8237
	glRGB8I                              = 0x8d8f
	glRGBA8I                             = 0x8d8e
	glR16UI                              = 0x8234
	glRG16UI                             = 0x823a
	glRGB16UI                            = 0x8d77
	glRGBA16UI                           = 0x8d76
	glR16I                               = 0x8233
	glRG16I                              = 0x8239
	glRGB16I                             = 0x8d89
	glRGBA16I                            = 0x8d88
	glR32UI                              = 0x8236
	glRG32UI                             = 0x823c
	glRGB32UI                            = 0x8d71
	glR32I                               = 0x8235
	glRG32I                              = 0x823b
	glRGB32I                             = 0x8d83
	glR8SNorm                            = 0x8f94
	glRG8SNorm                           = 0x8f95
	glRGB8SNorm                          = 0x8f96
	glRGBA8SNorm                         = 0x8f97
	glR16SNorm                           = 0x8f98
	glRG16SNorm                          = 0x8f99
	glRGB16SNorm                         = 0x8f9a
	glRGBA16SNorm                        = 0x8f9b
	glR16F                               = 0x822d
	glRG16F                              = 0x822f
	glRGB16F                             = 0x881b
	glR32F                               = 0x822e
	glRG32F                              = 0x8230
	glRGB32F                             = 0x8815
	glRGBA8                              = 0x8058
	glRGB10A2                            = 0x8059
	glRGB10A2UI                          = 0x906f
	glR11FG11FB10F                       = 0x8c3a
	glRGB9E5                             = 0x8c3d
	glRGBA16F                            = 0x881a
	glRGBA32F                            = 0x8814
	glRGBA32UI                           = 0x8d70
	glRGBA32I                            = 0x8d82
	glSRGB8                              = 0x8c41
	glSRGB8Alpha8                        = 0x8c43
	glRG                                 = 0x8227
	glRGInteger                          = 0x8228
	glRGB                                = 0x1907
	glRGBInteger                         = 0x8d98
	glRGBA                               = 0x1908
	glRGBAInteger                        = 0x8d99
	glBGRAInteger                        = 0x8d9b
	glBGRA                               = 0x80e1
	glUnsignedByte                       = 0x1401
	glByte                               = 0x1400
	glShort                              = 0x1402
	glUnsignedShort                      = 0x1403
	glUnsignedInt                        = 0x1405
	glInt                                = 0x1404
	glUnsignedInt8888                    = 0x8035
	glUnsignedInt248                     = 0x84fa
	glUnsignedInt2101010Rev              = 0x8368
	glInt2101010Rev                      = 0x8d9f
	glUnsignedInt10F11F11FRev            = 0x8c3b
	glUnsignedInt5999Rev                 = 0x8c3e
	glHalfFloat                          = 0x140b
	glFloat                              = 0x1406
	glFixed                              = 0x140c
	glFramebuffer                        = 0x8d40
	glFramebufferSRGB                    = 0x8db9
	glReadFramebuffer                    = 0x8ca8
	glDrawFramebuffer                    = 0x8ca9
	glColorAttachment0                   = 0x8ce0
	glDepthAttachment                    = 0x8d00
	glStencilAttachment                  = 0x8d20
	glDepthStencilAttachment             = 0x821a
	glFramebufferComplete                = 0x8cd5
	glDepthComponent                     = 0x1902
	glDepthComponent16                   = 0x81a5
	glDepthComponent24                   = 0x81a6
	glDepthComponent32F                  = 0x8cac
	glDepth32FStencil8                   = 0x8cad
	glStencilIndex8                      = 0x8d48
	glDepthStencil                       = 0x84f9
	glDepth24Stencil8                    = 0x88f0
	glFloat32UnsignedInt248Rev           = 0x8dad
	glVertexShader                       = 0x8b31
	glFragmentShader                     = 0x8b30
	glGeometryShader                     = 0x8dd9
	glTessControlShader                  = 0x8e88
	glTessEvaluationShader               = 0x8e87
	glCompileStatus                      = 0x8b81
	glLinkStatus                         = 0x8b82
	glInfoLogLength                      = 0x8b84
	glColorBufferBit                     = 0x00004000
	glDepthBufferBit                     = 0x00000100
	glStencilBufferBit                   = 0x00000400
	glPoints                             = 0x0000
	glLines                              = 0x0001
	glLineLoop                           = 0x0002
	glLineStrip                          = 0x0003
	glTriangles                          = 0x0004
	glTriangleStrip                      = 0x0005
	glTriangleFan                        = 0x0006
	glLinesAdjacency                     = 0x000a
	glLineStripAdjacency                 = 0x000b
	glTrianglesAdjacency                 = 0x000c
	glTriangleStripAdjacency             = 0x000d
	glPatches                            = 0x000e
	glPatchVertices                      = 0x8e72
	glPatchDefaultInnerLevel             = 0x8e73
	glPatchDefaultOuterLevel             = 0x8e74
	glCullFace                           = 0x0b44
	glFront                              = 0x0404
	glBack                               = 0x0405
	glFrontAndBack                       = 0x0408
	glCW                                 = 0x0900
	glCCW                                = 0x0901
	glDepthTest                          = 0x0b71
	glStencilTest                        = 0x0b90
	glBlend                              = 0x0be2
	glDither                             = 0x0bd0
	glScissorTest                        = 0x0c11
	glMultisample                        = 0x809d
	glSampleMask                         = 0x8e51
	glSampleShading                      = 0x8c36
	glMinSampleShadingValue              = 0x8c37
	glPrimitiveRestart                   = 0x8f9d
	glProgramPointSize                   = 0x8642
	glRasterizerDiscard                  = 0x8c89
	glDepthClamp                         = 0x864f
	glClipDistance0                      = 0x3000
	glTextureCubeMapSeamless             = 0x884f
	glTransformFeedbackBuffer            = 0x8c8e
	glTransformFeedback                  = 0x8e22
	glInterleavedAttribs                 = 0x8c8c
	glPointSpriteCoordOrigin             = 0x8ca0
	glLowerLeft                          = 0x8ca1
	glUpperLeft                          = 0x8ca2
	glPolygonOffsetFill                  = 0x8037
	glNever                              = 0x0200
	glLess                               = 0x0201
	glEqual                              = 0x0202
	glLEqual                             = 0x0203
	glGreater                            = 0x0204
	glNotEqual                           = 0x0205
	glGEqual                             = 0x0206
	glAlways                             = 0x0207
	glKeep                               = 0x1e00
	glReplace                            = 0x1e01
	glIncrement                          = 0x1e02
	glDecrement                          = 0x1e03
	glIncrementWrap                      = 0x8507
	glDecrementWrap                      = 0x8508
	glInvert                             = 0x150a
	glStencilIndex                       = 0x1901
	glColor                              = 0x1800
	glUnpackAlignment                    = 0x0cf5
	glPackAlignment                      = 0x0d05
	glTextureMinFilter                   = 0x2801
	glTextureMagFilter                   = 0x2800
	glTextureMinLOD                      = 0x813a
	glTextureMaxLOD                      = 0x813b
	glTextureLODBias                     = 0x8501
	glTextureBaseLevel                   = 0x813c
	glTextureMaxLevel                    = 0x813d
	glTextureBorderColor                 = 0x1004
	glTextureCompareMode                 = 0x884c
	glTextureCompareFunc                 = 0x884d
	glTextureMaxAnisotropyExt            = 0x84fe
	glCompareRefToTexture                = 0x884e
	glNone                               = 0
	glLinear                             = 0x2601
	glNearest                            = 0x2600
	glNearestMipmapNearest               = 0x2700
	glLinearMipmapNearest                = 0x2701
	glNearestMipmapLinear                = 0x2702
	glLinearMipmapLinear                 = 0x2703
	glRepeat                             = 0x2901
	glClampToEdge                        = 0x812f
	glClampToBorder                      = 0x812d
	glMirroredRepeat                     = 0x8370
	glTextureWrapS                       = 0x2802
	glTextureWrapT                       = 0x2803
	glTextureWrapR                       = 0x8072
	glTextureSwizzleR                    = 0x8e42
	glTextureSwizzleG                    = 0x8e43
	glTextureSwizzleB                    = 0x8e44
	glTextureSwizzleA                    = 0x8e45
	glRed                                = 0x1903
	glRedInteger                         = 0x8d94
	glGreen                              = 0x1904
	glBlue                               = 0x1905
	glAlpha                              = 0x1906
	glZero                               = 0
	glOne                                = 1
	glSrcColor                           = 0x0300
	glOneMinusSrcColor                   = 0x0301
	glSrcAlpha                           = 0x0302
	glOneMinusSrcAlpha                   = 0x0303
	glDstAlpha                           = 0x0304
	glOneMinusDstAlpha                   = 0x0305
	glDstColor                           = 0x0306
	glOneMinusDstColor                   = 0x0307
	glSrcAlphaSaturate                   = 0x0308
	glConstantColor                      = 0x8001
	glOneMinusConstantColor              = 0x8002
	glConstantAlpha                      = 0x8003
	glOneMinusConstantAlpha              = 0x8004
	glSrc1Color                          = 0x88f9
	glOneMinusSrc1Color                  = 0x88fa
	glSrc1Alpha                          = 0x8589
	glOneMinusSrc1Alpha                  = 0x88fb
	glFuncAdd                            = 0x8006
	glMin                                = 0x8007
	glMax                                = 0x8008
	glFuncSubtract                       = 0x800a
	glFuncReverseSubtract                = 0x800b
	glSyncGPUCommandsComplete            = 0x9117
	glSamplesPassed                      = 0x8914
	glAnySamplesPassed                   = 0x8c2f
	glAnySamplesPassedConservative       = 0x8d6a
	glQueryResult                        = 0x8866
	glQueryResultAvailable               = 0x8867
	glTimeElapsed                        = 0x88bf
	glTimestamp                          = 0x8e28
	glPrimitivesGenerated                = 0x8c87
	glTransformFeedbackPrimitivesWritten = 0x8c88
	glQueryWait                          = 0x8e13
	glQueryNoWait                        = 0x8e14
	glQueryByRegionWait                  = 0x8e15
	glQueryByRegionNoWait                = 0x8e16
	glTimeoutIgnored                     = ^uint64(0)
)

type hostGL struct {
	genTextures                     func(int32, *uint32)
	deleteTextures                  func(int32, *uint32)
	bindTexture                     func(uint32, uint32)
	activeTexture                   func(uint32)
	texImage1D                      func(uint32, int32, int32, int32, int32, uint32, uint32, uintptr)
	texSubImage1D                   func(uint32, int32, int32, int32, uint32, uint32, uintptr)
	texImage2D                      func(uint32, int32, int32, int32, int32, int32, uint32, uint32, uintptr)
	texSubImage2D                   func(uint32, int32, int32, int32, int32, int32, uint32, uint32, uintptr)
	texImage3D                      func(uint32, int32, int32, int32, int32, int32, int32, uint32, uint32, uintptr)
	texSubImage3D                   func(uint32, int32, int32, int32, int32, int32, int32, int32, uint32, uint32, uintptr)
	getTexImage                     func(uint32, int32, uint32, uint32, uintptr)
	texImage2DMultisample           func(uint32, int32, uint32, int32, int32, bool)
	texImage3DMultisample           func(uint32, int32, uint32, int32, int32, int32, bool)
	texParameteri                   func(uint32, uint32, int32)
	texParameterf                   func(uint32, uint32, float32)
	texParameterfv                  func(uint32, uint32, *float32)
	texBuffer                       func(uint32, uint32, uint32)
	genSamplers                     func(int32, *uint32)
	deleteSamplers                  func(int32, *uint32)
	bindSampler                     func(uint32, uint32)
	samplerParameteri               func(uint32, uint32, int32)
	samplerParameterf               func(uint32, uint32, float32)
	samplerParameterfv              func(uint32, uint32, *float32)
	getSamplerParameterfv           func(uint32, uint32, *float32)
	pixelStorei                     func(uint32, int32)
	genBuffers                      func(int32, *uint32)
	deleteBuffers                   func(int32, *uint32)
	bindBuffer                      func(uint32, uint32)
	bufferData                      func(uint32, int, uintptr, uint32)
	bufferSubData                   func(uint32, int, int, uintptr)
	getBufferSubData                func(uint32, int, int, uintptr)
	bindBufferRange                 func(uint32, uint32, uint32, int, int)
	genVertexArrays                 func(int32, *uint32)
	deleteVertexArrays              func(int32, *uint32)
	bindVertexArray                 func(uint32)
	vertexAttribPtr                 func(uint32, int32, uint32, bool, int32, uintptr)
	vertexAttribIPtr                func(uint32, int32, uint32, int32, uintptr)
	vertexAttrib4f                  func(uint32, float32, float32, float32, float32)
	vertexAttribI4i                 func(uint32, int32, int32, int32, int32)
	vertexAttribI4ui                func(uint32, uint32, uint32, uint32, uint32)
	vertexAttribDivisor             func(uint32, uint32)
	enableVertexAttrib              func(uint32)
	disableVertexAttrib             func(uint32)
	genFramebuffers                 func(int32, *uint32)
	deleteFramebuffers              func(int32, *uint32)
	bindFramebuffer                 func(uint32, uint32)
	framebufferTexture1D            func(uint32, uint32, uint32, uint32, int32)
	framebufferTexture              func(uint32, uint32, uint32, uint32, int32)
	framebufferTextureAll           func(uint32, uint32, uint32, int32)
	framebufferTextureLayer         func(uint32, uint32, uint32, int32, int32)
	checkFramebuffer                func(uint32) uint32
	blitFramebuffer                 func(int32, int32, int32, int32, int32, int32, int32, int32, uint32, uint32)
	createShader                    func(uint32) uint32
	shaderSource                    func(uint32, int32, **byte, *int32)
	compileShader                   func(uint32)
	getShaderiv                     func(uint32, uint32, *int32)
	getShaderInfoLog                func(uint32, int32, *int32, *byte)
	deleteShader                    func(uint32)
	createProgram                   func() uint32
	attachShader                    func(uint32, uint32)
	linkProgram                     func(uint32)
	transformFeedbackVaryings       func(uint32, int32, **byte, uint32)
	getProgramiv                    func(uint32, uint32, *int32)
	getProgramInfoLog               func(uint32, int32, *int32, *byte)
	deleteProgram                   func(uint32)
	useProgram                      func(uint32)
	getUniformLocation              func(uint32, *byte) int32
	getUniformBlockIndex            func(uint32, *byte) uint32
	getActiveUniformBlockiv         func(uint32, uint32, uint32, *int32)
	uniformBlockBinding             func(uint32, uint32, uint32)
	uniformMatrix4fv                func(int32, int32, bool, *float32)
	uniform4fv                      func(int32, int32, *float32)
	uniform4iv                      func(int32, int32, *int32)
	uniform1f                       func(int32, float32)
	uniform1i                       func(int32, int32)
	viewport                        func(int32, int32, int32, int32)
	viewportIndexedf                func(uint32, float32, float32, float32, float32)
	pointSize                       func(float32)
	pointParameteri                 func(uint32, int32)
	sampleMaski                     func(uint32, uint32)
	minSampleShading                func(float32)
	patchParameteri                 func(uint32, int32)
	patchParameterfv                func(uint32, *float32)
	depthRange                      func(float64, float64)
	depthRangeIndexed               func(uint32, float64, float64)
	scissor                         func(int32, int32, int32, int32)
	scissorIndexed                  func(uint32, int32, int32, int32, int32)
	clearColor                      func(float32, float32, float32, float32)
	clearDepth                      func(float64)
	clearStencil                    func(int32)
	clear                           func(uint32)
	enable                          func(uint32)
	disable                         func(uint32)
	cullFace                        func(uint32)
	frontFace                       func(uint32)
	depthFunc                       func(uint32)
	depthMask                       func(bool)
	stencilFuncSeparate             func(uint32, uint32, int32, uint32)
	stencilOpSeparate               func(uint32, uint32, uint32, uint32)
	stencilMaskSeparate             func(uint32, uint32)
	blendColor                      func(float32, float32, float32, float32)
	blendFuncSeparate               func(uint32, uint32, uint32, uint32)
	blendEquationSeparate           func(uint32, uint32)
	colorMask                       func(bool, bool, bool, bool)
	enablei                         func(uint32, uint32)
	disablei                        func(uint32, uint32)
	blendFuncSeparatei              func(uint32, uint32, uint32, uint32, uint32)
	blendEquationSeparatei          func(uint32, uint32, uint32)
	colorMaski                      func(uint32, bool, bool, bool, bool)
	drawArrays                      func(uint32, int32, int32)
	drawArraysInstanced             func(uint32, int32, int32, int32)
	drawArraysIndirect              func(uint32, uintptr)
	drawElements                    func(uint32, int32, uint32, uintptr)
	drawElementsInstanced           func(uint32, int32, uint32, uintptr, int32)
	drawElementsBaseVertex          func(uint32, int32, uint32, uintptr, int32)
	drawElementsInstancedBaseVertex func(uint32, int32, uint32, uintptr, int32, int32)
	drawElementsIndirect            func(uint32, uint32, uintptr)
	beginTransformFeedback          func(uint32)
	endTransformFeedback            func()
	genTransformFeedbacks           func(int32, *uint32)
	deleteTransformFeedbacks        func(int32, *uint32)
	bindTransformFeedback           func(uint32, uint32)
	pauseTransformFeedback          func()
	resumeTransformFeedback         func()
	genQueries                      func(int32, *uint32)
	deleteQueries                   func(int32, *uint32)
	beginQuery                      func(uint32, uint32)
	endQuery                        func(uint32)
	beginQueryIndexed               func(uint32, uint32, uint32)
	endQueryIndexed                 func(uint32, uint32)
	queryCounter                    func(uint32, uint32)
	getQueryObjectuiv               func(uint32, uint32, *uint32)
	getQueryObjectui64v             func(uint32, uint32, *uint64)
	beginConditionalRender          func(uint32, uint32)
	endConditionalRender            func()
	primitiveRestartIndex           func(uint32)
	polygonOffset                   func(float32, float32)
	readPixels                      func(int32, int32, int32, int32, uint32, uint32, uintptr)
	clearBufferfv                   func(uint32, int32, *float32)
	clearBufferiv                   func(uint32, int32, *int32)
	clearBufferuiv                  func(uint32, int32, *uint32)
	drawBuffer                      func(uint32)
	drawBuffers                     func(int32, *uint32)
	readBuffer                      func(uint32)
	fenceSync                       func(uint32, uint32) uintptr
	waitSync                        func(uintptr, uint32, uint64)
	deleteSync                      func(uintptr)
	flush                           func()
	finish                          func()
	getError                        func() uint32
	getFloatv                       func(uint32, *float32)
	isEnabled                       func(uint32) bool
}

func loadHostGL() (*hostGL, error) {
	handle, err := purego.Dlopen("/System/Library/Frameworks/OpenGL.framework/OpenGL", purego.RTLD_GLOBAL|purego.RTLD_LAZY)
	if err != nil {
		return nil, fmt.Errorf("load OpenGL for VirGL: %w", err)
	}
	register := func(destination any, name string) {
		purego.RegisterLibFunc(destination, handle, name)
	}
	gl := &hostGL{}
	register(&gl.genTextures, "glGenTextures")
	register(&gl.deleteTextures, "glDeleteTextures")
	register(&gl.bindTexture, "glBindTexture")
	register(&gl.activeTexture, "glActiveTexture")
	register(&gl.texImage1D, "glTexImage1D")
	register(&gl.texSubImage1D, "glTexSubImage1D")
	register(&gl.texImage2D, "glTexImage2D")
	register(&gl.texSubImage2D, "glTexSubImage2D")
	register(&gl.texImage3D, "glTexImage3D")
	register(&gl.texSubImage3D, "glTexSubImage3D")
	register(&gl.getTexImage, "glGetTexImage")
	register(&gl.texImage2DMultisample, "glTexImage2DMultisample")
	register(&gl.texImage3DMultisample, "glTexImage3DMultisample")
	register(&gl.texParameteri, "glTexParameteri")
	register(&gl.texParameterf, "glTexParameterf")
	register(&gl.texParameterfv, "glTexParameterfv")
	register(&gl.texBuffer, "glTexBuffer")
	register(&gl.genSamplers, "glGenSamplers")
	register(&gl.deleteSamplers, "glDeleteSamplers")
	register(&gl.bindSampler, "glBindSampler")
	register(&gl.samplerParameteri, "glSamplerParameteri")
	register(&gl.samplerParameterf, "glSamplerParameterf")
	register(&gl.samplerParameterfv, "glSamplerParameterfv")
	register(&gl.getSamplerParameterfv, "glGetSamplerParameterfv")
	register(&gl.pixelStorei, "glPixelStorei")
	register(&gl.genBuffers, "glGenBuffers")
	register(&gl.deleteBuffers, "glDeleteBuffers")
	register(&gl.bindBuffer, "glBindBuffer")
	register(&gl.bufferData, "glBufferData")
	register(&gl.bufferSubData, "glBufferSubData")
	register(&gl.getBufferSubData, "glGetBufferSubData")
	register(&gl.bindBufferRange, "glBindBufferRange")
	register(&gl.genVertexArrays, "glGenVertexArrays")
	register(&gl.deleteVertexArrays, "glDeleteVertexArrays")
	register(&gl.bindVertexArray, "glBindVertexArray")
	register(&gl.vertexAttribPtr, "glVertexAttribPointer")
	register(&gl.vertexAttribIPtr, "glVertexAttribIPointer")
	register(&gl.vertexAttrib4f, "glVertexAttrib4f")
	register(&gl.vertexAttribI4i, "glVertexAttribI4i")
	register(&gl.vertexAttribI4ui, "glVertexAttribI4ui")
	register(&gl.vertexAttribDivisor, "glVertexAttribDivisor")
	register(&gl.enableVertexAttrib, "glEnableVertexAttribArray")
	register(&gl.disableVertexAttrib, "glDisableVertexAttribArray")
	register(&gl.genFramebuffers, "glGenFramebuffers")
	register(&gl.deleteFramebuffers, "glDeleteFramebuffers")
	register(&gl.bindFramebuffer, "glBindFramebuffer")
	register(&gl.framebufferTexture1D, "glFramebufferTexture1D")
	register(&gl.framebufferTexture, "glFramebufferTexture2D")
	register(&gl.framebufferTextureAll, "glFramebufferTexture")
	register(&gl.framebufferTextureLayer, "glFramebufferTextureLayer")
	register(&gl.checkFramebuffer, "glCheckFramebufferStatus")
	register(&gl.blitFramebuffer, "glBlitFramebuffer")
	register(&gl.createShader, "glCreateShader")
	register(&gl.shaderSource, "glShaderSource")
	register(&gl.compileShader, "glCompileShader")
	register(&gl.getShaderiv, "glGetShaderiv")
	register(&gl.getShaderInfoLog, "glGetShaderInfoLog")
	register(&gl.deleteShader, "glDeleteShader")
	register(&gl.createProgram, "glCreateProgram")
	register(&gl.attachShader, "glAttachShader")
	register(&gl.linkProgram, "glLinkProgram")
	register(&gl.transformFeedbackVaryings, "glTransformFeedbackVaryings")
	register(&gl.getProgramiv, "glGetProgramiv")
	register(&gl.getProgramInfoLog, "glGetProgramInfoLog")
	register(&gl.deleteProgram, "glDeleteProgram")
	register(&gl.useProgram, "glUseProgram")
	register(&gl.getUniformLocation, "glGetUniformLocation")
	register(&gl.getUniformBlockIndex, "glGetUniformBlockIndex")
	register(&gl.getActiveUniformBlockiv, "glGetActiveUniformBlockiv")
	register(&gl.uniformBlockBinding, "glUniformBlockBinding")
	register(&gl.uniformMatrix4fv, "glUniformMatrix4fv")
	register(&gl.uniform4fv, "glUniform4fv")
	register(&gl.uniform4iv, "glUniform4iv")
	register(&gl.uniform1f, "glUniform1f")
	register(&gl.uniform1i, "glUniform1i")
	register(&gl.viewport, "glViewport")
	register(&gl.viewportIndexedf, "glViewportIndexedf")
	register(&gl.pointSize, "glPointSize")
	register(&gl.pointParameteri, "glPointParameteri")
	register(&gl.sampleMaski, "glSampleMaski")
	register(&gl.minSampleShading, "glMinSampleShading")
	register(&gl.patchParameteri, "glPatchParameteri")
	register(&gl.patchParameterfv, "glPatchParameterfv")
	register(&gl.depthRange, "glDepthRange")
	register(&gl.depthRangeIndexed, "glDepthRangeIndexed")
	register(&gl.scissor, "glScissor")
	register(&gl.scissorIndexed, "glScissorIndexed")
	register(&gl.clearColor, "glClearColor")
	register(&gl.clearDepth, "glClearDepth")
	register(&gl.clearStencil, "glClearStencil")
	register(&gl.clear, "glClear")
	register(&gl.enable, "glEnable")
	register(&gl.disable, "glDisable")
	register(&gl.cullFace, "glCullFace")
	register(&gl.frontFace, "glFrontFace")
	register(&gl.depthFunc, "glDepthFunc")
	register(&gl.depthMask, "glDepthMask")
	register(&gl.stencilFuncSeparate, "glStencilFuncSeparate")
	register(&gl.stencilOpSeparate, "glStencilOpSeparate")
	register(&gl.stencilMaskSeparate, "glStencilMaskSeparate")
	register(&gl.blendColor, "glBlendColor")
	register(&gl.blendFuncSeparate, "glBlendFuncSeparate")
	register(&gl.blendEquationSeparate, "glBlendEquationSeparate")
	register(&gl.colorMask, "glColorMask")
	register(&gl.enablei, "glEnablei")
	register(&gl.disablei, "glDisablei")
	register(&gl.blendFuncSeparatei, "glBlendFuncSeparatei")
	register(&gl.blendEquationSeparatei, "glBlendEquationSeparatei")
	register(&gl.colorMaski, "glColorMaski")
	register(&gl.drawArrays, "glDrawArrays")
	register(&gl.drawArraysInstanced, "glDrawArraysInstanced")
	register(&gl.drawArraysIndirect, "glDrawArraysIndirect")
	register(&gl.drawElements, "glDrawElements")
	register(&gl.drawElementsInstanced, "glDrawElementsInstanced")
	register(&gl.drawElementsBaseVertex, "glDrawElementsBaseVertex")
	register(&gl.drawElementsInstancedBaseVertex, "glDrawElementsInstancedBaseVertex")
	register(&gl.drawElementsIndirect, "glDrawElementsIndirect")
	register(&gl.beginTransformFeedback, "glBeginTransformFeedback")
	register(&gl.endTransformFeedback, "glEndTransformFeedback")
	register(&gl.genTransformFeedbacks, "glGenTransformFeedbacks")
	register(&gl.deleteTransformFeedbacks, "glDeleteTransformFeedbacks")
	register(&gl.bindTransformFeedback, "glBindTransformFeedback")
	register(&gl.pauseTransformFeedback, "glPauseTransformFeedback")
	register(&gl.resumeTransformFeedback, "glResumeTransformFeedback")
	register(&gl.genQueries, "glGenQueries")
	register(&gl.deleteQueries, "glDeleteQueries")
	register(&gl.beginQuery, "glBeginQuery")
	register(&gl.endQuery, "glEndQuery")
	register(&gl.beginQueryIndexed, "glBeginQueryIndexed")
	register(&gl.endQueryIndexed, "glEndQueryIndexed")
	register(&gl.queryCounter, "glQueryCounter")
	register(&gl.getQueryObjectuiv, "glGetQueryObjectuiv")
	register(&gl.getQueryObjectui64v, "glGetQueryObjectui64v")
	register(&gl.beginConditionalRender, "glBeginConditionalRender")
	register(&gl.endConditionalRender, "glEndConditionalRender")
	register(&gl.primitiveRestartIndex, "glPrimitiveRestartIndex")
	register(&gl.polygonOffset, "glPolygonOffset")
	register(&gl.readPixels, "glReadPixels")
	register(&gl.clearBufferfv, "glClearBufferfv")
	register(&gl.clearBufferiv, "glClearBufferiv")
	register(&gl.clearBufferuiv, "glClearBufferuiv")
	register(&gl.drawBuffer, "glDrawBuffer")
	register(&gl.drawBuffers, "glDrawBuffers")
	register(&gl.readBuffer, "glReadBuffer")
	register(&gl.fenceSync, "glFenceSync")
	register(&gl.waitSync, "glWaitSync")
	register(&gl.deleteSync, "glDeleteSync")
	register(&gl.flush, "glFlush")
	register(&gl.finish, "glFinish")
	register(&gl.getError, "glGetError")
	register(&gl.getFloatv, "glGetFloatv")
	register(&gl.isEnabled, "glIsEnabled")
	return gl, nil
}

func (gl *hostGL) compileProgram(vertexSource, fragmentSource string) (uint32, error) {
	return gl.compileProgramWithTransformFeedback(vertexSource, fragmentSource, nil)
}

func (gl *hostGL) compileProgramWithTransformFeedback(vertexSource, fragmentSource string, varyings []string) (uint32, error) {
	return gl.compileProgramWithGeometryTransformFeedback(vertexSource, "", fragmentSource, varyings)
}

func (gl *hostGL) compileProgramWithGeometryTransformFeedback(vertexSource, geometrySource, fragmentSource string, varyings []string) (uint32, error) {
	return gl.compileProgramWithTessellationGeometryTransformFeedback(vertexSource, "", "", geometrySource, fragmentSource, varyings)
}

func (gl *hostGL) compileProgramWithTessellationGeometryTransformFeedback(vertexSource, tessControlSource, tessEvaluationSource, geometrySource, fragmentSource string, varyings []string) (uint32, error) {
	compile := func(kind uint32, source string) (uint32, error) {
		shader := gl.createShader(kind)
		sourceBytes := []byte(source)
		sourcePointer := &sourceBytes[0]
		length := int32(len(sourceBytes))
		gl.shaderSource(shader, 1, &sourcePointer, &length)
		gl.compileShader(shader)
		var status int32
		gl.getShaderiv(shader, glCompileStatus, &status)
		if status == 0 {
			return 0, fmt.Errorf("compile VirGL host shader: %s", gl.shaderLog(shader))
		}
		return shader, nil
	}
	vertex, err := compile(glVertexShader, vertexSource)
	if err != nil {
		return 0, err
	}
	defer gl.deleteShader(vertex)
	var tessControl uint32
	if tessControlSource != "" {
		tessControl, err = compile(glTessControlShader, tessControlSource)
		if err != nil {
			return 0, err
		}
		defer gl.deleteShader(tessControl)
	}
	var tessEvaluation uint32
	if tessEvaluationSource != "" {
		tessEvaluation, err = compile(glTessEvaluationShader, tessEvaluationSource)
		if err != nil {
			return 0, err
		}
		defer gl.deleteShader(tessEvaluation)
	}
	var geometry uint32
	if geometrySource != "" {
		geometry, err = compile(glGeometryShader, geometrySource)
		if err != nil {
			return 0, err
		}
		defer gl.deleteShader(geometry)
	}
	fragment, err := compile(glFragmentShader, fragmentSource)
	if err != nil {
		return 0, err
	}
	defer gl.deleteShader(fragment)
	program := gl.createProgram()
	gl.attachShader(program, vertex)
	if tessControl != 0 {
		gl.attachShader(program, tessControl)
	}
	if tessEvaluation != 0 {
		gl.attachShader(program, tessEvaluation)
	}
	if geometry != 0 {
		gl.attachShader(program, geometry)
	}
	gl.attachShader(program, fragment)
	if len(varyings) != 0 {
		names := make([][]byte, len(varyings))
		pointers := make([]*byte, len(varyings))
		for index, varying := range varyings {
			names[index] = append([]byte(varying), 0)
			pointers[index] = &names[index][0]
		}
		gl.transformFeedbackVaryings(program, int32(len(pointers)), &pointers[0], glInterleavedAttribs)
	}
	gl.linkProgram(program)
	var status int32
	gl.getProgramiv(program, glLinkStatus, &status)
	if status == 0 {
		defer gl.deleteProgram(program)
		return 0, fmt.Errorf("link VirGL host program: %s", gl.programLog(program))
	}
	return program, nil
}

func (gl *hostGL) shaderLog(shader uint32) string {
	var length int32
	gl.getShaderiv(shader, glInfoLogLength, &length)
	if length <= 1 {
		return "no shader log"
	}
	result := make([]byte, length)
	gl.getShaderInfoLog(shader, length, &length, &result[0])
	return string(result[:length])
}

func (gl *hostGL) programLog(program uint32) string {
	var length int32
	gl.getProgramiv(program, glInfoLogLength, &length)
	if length <= 1 {
		return "no program log"
	}
	result := make([]byte, length)
	gl.getProgramInfoLog(program, length, &length, &result[0])
	return string(result[:length])
}

func glPointer(bytes []byte) uintptr {
	if len(bytes) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&bytes[0]))
}

func glPointerUint32(words []uint32) uintptr {
	if len(words) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&words[0]))
}

func uniformLocation(gl *hostGL, program uint32, name string) int32 {
	bytes := append([]byte(name), 0)
	return gl.getUniformLocation(program, &bytes[0])
}
