//go:build darwin

package virgl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"math/bits"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

var errDarwinAcceleratedOpenGLUnavailable = errors.New("create accelerated VirGL pixel format")

const (
	nsOpenGLPFAAccelerated       = 73
	nsOpenGLPFAColorSize         = 8
	nsOpenGLPFADepthSize         = 12
	nsOpenGLPFAStencilSize       = 13
	nsOpenGLPFAOpenGLProfile     = 99
	nsOpenGLProfileVersion41Core = 0x4100
	maxHostPrograms              = 64
	emulatedVertexTableEntries   = 4096
)

type hostRequest struct {
	run  func() error
	done chan error
}

type darwinHost struct {
	requests                chan hostRequest
	stop                    chan struct{}
	done                    chan struct{}
	once                    sync.Once
	gl                      *hostGL
	vao                     uint32
	emulatedVertexIDBuffer  uint32
	emulatedVertexTable     uint32
	constantStagingBuffers  [5][16]uint32
	constantBufferTextures  [5][16]uint32
	retiredConstantBuffers  []uint32
	zeroUniformBuffer       uint32
	enabledVertexAttributes uint32
	blitReadFBO             uint32
	blitDrawFBO             uint32
	depthOnlyFBO            uint32
	discardFBO              uint32
	discardTexture          uint32
	framebufferBindingValid bool
	boundFramebuffer        uint32
	boundColorAttachments   [8]hostFramebufferAttachment
	boundDepthTexture       uint32
	boundDepthAttachment    uint32
	boundDepthSurface       hostFramebufferAttachment
	currentProgram          uint32
	programs                map[hostProgramKey]hostProgram
	programUseSequence      uint64
	contexts                map[uint32]*hostContext
	activeContext           *hostContext
	resources               map[uint32]*hostResource
	allResources            map[*hostResource]struct{}
	sharedPresentation      bool
	nativeFrames            [3]hostNativeFrame
	pendingBufferTransfers  []hostBufferTransfer
}

type hostBufferTransfer struct {
	resource    *hostResource
	description virtio.GPUResource3D
	data        []byte
	transfer    virtio.GPUTransfer3D
}

type hostNativeFrame struct {
	texture       uint32
	width         int
	height        int
	producerFence uintptr
	consumerFence uintptr
	inUse         bool
}

type hostResource struct {
	description           virtio.GPUResource3D
	texture               uint32
	textureTarget         uint32
	buffer                uint32
	bufferBytes           []byte
	bufferDirty           bool
	bufferDirtyStart      uint32
	bufferDirtyEnd        uint32
	framebuffer           uint32
	depth                 bool
	stencil               bool
	packedStencil         bool
	emulatedIntegerMSAA   bool
	references            int
	samplerViewConfigured bool
	appliedSamplerView    hostSamplerView
}

type hostSurface struct {
	resourceID uint32
	resource   *hostResource
	format     uint32
	level      uint32
	firstLayer uint32
	lastLayer  uint32
}

type hostFramebufferAttachment struct {
	texture uint32
	target  uint32
	level   uint32
	layer   uint32
	layered bool
}

type hostSamplerView struct {
	resourceID uint32
	resource   *hostResource
	texture    uint32
	target     uint32
	format     uint32
	firstLevel uint32
	lastLevel  uint32
	firstLayer uint32
	lastLayer  uint32
	swizzle    [4]uint32
}

type hostSamplerState struct {
	id          uint32
	state       uint32
	lodBias     float32
	minLOD      float32
	maxLOD      float32
	borderColor [4]float32
}

type hostBlendState struct {
	state         uint32
	renderTargets [8]uint32
}

type hostDepthStencilAlpha struct {
	state   uint32
	stencil [2]uint32
}

type hostVertexElement struct {
	offset          uint32
	instanceDivisor uint32
	bufferIndex     uint32
	format          uint32
}

type hostVertexBuffer struct {
	stride     uint32
	offset     uint32
	resourceID uint32
	resource   *hostResource
}

type hostShader struct {
	stage                     uint32
	tgsi                      string
	source                    string
	generation                uint64
	streamOutputVaryings      []string
	streamOutputBufferStrides [4]uint32
	maxInterleavedWorkaround  bool
	geometryOutputMode        uint32
	tessEvaluationOutputMode  uint32
}

type hostShaderAssembly struct {
	stage                     uint32
	numTokens                 uint32
	totalBytes                uint32
	nextOffset                uint32
	text                      []byte
	streamOutputs             []tgsiStreamOutput
	streamOutputBufferStrides [4]uint32
}

type hostStreamoutTarget struct {
	resourceID uint32
	resource   *hostResource
	offset     uint32
	size       uint32
}

const (
	hostStreamoutNeedBegin uint8 = iota
	hostStreamoutPaused
)

type hostStreamoutObject struct {
	id                 uint32
	handles            [4]uint32
	state              uint8
	writtenBytes       [4]uint32
	appendOffsetsValid bool
}

type hostActiveStreamout struct {
	maxInterleavedWorkaround bool
	stagingBuffers           [2]uint32
	target                   hostStreamoutTarget
	vertexCapacity           uint32
	object                   *hostStreamoutObject
	persistent               bool
	advanceBytes             [4]uint32
	advanceOffsetsValid      bool
}

type hostQuery struct {
	id         uint32
	queryType  uint32
	index      uint32
	target     uint32
	resultSize uint32
	resource   *hostResource
	offset     uint32
	active     bool
	ended      bool
}

type hostProgram struct {
	id                    uint32
	lastUsed              uint64
	constants             [5][16]int32
	constantUniforms      [5][16]int32
	constantSizes         [5][16]int32
	constantTextures      [5][16]int32
	winsysAdjustY         int32
	samplers              [5][16]int32
	explicitLODCrossovers [5][16]int32
	samplerSampleCounts   [5][16]int32
	samplerLevelCounts    [5][16]int32
	samplerViewSwizzles   [5][16]int32
}

type hostUniformBuffer struct {
	resourceID uint32
	resource   *hostResource
	offset     uint32
	length     uint32
}

type hostProgramKey struct {
	context                                                                               *hostContext
	vertexHandle, fragmentHandle, geometryHandle, tessControlHandle, tessEvaluationHandle uint32
	vertexGeneration, fragmentGeneration, geometryGeneration                              uint64
	tessControlGeneration, tessEvaluationGeneration                                       uint64
	pointSpriteCoordinates                                                                uint32
	fragmentOutputClasses                                                                 [8]uint8
	dualSourceBlend                                                                       bool
	emulatedVertexSystemValue, emulatedVertexAttribute                                    uint8
	signedVertexInputs, unsignedVertexInputs                                              uint16
	disableStreamout                                                                      bool
}

type hostRasterizer struct {
	state                  uint32
	pointSize              float32
	spriteCoordinateEnable uint32
	clipPlaneEnable        uint8
	offsetUnits            float32
	offsetScale            float32
}

type hostScissor struct {
	minX, minY uint32
	maxX, maxY uint32
}

type hostViewport struct {
	x, y          int32
	width, height int32
	adjustY       float32
	near, far     float64
}

type hostContext struct {
	subcontexts           map[uint32]*hostContext
	activeSubcontext      uint32
	blendStates           map[uint32]hostBlendState
	surfaces              map[uint32]hostSurface
	samplerViews          map[uint32]hostSamplerView
	samplerStates         map[uint32]hostSamplerState
	depthStencilAlpha     map[uint32]hostDepthStencilAlpha
	vertexElements        map[uint32][]hostVertexElement
	rasterizers           map[uint32]hostRasterizer
	shaders               map[uint32]hostShader
	shaderAssemblies      map[uint32]*hostShaderAssembly
	streamoutTargets      map[uint32]hostStreamoutTarget
	streamoutObjects      map[[4]uint32]*hostStreamoutObject
	currentStreamout      *hostStreamoutObject
	queries               map[uint32]hostQuery
	activeQueries         map[uint64]uint32
	conditionalQuery      uint32
	boundStreamoutTargets [4]uint32
	boundShaders          [6]uint32
	boundVertexElements   uint32
	boundBlend            uint32
	boundDSA              uint32
	boundRasterizer       uint32
	colorSurfaces         [8]uint32
	depthSurface          uint32
	blendColor            [4]float32
	stencilRef            [2]uint8
	scissors              [16]hostScissor
	viewports             [16]hostViewport
	viewportSet           uint16
	vertexBuffers         [16]hostVertexBuffer
	indexBuffer           uint32
	indexResource         *hostResource
	indexSize             uint32
	indexOffset           uint32
	constants             [5][16][]float32
	uniformBuffers        [5][16]hostUniformBuffer
	tessFactors           [6]float32
	boundSamplerViews     [6][16]uint32
	boundSamplerStates    [6][16]uint32
	nextShaderGeneration  uint64
}

func NewHostRenderer() (virtio.GPURenderer, error) {
	return NewHostRendererWithShareGroup(0, 0)
}

func NewHostRendererWithShareGroup(shareContext, sharePixelFormat uintptr) (virtio.GPURenderer, error) {
	host, err := newDarwinHost(shareContext, sharePixelFormat)
	if err != nil {
		return nil, err
	}
	backend, err := captureHostFromEnvironment(host)
	if err != nil {
		_ = host.close()
		return nil, err
	}
	return NewRenderer(backend), nil
}

func newDarwinHost(shareGroup ...uintptr) (*darwinHost, error) {
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_GLOBAL|purego.RTLD_LAZY); err != nil {
		return nil, fmt.Errorf("load AppKit for VirGL: %w", err)
	}
	gl, err := loadHostGL()
	if err != nil {
		return nil, err
	}
	host := &darwinHost{
		requests:     make(chan hostRequest),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		gl:           gl,
		programs:     make(map[hostProgramKey]hostProgram),
		contexts:     make(map[uint32]*hostContext),
		resources:    make(map[uint32]*hostResource),
		allResources: make(map[*hostResource]struct{}),
	}
	var shareContext uintptr
	var sharePixelFormat uintptr
	if len(shareGroup) != 0 {
		shareContext = shareGroup[0]
	}
	if len(shareGroup) > 1 {
		sharePixelFormat = shareGroup[1]
	}
	host.sharedPresentation = shareContext != 0 && sharePixelFormat != 0
	ready := make(chan error, 1)
	go host.contextLoop(shareContext, sharePixelFormat, ready)
	if err := <-ready; err != nil {
		<-host.done
		return nil, err
	}
	return host, nil
}

func (h *darwinHost) contextLoop(shareContext, sharePixelFormat uintptr, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(h.done)

	alloc := objc.RegisterName("alloc")
	initSelector := objc.RegisterName("init")
	release := objc.RegisterName("release")
	initWithAttributes := objc.RegisterName("initWithAttributes:")
	initWithFormat := objc.RegisterName("initWithFormat:shareContext:")
	makeCurrent := objc.RegisterName("makeCurrentContext")
	clearCurrent := objc.RegisterName("clearCurrentContext")

	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(alloc)
	pool = pool.Send(initSelector)
	if pool == 0 {
		ready <- errors.New("create VirGL autorelease pool")
		return
	}
	defer pool.Send(release)

	attributes := []uint32{
		nsOpenGLPFAAccelerated,
		nsOpenGLPFAColorSize, 24,
		nsOpenGLPFADepthSize, 24,
		nsOpenGLPFAStencilSize, 8,
		nsOpenGLPFAOpenGLProfile, nsOpenGLProfileVersion41Core,
		0,
	}
	format := objc.ID(sharePixelFormat)
	if format == 0 {
		format = objc.ID(objc.GetClass("NSOpenGLPixelFormat")).Send(alloc)
		format = format.Send(initWithAttributes, unsafe.Pointer(&attributes[0]))
		if format == 0 {
			ready <- errDarwinAcceleratedOpenGLUnavailable
			return
		}
		defer format.Send(release)
	}

	context := objc.ID(objc.GetClass("NSOpenGLContext")).Send(alloc)
	context = context.Send(initWithFormat, format, objc.ID(shareContext))
	if context == 0 {
		ready <- errors.New("create VirGL OpenGL 4.1 context")
		return
	}
	defer context.Send(release)
	context.Send(makeCurrent)

	h.gl.genVertexArrays(1, &h.vao)
	h.gl.bindVertexArray(h.vao)
	h.gl.genFramebuffers(1, &h.blitReadFBO)
	h.gl.genFramebuffers(1, &h.blitDrawFBO)
	h.gl.genFramebuffers(1, &h.depthOnlyFBO)
	h.gl.genTextures(1, &h.discardTexture)
	h.gl.bindTexture(glTexture2D, h.discardTexture)
	h.gl.texImage2D(glTexture2D, 0, glRGBA8, 1, 1, 0, glRGBA, glUnsignedByte, 0)
	h.gl.genFramebuffers(1, &h.discardFBO)
	h.gl.bindFramebuffer(glFramebuffer, h.discardFBO)
	h.gl.framebufferTexture(glFramebuffer, glColorAttachment0, glTexture2D, h.discardTexture, 0)
	h.gl.bindFramebuffer(glFramebuffer, 0)
	h.gl.pixelStorei(glPackAlignment, 1)
	h.gl.pixelStorei(glUnpackAlignment, 1)
	h.gl.enable(glProgramPointSize)
	ready <- nil
	defer func() {
		h.releaseGLObjects()
		objc.ID(objc.GetClass("NSOpenGLContext")).Send(clearCurrent)
	}()

	for {
		select {
		case request := <-h.requests:
			context.Send(makeCurrent)
			request.done <- request.run()
		case <-h.stop:
			return
		}
	}
}

func (h *darwinHost) dispatch(run func() error) error {
	request := hostRequest{
		done: make(chan error, 1),
		run:  run,
	}
	select {
	case h.requests <- request:
	case <-h.done:
		return errors.New("VirGL OpenGL context is closed")
	}
	select {
	case err := <-request.done:
		return err
	case <-h.done:
		return errors.New("VirGL OpenGL context closed during submission")
	}
}

func (h *darwinHost) createContext(id uint32) error {
	return h.dispatch(func() error {
		if h.contexts[id] != nil {
			return fmt.Errorf("Darwin VirGL context %d already exists", id)
		}
		h.contexts[id] = newHostContext()
		return nil
	})
}

func (h *darwinHost) destroyContext(id uint32) error {
	return h.dispatch(func() error {
		context := h.contexts[id]
		if context == nil {
			return fmt.Errorf("unknown Darwin VirGL context %d", id)
		}
		h.releaseContextResources(context)
		if h.activeContext == context {
			h.activeContext = nil
		}
		delete(h.contexts, id)
		return nil
	})
}

func (h *darwinHost) createResource(description virtio.GPUResource3D) error {
	return h.dispatch(func() error {
		if h.resources[description.ID] != nil {
			return fmt.Errorf("Darwin VirGL resource %d already exists", description.ID)
		}
		hostResource := &hostResource{description: description, references: 1}
		switch description.Target {
		case 0:
			if uint64(int(description.Width)) != uint64(description.Width) {
				return fmt.Errorf("VirGL buffer resource %d is too large", description.ID)
			}
			hostResource.bufferBytes = make([]byte, int(description.Width))
			h.gl.genBuffers(1, &hostResource.buffer)
			h.gl.bindBuffer(glArrayBuffer, hostResource.buffer)
			h.gl.bufferData(glArrayBuffer, int(description.Width), 0, glStreamDraw)
		case 1, 2, 3, 4, 5, 6, 7, 8:
			multisample := description.Samples != 0
			hostResource.emulatedIntegerMSAA = multisample && isIntegerTextureFormat(description.Format)
			if multisample && (description.Samples > 4 || (description.Target != 2 && description.Target != 7) || description.LastLevel != 0) {
				return fmt.Errorf("VirGL multisample resource %d has unsupported target %d, sample count %d, or last level %d",
					description.ID, description.Target, description.Samples, description.LastLevel)
			}
			maxTextureDimension := uint32(capsetMaxTexture2D)
			if description.Target == 3 {
				maxTextureDimension = capsetMaxTexture3D
			}
			if description.Width > maxTextureDimension || description.Height > maxTextureDimension ||
				(description.Target == 3 && description.Depth > maxTextureDimension) {
				return fmt.Errorf("VirGL texture resource %d dimensions %dx%d exceed %d",
					description.ID, description.Width, description.Height, maxTextureDimension)
			}
			if description.Target == 1 && (description.Height != 1 || description.Depth != 1 || description.ArraySize != 1) {
				return fmt.Errorf("VirGL 1D resource %d must have height, depth, and array size 1, got %d, %d, %d",
					description.ID, description.Height, description.Depth, description.ArraySize)
			}
			if description.Target == 4 && (description.Width != description.Height ||
				(description.ArraySize != 1 && description.ArraySize != 6)) {
				return fmt.Errorf("VirGL cube resource %d must be square with one cube, got %dx%d array size %d",
					description.ID, description.Width, description.Height, description.ArraySize)
			}
			if description.Target == 8 && (description.Width != description.Height || description.ArraySize == 0 ||
				description.ArraySize > capsetMaxArrayLayers || description.ArraySize%6 != 0) {
				return fmt.Errorf("VirGL cube-array resource %d must be square with a positive multiple-of-six layer count no greater than %d, got %dx%d array size %d",
					description.ID, capsetMaxArrayLayers, description.Width, description.Height, description.ArraySize)
			}
			if description.Target == 7 && (description.ArraySize == 0 || description.ArraySize > capsetMaxArrayLayers) {
				return fmt.Errorf("VirGL 2D array resource %d has invalid layer count %d", description.ID, description.ArraySize)
			}
			if description.Target == 6 && (description.Height != 1 || description.Depth != 1 ||
				description.ArraySize == 0 || description.ArraySize > capsetMaxArrayLayers) {
				return fmt.Errorf("VirGL 1D array resource %d has invalid height %d, depth %d, or layer count %d",
					description.ID, description.Height, description.Depth, description.ArraySize)
			}
			if description.Target == 5 && (description.Depth != 1 || description.ArraySize != 1 || description.LastLevel != 0) {
				return fmt.Errorf("VirGL rectangle resource %d must have depth and array size 1 and no mipmaps, got %d, %d, level %d",
					description.ID, description.Depth, description.ArraySize, description.LastLevel)
			}
			if description.Target == 3 && description.ArraySize != 1 {
				return fmt.Errorf("VirGL 3D resource %d must have array size 1, got %d", description.ID, description.ArraySize)
			}
			maxDimension := description.Width
			if description.Target != 6 && description.Height > maxDimension {
				maxDimension = description.Height
			}
			if description.Target == 3 && description.Depth > maxDimension {
				maxDimension = description.Depth
			}
			maxLevel := uint32(0)
			for dimension := maxDimension; dimension > 1; dimension >>= 1 {
				maxLevel++
			}
			if description.LastLevel > maxLevel {
				return fmt.Errorf("VirGL resource %d last mip level %d exceeds %d for %dx%d texture",
					description.ID, description.LastLevel, maxLevel, description.Width, description.Height)
			}
			h.gl.genTextures(1, &hostResource.texture)
			// Apple's rectangle path rejects otherwise valid integer textures at
			// sampling time. Keep the VirGL RECT contract in the translated shader,
			// but use ordinary 2D storage and normalize coordinates there.
			hostResource.textureTarget = glTexture2D
			if description.Target == 1 {
				hostResource.textureTarget = glTexture1D
			} else if description.Target == 3 {
				hostResource.textureTarget = glTexture3D
			} else if description.Target == 4 {
				hostResource.textureTarget = glTextureCubeMap
			} else if description.Target == 6 {
				hostResource.textureTarget = glTexture1DArray
			} else if description.Target == 7 {
				hostResource.textureTarget = glTexture2DArray
			} else if description.Target == 8 {
				hostResource.textureTarget = glTextureCubeMapArray
			}
			if hostResource.emulatedIntegerMSAA && description.Target == 2 {
				hostResource.textureTarget = glTexture2DArray
			} else if multisample && description.Target == 2 {
				hostResource.textureTarget = glTexture2DMultisample
			} else if multisample && description.Target == 7 && !hostResource.emulatedIntegerMSAA {
				hostResource.textureTarget = glTexture2DMultisampleArray
			}
			h.gl.bindTexture(hostResource.textureTarget, hostResource.texture)
			if !multisample {
				h.gl.texParameteri(hostResource.textureTarget, glTextureMinFilter, glLinear)
				h.gl.texParameteri(hostResource.textureTarget, glTextureMagFilter, glLinear)
				h.gl.texParameteri(hostResource.textureTarget, glTextureBaseLevel, 0)
				h.gl.texParameteri(hostResource.textureTarget, glTextureMaxLevel, int32(description.LastLevel))
			}
			allocateLevels := func(internalFormat int32, format, dataType uint32) {
				if hostResource.emulatedIntegerMSAA {
					layers := description.Samples
					if description.Target == 7 {
						layers *= description.ArraySize
					}
					h.gl.texImage3D(glTexture2DArray, 0, internalFormat, int32(description.Width), int32(description.Height), int32(layers), 0, format, dataType, 0)
					return
				}
				if multisample {
					if description.Target == 2 {
						h.gl.texImage2DMultisample(glTexture2DMultisample, int32(description.Samples), uint32(internalFormat), int32(description.Width), int32(description.Height), true)
					} else {
						h.gl.texImage3DMultisample(glTexture2DMultisampleArray, int32(description.Samples), uint32(internalFormat), int32(description.Width), int32(description.Height), int32(description.ArraySize), true)
					}
					return
				}
				for level := uint32(0); level <= description.LastLevel; level++ {
					width, height := description.Width>>level, description.Height>>level
					if width == 0 {
						width = 1
					}
					if height == 0 {
						height = 1
					}
					depth := description.Depth >> level
					if depth == 0 {
						depth = 1
					}
					if description.Target == 1 {
						h.gl.texImage1D(glTexture1D, int32(level), internalFormat, int32(width), 0, format, dataType, 0)
					} else if description.Target == 3 {
						h.gl.texImage3D(glTexture3D, int32(level), internalFormat, int32(width), int32(height), int32(depth), 0, format, dataType, 0)
					} else if description.Target == 4 {
						for face := uint32(0); face < 6; face++ {
							h.gl.texImage2D(glTextureCubeMapPositiveX+face, int32(level), internalFormat, int32(width), int32(height), 0, format, dataType, 0)
						}
					} else if description.Target == 6 {
						h.gl.texImage2D(glTexture1DArray, int32(level), internalFormat, int32(width), int32(description.ArraySize), 0, format, dataType, 0)
					} else if description.Target == 7 {
						h.gl.texImage3D(glTexture2DArray, int32(level), internalFormat, int32(width), int32(height), int32(description.ArraySize), 0, format, dataType, 0)
					} else if description.Target == 8 {
						h.gl.texImage3D(glTextureCubeMapArray, int32(level), internalFormat, int32(width), int32(height), int32(description.ArraySize), 0, format, dataType, 0)
					} else {
						h.gl.texImage2D(glTexture2D, int32(level), internalFormat, int32(width), int32(height), 0, format, dataType, 0)
					}
				}
			}
			nativeFormat, ok := describeDarwinTextureFormat(description.Format)
			if !ok {
				return fmt.Errorf("VirGL resource %d uses unsupported Darwin texture format %d", description.ID, description.Format)
			}
			hostResource.depth = nativeFormat.depth
			hostResource.stencil = nativeFormat.stencil
			hostResource.packedStencil = nativeFormat.packedStencil
			storageExternal, storageType := nativeFormat.external, nativeFormat.dataType
			if nativeFormat.packedStencil {
				storageExternal, storageType = glDepthStencil, glUnsignedInt248
			}
			allocateLevels(nativeFormat.internal, storageExternal, storageType)
			if nativeFormat.render && !hostResource.depth && !hostResource.stencil {
				h.gl.genFramebuffers(1, &hostResource.framebuffer)
				if description.Target == 2 {
					h.framebufferBindingValid = false
					h.gl.bindFramebuffer(glFramebuffer, hostResource.framebuffer)
					if hostResource.emulatedIntegerMSAA {
						h.gl.framebufferTextureLayer(glFramebuffer, glColorAttachment0, hostResource.texture, 0, 0)
					} else {
						h.gl.framebufferTexture(glFramebuffer, glColorAttachment0, hostResource.textureTarget, hostResource.texture, 0)
					}
					if status := h.gl.checkFramebuffer(glFramebuffer); status != glFramebufferComplete {
						return fmt.Errorf("VirGL resource %d framebuffer status %#x", description.ID, status)
					}
				}
			}
		default:
			return fmt.Errorf("VirGL resource %d target %d is not supported by the Darwin backend", description.ID, description.Target)
		}
		h.resources[description.ID] = hostResource
		h.allResources[hostResource] = struct{}{}
		return nil
	})
}

func (h *darwinHost) unrefResource(id uint32) error {
	return h.dispatch(func() error {
		resource := h.resources[id]
		if resource == nil {
			return fmt.Errorf("unknown Darwin VirGL resource %d", id)
		}
		// A guest handle may be released while a vertex buffer, index buffer,
		// sampler view, or surface still retains the host resource. Preserve
		// transfer ordering by committing queued writes before removing the
		// guest-visible handle; dropping them leaves retained bindings with
		// stale contents.
		if err := h.flushPendingBufferTransfers(); err != nil {
			return err
		}
		delete(h.resources, id)
		h.releaseResource(resource)
		return nil
	})
}

func (h *darwinHost) transferToHost(resource *resource, transfer virtio.GPUTransfer3D) error {
	return h.dispatch(func() error {
		hostResource := h.resources[resource.description.ID]
		if hostResource == nil {
			return fmt.Errorf("unknown Darwin VirGL resource %d", resource.description.ID)
		}
		return h.uploadResource(hostResource, resource.description, resource.data, transfer)
	})
}

func (h *darwinHost) queueBufferTransfer(resource *resource, transfer virtio.GPUTransfer3D) error {
	hostResource := h.resources[resource.description.ID]
	if hostResource == nil || hostResource.buffer == 0 {
		return fmt.Errorf("unknown Darwin VirGL buffer resource %d", resource.description.ID)
	}
	box := transfer.Box
	if box.X > resource.description.Width || box.Width > resource.description.Width-box.X {
		return fmt.Errorf("buffer transfer range %d..%d exceeds resource size %d",
			box.X, uint64(box.X)+uint64(box.Width), resource.description.Width)
	}
	end := transfer.Offset + uint64(box.Width)
	if end < transfer.Offset || end > uint64(len(resource.data)) {
		return fmt.Errorf("buffer transfer backing range %d..%d exceeds %d bytes",
			transfer.Offset, end, len(resource.data))
	}
	h.pendingBufferTransfers = append(h.pendingBufferTransfers, hostBufferTransfer{
		resource:    hostResource,
		description: resource.description,
		data:        resource.data,
		transfer:    transfer,
	})
	return nil
}

func (h *darwinHost) uploadResource(hostResource *hostResource, description virtio.GPUResource3D, data []byte, transfer virtio.GPUTransfer3D) error {
	switch {
	case hostResource.buffer != 0:
		box := transfer.Box
		if box.X > description.Width || box.Width > description.Width-box.X {
			return fmt.Errorf("buffer transfer range %d..%d exceeds resource size %d",
				box.X, uint64(box.X)+uint64(box.Width), description.Width)
		}
		end := transfer.Offset + uint64(box.Width)
		if end < transfer.Offset || end > uint64(len(data)) {
			return fmt.Errorf("buffer transfer backing range %d..%d exceeds %d bytes",
				transfer.Offset, end, len(data))
		}
		bytes := data[int(transfer.Offset):int(end)]
		copy(hostResource.bufferBytes[int(box.X):int(box.X+box.Width)], bytes)
		h.markBufferDirty(hostResource, box.X, box.Width)
	case hostResource.texture != 0:
		if description.Samples != 0 {
			return fmt.Errorf("direct upload to multisample VirGL resource %d is unsupported", description.ID)
		}
		box := transfer.Box
		validLayerRange := box.Depth > 0
		switch description.Target {
		case 1:
			validLayerRange = box.Depth == 1 && box.Z == 0 && box.Height == 1 && box.Y == 0
		case 2:
			validLayerRange = box.Depth == 1 && box.Z == 0
		case 3:
			levelDepth := description.Depth >> transfer.Level
			if levelDepth == 0 {
				levelDepth = 1
			}
			validLayerRange = box.Z <= levelDepth && box.Depth <= levelDepth-box.Z
		case 4:
			validLayerRange = box.Depth == 1 && box.Z < 6
		case 5:
			validLayerRange = box.Depth == 1 && box.Z == 0
		case 6:
			validLayerRange = box.Y == 0 && box.Height == 1 && box.Z <= description.ArraySize && box.Depth <= description.ArraySize-box.Z
		case 7, 8:
			validLayerRange = box.Z <= description.ArraySize && box.Depth <= description.ArraySize-box.Z
		}
		if !validLayerRange {
			return fmt.Errorf("Darwin VirGL texture transfer targets invalid layer %d depth %d for target %d", box.Z, box.Depth, description.Target)
		}
		levelWidth, levelHeight := description.Width>>transfer.Level, description.Height>>transfer.Level
		if levelWidth == 0 {
			levelWidth = 1
		}
		if levelHeight == 0 {
			levelHeight = 1
		}
		if transfer.Level > description.LastLevel ||
			box.X > levelWidth || box.Width > levelWidth-box.X ||
			box.Y > levelHeight || box.Height > levelHeight-box.Y {
			return fmt.Errorf("texture transfer box exceeds mip level %d dimensions %dx%d",
				transfer.Level, levelWidth, levelHeight)
		}
		bytesPerPixel := textureFormatBytes(description.Format)
		rowBytes := uint64(box.Width) * bytesPerPixel
		stride := uint64(transfer.Stride)
		if stride == 0 {
			stride = uint64(levelWidth) * bytesPerPixel
		}
		if stride < rowBytes {
			return fmt.Errorf("texture transfer stride %d is smaller than row size %d", stride, rowBytes)
		}
		layerSpan := rowBytes
		if box.Height > 1 {
			layerSpan += uint64(box.Height-1) * stride
		}
		layerStride := uint64(transfer.LayerStride)
		if layerStride == 0 {
			layerStride = stride * uint64(levelHeight)
		}
		if box.Depth > 1 && layerStride < layerSpan {
			return fmt.Errorf("texture transfer layer stride %d is smaller than layer size %d", layerStride, layerSpan)
		}
		required := layerSpan + uint64(box.Depth-1)*layerStride
		end := transfer.Offset + required
		if end < transfer.Offset || end > uint64(len(data)) {
			return fmt.Errorf("texture transfer backing range %d..%d exceeds %d bytes",
				transfer.Offset, end, len(data))
		}
		bytes := data[int(transfer.Offset):int(end)]
		packedLayer := rowBytes * uint64(box.Height)
		if stride != rowBytes || layerStride != packedLayer {
			packed := make([]byte, int(packedLayer)*int(box.Depth))
			for layer := uint32(0); layer < box.Depth; layer++ {
				for row := uint32(0); row < box.Height; row++ {
					sourceOffset := int(uint64(layer)*layerStride + uint64(row)*stride)
					destinationOffset := int(uint64(layer)*packedLayer + uint64(row)*rowBytes)
					copy(packed[destinationOffset:destinationOffset+int(rowBytes)],
						bytes[sourceOffset:sourceOffset+int(rowBytes)])
				}
			}
			bytes = packed
		}
		h.gl.bindTexture(hostResource.textureTarget, hostResource.texture)
		nativeFormat, ok := describeDarwinTextureFormat(description.Format)
		if !ok {
			return fmt.Errorf("unsupported Darwin texture transfer format %d", description.Format)
		}
		format, dataType := nativeFormat.external, nativeFormat.dataType
		if description.Format == virglFormatZ24X8UNorm {
			scaled := make([]byte, len(bytes))
			for offset := 0; offset+4 <= len(bytes); offset += 4 {
				value := uint64(binary.LittleEndian.Uint32(bytes[offset:]) & 0x00ffffff)
				value = (value*math.MaxUint32 + 0x007fffff) / 0x00ffffff
				binary.LittleEndian.PutUint32(scaled[offset:], uint32(value))
			}
			bytes = scaled
		} else if description.Format == virglFormatZ24UNormS8UInt {
			repacked := make([]byte, len(bytes))
			for offset := 0; offset+4 <= len(bytes); offset += 4 {
				value := binary.LittleEndian.Uint32(bytes[offset:])
				binary.LittleEndian.PutUint32(repacked[offset:], value<<8|value>>24)
			}
			bytes = repacked
		}
		if hostResource.stencil && !hostResource.depth {
			if hostResource.packedStencil {
				packed := make([]byte, len(bytes)*4)
				for index, value := range bytes {
					binary.LittleEndian.PutUint32(packed[index*4:], 0xffffff00|uint32(value))
				}
				bytes = packed
				format, dataType = glDepthStencil, glUnsignedInt248
			} else {
				format, dataType = glStencilIndex, glUnsignedByte
			}
		}
		imageTarget := hostResource.textureTarget
		if description.Target == 4 {
			imageTarget = glTextureCubeMapPositiveX + box.Z
		}
		if description.Target == 1 {
			h.gl.texSubImage1D(imageTarget, int32(transfer.Level), int32(box.X), int32(box.Width), format, dataType, glPointer(bytes))
		} else if description.Target == 3 {
			h.gl.texSubImage3D(imageTarget, int32(transfer.Level), int32(box.X), int32(box.Y), int32(box.Z),
				int32(box.Width), int32(box.Height), int32(box.Depth), format, dataType, glPointer(bytes))
		} else if description.Target == 6 {
			h.gl.texSubImage2D(imageTarget, int32(transfer.Level), int32(box.X), int32(box.Z),
				int32(box.Width), int32(box.Depth), format, dataType, glPointer(bytes))
		} else if description.Target == 7 || description.Target == 8 {
			h.gl.texSubImage3D(imageTarget, int32(transfer.Level), int32(box.X), int32(box.Y), int32(box.Z),
				int32(box.Width), int32(box.Height), int32(box.Depth), format, dataType, glPointer(bytes))
		} else {
			h.gl.texSubImage2D(imageTarget, int32(transfer.Level), int32(box.X), int32(box.Y),
				int32(box.Width), int32(box.Height), format, dataType, glPointer(bytes))
		}
	}
	return nil
}

func (h *darwinHost) transferFromHost(resource *resource, transfer virtio.GPUTransfer3D) error {
	return h.dispatch(func() error {
		if err := h.flushPendingBufferTransfers(); err != nil {
			return err
		}
		hostResource := h.resources[resource.description.ID]
		if hostResource == nil {
			return fmt.Errorf("unknown Darwin VirGL resource %d", resource.description.ID)
		}

		switch {
		case hostResource.buffer != 0:
			box := transfer.Box
			if box.X > resource.description.Width || box.Width > resource.description.Width-box.X {
				return fmt.Errorf("buffer transfer range %d..%d exceeds resource size %d",
					box.X, uint64(box.X)+uint64(box.Width), resource.description.Width)
			}
			end := transfer.Offset + uint64(box.Width)
			if end < transfer.Offset || end > uint64(len(resource.data)) {
				return fmt.Errorf("buffer transfer backing range %d..%d exceeds %d bytes",
					transfer.Offset, end, len(resource.data))
			}
			if box.Width == 0 {
				return nil
			}
			h.publishBuffer(hostResource)
			bytes := resource.data[int(transfer.Offset):int(end)]
			h.gl.bindBuffer(glArrayBuffer, hostResource.buffer)
			h.gl.getBufferSubData(glArrayBuffer, int(box.X), len(bytes), glPointer(bytes))
			copy(hostResource.bufferBytes[int(box.X):int(box.X+box.Width)], bytes)
			return nil

		case hostResource.texture != 0:
			if resource.description.Samples != 0 {
				return fmt.Errorf("direct readback from multisample VirGL resource %d requires a resolve", resource.description.ID)
			}
			box := transfer.Box
			validLayerRange := box.Depth > 0
			switch resource.description.Target {
			case 1:
				validLayerRange = box.Depth == 1 && box.Z == 0 && box.Height == 1 && box.Y == 0
			case 2:
				validLayerRange = box.Depth == 1 && box.Z == 0
			case 3:
				levelDepth := resource.description.Depth >> transfer.Level
				if levelDepth == 0 {
					levelDepth = 1
				}
				validLayerRange = box.Z <= levelDepth && box.Depth <= levelDepth-box.Z
			case 4:
				validLayerRange = box.Depth == 1 && box.Z < 6
			case 5:
				validLayerRange = box.Depth == 1 && box.Z == 0
			case 6:
				validLayerRange = box.Y == 0 && box.Height == 1 && box.Z <= resource.description.ArraySize && box.Depth <= resource.description.ArraySize-box.Z
			case 7, 8:
				validLayerRange = box.Z <= resource.description.ArraySize && box.Depth <= resource.description.ArraySize-box.Z
			}
			if !validLayerRange {
				return fmt.Errorf("Darwin VirGL texture transfer targets invalid layer %d depth %d for target %d", box.Z, box.Depth, resource.description.Target)
			}
			levelWidth, levelHeight := resource.description.Width>>transfer.Level, resource.description.Height>>transfer.Level
			if levelWidth == 0 {
				levelWidth = 1
			}
			if levelHeight == 0 {
				levelHeight = 1
			}
			if transfer.Level > resource.description.LastLevel ||
				box.X > levelWidth || box.Width > levelWidth-box.X ||
				box.Y > levelHeight || box.Height > levelHeight-box.Y {
				return fmt.Errorf("texture transfer box exceeds mip level %d dimensions %dx%d",
					transfer.Level, levelWidth, levelHeight)
			}
			bytesPerPixel := textureFormatBytes(resource.description.Format)
			rowBytes := uint64(box.Width) * bytesPerPixel
			stride := uint64(transfer.Stride)
			if stride == 0 {
				stride = uint64(levelWidth) * bytesPerPixel
			}
			if stride < rowBytes {
				return fmt.Errorf("texture transfer stride %d is smaller than row size %d", stride, rowBytes)
			}
			layerSpan := rowBytes
			if box.Height > 1 {
				layerSpan += uint64(box.Height-1) * stride
			}
			layerStride := uint64(transfer.LayerStride)
			if layerStride == 0 {
				layerStride = stride * uint64(levelHeight)
			}
			if box.Depth > 1 && layerStride < layerSpan {
				return fmt.Errorf("texture transfer layer stride %d is smaller than layer size %d", layerStride, layerSpan)
			}
			required := layerSpan + uint64(box.Depth-1)*layerStride
			end := transfer.Offset + required
			if end < transfer.Offset || end > uint64(len(resource.data)) {
				return fmt.Errorf("texture transfer backing range %d..%d exceeds %d bytes",
					transfer.Offset, end, len(resource.data))
			}
			if box.Width == 0 || box.Height == 0 {
				return nil
			}

			packedLayer := rowBytes * uint64(box.Height)
			packed := make([]byte, int(packedLayer)*int(box.Depth))
			attachment := uint32(glColorAttachment0)
			nativeFormat, ok := describeDarwinTextureFormat(resource.description.Format)
			if !ok {
				return fmt.Errorf("unsupported Darwin texture readback format %d", resource.description.Format)
			}
			format, dataType := nativeFormat.external, nativeFormat.dataType
			if hostResource.depth {
				attachment = glDepthAttachment
			}
			if hostResource.depth && hostResource.stencil {
				attachment = glDepthStencilAttachment
			} else if hostResource.stencil {
				if hostResource.packedStencil {
					attachment = glDepthStencilAttachment
				} else {
					attachment = glStencilAttachment
				}
			}

			h.framebufferBindingValid = false
			if resource.description.Format == virglFormatR9G9B9E5Float &&
				(resource.description.Target == 2 || resource.description.Target == 5) {
				full := make([]byte, int(levelWidth)*int(levelHeight)*4)
				h.gl.bindTexture(hostResource.textureTarget, hostResource.texture)
				h.drainGLErrors()
				h.gl.getTexImage(hostResource.textureTarget, int32(transfer.Level), format, dataType, glPointer(full))
				if glError := h.gl.getError(); glError != 0 {
					return fmt.Errorf("VirGL shared-exponent texture readback GL error %#x", glError)
				}
				for row := uint32(0); row < box.Height; row++ {
					source := (int(box.Y+row)*int(levelWidth) + int(box.X)) * 4
					destination := int(row) * int(rowBytes)
					copy(packed[destination:destination+int(rowBytes)], full[source:source+int(rowBytes)])
				}
			} else {
				for layer := uint32(0); layer < box.Depth; layer++ {
					h.gl.bindFramebuffer(glReadFramebuffer, h.blitReadFBO)
					h.gl.framebufferTexture(glReadFramebuffer, glColorAttachment0, glTexture2D, 0, 0)
					h.gl.framebufferTexture(glReadFramebuffer, glDepthAttachment, glTexture2D, 0, 0)
					h.gl.framebufferTexture(glReadFramebuffer, glStencilAttachment, glTexture2D, 0, 0)
					h.gl.framebufferTexture(glReadFramebuffer, glDepthStencilAttachment, glTexture2D, 0, 0)
					if resource.description.Target == 1 {
						h.gl.framebufferTexture1D(glReadFramebuffer, attachment, glTexture1D, hostResource.texture, int32(transfer.Level))
					} else if resource.description.Target == 3 || resource.description.Target == 6 || resource.description.Target == 7 || resource.description.Target == 8 {
						h.gl.framebufferTextureLayer(glReadFramebuffer, attachment, hostResource.texture, int32(transfer.Level), int32(box.Z+layer))
					} else {
						imageTarget := hostResource.textureTarget
						if resource.description.Target == 4 {
							imageTarget = glTextureCubeMapPositiveX + box.Z
						}
						h.gl.framebufferTexture(glReadFramebuffer, attachment, imageTarget, hostResource.texture, int32(transfer.Level))
					}
					if attachment == glColorAttachment0 {
						h.gl.readBuffer(glColorAttachment0)
					} else {
						h.gl.readBuffer(glNone)
					}
					if status := h.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
						return fmt.Errorf("VirGL transfer source framebuffer status %#x", status)
					}
					h.gl.finish()
					destination := uint64(layer) * packedLayer
					h.gl.readPixels(int32(box.X), int32(box.Y), int32(box.Width), int32(box.Height),
						format, dataType, glPointer(packed[int(destination):]))
				}
			}
			if resource.description.Format == virglFormatZ24X8UNorm {
				for offset := 0; offset+4 <= len(packed); offset += 4 {
					value := uint64(binary.LittleEndian.Uint32(packed[offset:]))
					value = (value*0x00ffffff + math.MaxUint32/2) / math.MaxUint32
					binary.LittleEndian.PutUint32(packed[offset:], uint32(value))
				}
			} else if resource.description.Format == virglFormatZ24UNormS8UInt {
				for offset := 0; offset+4 <= len(packed); offset += 4 {
					value := binary.LittleEndian.Uint32(packed[offset:])
					binary.LittleEndian.PutUint32(packed[offset:], value>>8|value<<24)
				}
			}
			for layer := uint32(0); layer < box.Depth; layer++ {
				for row := uint32(0); row < box.Height; row++ {
					source := int(uint64(layer)*packedLayer + uint64(row)*rowBytes)
					destination := int(transfer.Offset + uint64(layer)*layerStride + uint64(row)*stride)
					copy(resource.data[destination:destination+int(rowBytes)], packed[source:source+int(rowBytes)])
				}
			}
			return nil
		default:
			return fmt.Errorf("Darwin VirGL resource %d has no host storage", resource.description.ID)
		}
	})
}

func (h *darwinHost) execute(contextID uint32, commands []command, _ map[uint32]*resource) error {
	return h.dispatch(func() error {
		if err := h.flushPendingBufferTransfers(); err != nil {
			return err
		}
		root := h.contexts[contextID]
		if root == nil {
			return fmt.Errorf("unknown Darwin VirGL context %d", contextID)
		}
		for _, command := range commands {
			if command.Opcode >= 28 && command.Opcode <= 30 {
				if err := h.executeSubcontextCommand(root, command); err != nil {
					return fmt.Errorf("VirGL opcode %d object %d: %w", command.Opcode, command.Object, err)
				}
				continue
			}
			context := root.selectedContext()
			if err := h.activateContext(context); err != nil {
				return err
			}
			if err := h.executeCommand(context, command); err != nil {
				return fmt.Errorf("VirGL opcode %d object %d: %w", command.Opcode, command.Object, err)
			}
		}
		return nil
	})
}

func (h *darwinHost) readScanout(resource *resource, rect image.Rectangle) ([]byte, int, error) {
	var result []byte
	err := h.dispatch(func() error {
		width := int(resource.description.Width)
		height := int(resource.description.Height)
		rect = rect.Intersect(image.Rect(0, 0, width, height))
		if rect.Empty() {
			return errors.New("VirGL scanout rectangle is empty")
		}
		hostResource := h.resources[resource.description.ID]
		if hostResource == nil || hostResource.framebuffer == 0 {
			return fmt.Errorf("VirGL resource %d is not renderable", resource.description.ID)
		}
		raw := make([]byte, rect.Dx()*rect.Dy()*4)
		h.framebufferBindingValid = false
		h.gl.bindFramebuffer(glFramebuffer, hostResource.framebuffer)
		h.gl.finish()
		// VirGL's scanout resource stores the guest's top row at GL y=0. Keep
		// that ordering for the CPU framebuffer, whose rectangles also use a
		// top-left origin. Reversing these rows produces an upside-down desktop.
		h.gl.readPixels(int32(rect.Min.X), int32(rect.Min.Y), int32(rect.Dx()), int32(rect.Dy()), glBGRA, glUnsignedByte, glPointer(raw))
		result = raw
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return result, rect.Dx() * 4, nil
}

func (h *darwinHost) nativeScanout(resource *resource, rect image.Rectangle) (virtio.GPUNativeFrame, bool, error) {
	if !h.sharedPresentation {
		return virtio.GPUNativeFrame{}, false, nil
	}
	var frame virtio.GPUNativeFrame
	var slot *hostNativeFrame
	err := h.dispatch(func() error {
		width := int(resource.description.Width)
		height := int(resource.description.Height)
		rect = rect.Intersect(image.Rect(0, 0, width, height))
		if rect.Empty() {
			return errors.New("VirGL native scanout rectangle is empty")
		}
		hostResource := h.resources[resource.description.ID]
		if hostResource == nil || hostResource.texture == 0 || hostResource.depth {
			return fmt.Errorf("VirGL resource %d is not a color texture", resource.description.ID)
		}
		for index := range h.nativeFrames {
			if !h.nativeFrames[index].inUse {
				slot = &h.nativeFrames[index]
				break
			}
		}
		if slot == nil {
			return nil
		}
		if slot.consumerFence != 0 {
			h.gl.waitSync(slot.consumerFence, 0, glTimeoutIgnored)
			h.gl.deleteSync(slot.consumerFence)
			slot.consumerFence = 0
		}
		// A native frame is sampled by the frontend as the complete scanout.
		// RESOURCE_FLUSH may describe only a damaged subrectangle, so publishing
		// a texture sized to rect would stretch that subrectangle over the whole
		// desktop and discard every pixel outside it. Each pool slot is
		// independent, so populate a complete texture rather than relying on
		// damage retained in whichever slot happens to be free.
		if slot.texture == 0 || slot.width != width || slot.height != height {
			if slot.texture != 0 {
				h.gl.deleteTextures(1, &slot.texture)
			}
			h.gl.genTextures(1, &slot.texture)
			h.gl.bindTexture(glTexture2D, slot.texture)
			h.gl.texParameteri(glTexture2D, glTextureMinFilter, glNearest)
			h.gl.texParameteri(glTexture2D, glTextureMagFilter, glNearest)
			h.gl.texParameteri(glTexture2D, glTextureBaseLevel, 0)
			h.gl.texParameteri(glTexture2D, glTextureMaxLevel, 0)
			h.gl.texImage2D(glTexture2D, 0, glRGBA8, int32(width), int32(height), 0, glRGBA, glUnsignedByte, 0)
			slot.width, slot.height = width, height
		}
		h.framebufferBindingValid = false
		h.gl.bindFramebuffer(glReadFramebuffer, h.blitReadFBO)
		h.gl.framebufferTexture(glReadFramebuffer, glColorAttachment0, glTexture2D, hostResource.texture, 0)
		if status := h.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("VirGL native source framebuffer status %#x", status)
		}
		h.gl.bindFramebuffer(glDrawFramebuffer, h.blitDrawFBO)
		h.gl.framebufferTexture(glDrawFramebuffer, glColorAttachment0, glTexture2D, slot.texture, 0)
		if status := h.gl.checkFramebuffer(glDrawFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("VirGL native destination framebuffer status %#x", status)
		}
		h.gl.blitFramebuffer(
			0, int32(height), int32(width), 0,
			0, 0, int32(width), int32(height),
			glColorBufferBit, glNearest,
		)
		// Apple GL can make a shared texture name visible to another context
		// before its GL-on-Metal framebuffer blit has finished updating the
		// texture. A server-side wait in the consumer is not sufficient on that
		// path and presents partially updated compositor frames as black flashes
		// or trails from earlier windows. Complete the scanout copy before
		// publishing it; the fence below still preserves the native-frame lease
		// contract for consumers and future non-Apple implementations.
		h.gl.finish()
		slot.producerFence = h.gl.fenceSync(glSyncGPUCommandsComplete, 0)
		if slot.producerFence == 0 {
			return errors.New("create VirGL native frame fence")
		}
		h.gl.flush()
		slot.inUse = true
		frame = virtio.GPUNativeFrame{
			Width: width, Height: height, Damage: rect,
			Texture: slot.texture, ProducerFence: slot.producerFence,
		}
		return nil
	})
	if err != nil {
		return virtio.GPUNativeFrame{}, false, err
	}
	if slot == nil {
		return virtio.GPUNativeFrame{}, false, nil
	}
	var once sync.Once
	frame.ReleaseFrame = func(consumerFence uintptr) {
		once.Do(func() {
			_ = h.dispatch(func() error {
				if slot.producerFence != 0 {
					h.gl.deleteSync(slot.producerFence)
					slot.producerFence = 0
				}
				if slot.consumerFence != 0 {
					h.gl.deleteSync(slot.consumerFence)
				}
				slot.consumerFence = consumerFence
				slot.inUse = false
				return nil
			})
		})
	}
	return frame, true, nil
}

func (h *darwinHost) reset() error {
	return h.dispatch(func() error {
		h.pendingBufferTransfers = nil
		h.releaseNativeFrames()
		for _, resource := range h.resources {
			h.deleteResource(resource)
		}
		h.resources = make(map[uint32]*hostResource)
		h.contexts = make(map[uint32]*hostContext)
		h.activeContext = nil
		return nil
	})
}

func (h *darwinHost) flushPendingBufferTransfers() error {
	pending := h.pendingBufferTransfers
	h.pendingBufferTransfers = nil
	for _, transfer := range pending {
		if err := h.uploadResource(transfer.resource, transfer.description, transfer.data, transfer.transfer); err != nil {
			return err
		}
	}
	return nil
}

func newHostContext() *hostContext {
	return &hostContext{
		subcontexts:       make(map[uint32]*hostContext),
		blendStates:       make(map[uint32]hostBlendState),
		surfaces:          make(map[uint32]hostSurface),
		samplerViews:      make(map[uint32]hostSamplerView),
		samplerStates:     make(map[uint32]hostSamplerState),
		depthStencilAlpha: make(map[uint32]hostDepthStencilAlpha),
		vertexElements:    make(map[uint32][]hostVertexElement),
		rasterizers:       make(map[uint32]hostRasterizer),
		shaders:           make(map[uint32]hostShader),
		shaderAssemblies:  make(map[uint32]*hostShaderAssembly),
		streamoutTargets:  make(map[uint32]hostStreamoutTarget),
		streamoutObjects:  make(map[[4]uint32]*hostStreamoutObject),
		queries:           make(map[uint32]hostQuery),
		activeQueries:     make(map[uint64]uint32),
	}
}

func (c *hostContext) selectedContext() *hostContext {
	if c.activeSubcontext == 0 {
		return c
	}
	return c.subcontexts[c.activeSubcontext]
}

func (h *darwinHost) executeSubcontextCommand(root *hostContext, command command) error {
	if len(command.Payload) != 1 {
		return errors.New("invalid subcontext payload")
	}
	id := command.Payload[0]
	switch command.Opcode {
	case 28: // VIRGL_CCMD_SET_SUB_CTX
		if id != 0 && root.subcontexts[id] == nil {
			return fmt.Errorf("unknown VirGL subcontext %d", id)
		}
		root.activeSubcontext = id
		return h.activateContext(root.selectedContext())
	case 29: // VIRGL_CCMD_CREATE_SUB_CTX
		if id == 0 || root.subcontexts[id] != nil {
			return fmt.Errorf("VirGL subcontext %d already exists", id)
		}
		root.subcontexts[id] = newHostContext()
	case 30: // VIRGL_CCMD_DESTROY_SUB_CTX
		if id == 0 {
			return nil
		}
		context := root.subcontexts[id]
		if context == nil {
			return nil
		}
		if root.activeSubcontext == id {
			root.activeSubcontext = 0
		}
		if h.activeContext == context {
			h.activeContext = nil
		}
		h.releaseContextResources(context)
		delete(root.subcontexts, id)
	}
	return nil
}

func (h *darwinHost) executeCommand(context *hostContext, command command) error {
	payload := command.Payload
	switch command.Opcode {
	case 1: // VIRGL_CCMD_CREATE_OBJECT
		if len(payload) == 0 {
			return errors.New("missing object handle")
		}
		handle := payload[0]
		switch command.Object {
		case 1: // VIRGL_OBJECT_BLEND
			if len(payload) != 11 {
				return errors.New("invalid blend payload")
			}
			state := hostBlendState{state: payload[1]}
			copy(state.renderTargets[:], payload[3:])
			context.blendStates[handle] = state
		case 2: // VIRGL_OBJECT_RASTERIZER
			if len(payload) != 9 {
				return errors.New("truncated rasterizer")
			}
			context.rasterizers[handle] = hostRasterizer{
				state:                  payload[1],
				pointSize:              math.Float32frombits(payload[2]),
				spriteCoordinateEnable: payload[3],
				clipPlaneEnable:        uint8(payload[4] >> 24),
				offsetUnits:            math.Float32frombits(payload[6]),
				offsetScale:            math.Float32frombits(payload[7]),
			}
		case 3: // VIRGL_OBJECT_DSA
			if len(payload) != 5 {
				return errors.New("invalid depth/stencil/alpha payload")
			}
			context.depthStencilAlpha[handle] = hostDepthStencilAlpha{
				state: payload[1], stencil: [2]uint32{payload[2], payload[3]},
			}
		case 4: // VIRGL_OBJECT_SHADER
			generation := context.nextShaderGeneration
			if err := context.createShader(payload); err != nil {
				return err
			}
			if context.nextShaderGeneration != generation {
				h.deleteProgramsForShader(context, handle)
			}
			return nil
		case 5: // VIRGL_OBJECT_VERTEX_ELEMENTS
			if (len(payload)-1)%4 != 0 {
				return errors.New("invalid vertex element payload")
			}
			elements := make([]hostVertexElement, 0, (len(payload)-1)/4)
			for index := 1; index < len(payload); index += 4 {
				elements = append(elements, hostVertexElement{
					offset:          payload[index],
					instanceDivisor: payload[index+1],
					bufferIndex:     payload[index+2],
					format:          payload[index+3],
				})
			}
			context.vertexElements[handle] = elements
		case 6: // VIRGL_OBJECT_SAMPLER_VIEW
			if len(payload) != 6 {
				return errors.New("invalid sampler view payload")
			}
			resource := h.resources[payload[1]]
			if resource == nil {
				return fmt.Errorf("sampler view refers to unknown resource %d", payload[1])
			}
			format := payload[2] & 0x00ffffff
			firstLayer, lastLayer := payload[3]&0xffff, payload[3]>>16
			firstLevel, lastLevel := payload[4]&0xff, (payload[4]>>8)&0xff
			viewTexture, viewTarget := uint32(0), uint32(0)
			if resource.description.Target == 0 {
				elementSize := textureFormatBytes(format)
				firstElement, lastElement := payload[3], payload[4]
				if elementSize == 0 || firstElement != 0 || firstElement > lastElement || lastElement == ^uint32(0) ||
					uint64(lastElement+1)*elementSize != uint64(resource.description.Width) {
					return fmt.Errorf("texture-buffer view %d range %d..%d does not cover %d bytes in format %d",
						handle, firstElement, lastElement, resource.description.Width, format)
				}
				nativeFormat, ok := describeDarwinTextureFormat(format)
				if !ok || nativeFormat.depth || nativeFormat.stencil {
					return fmt.Errorf("texture-buffer view %d uses unsupported format %d", handle, format)
				}
				h.gl.genTextures(1, &viewTexture)
				viewTarget = glTextureBuffer
				h.gl.bindTexture(viewTarget, viewTexture)
				h.gl.texBuffer(viewTarget, uint32(nativeFormat.internal), resource.buffer)
				firstLayer, lastLayer, firstLevel, lastLevel = 0, 0, 0, 0
			} else {
				if firstLevel > lastLevel || lastLevel > resource.description.LastLevel {
					return fmt.Errorf("sampler view mip range %d..%d exceeds resource last level %d",
						firstLevel, lastLevel, resource.description.LastLevel)
				}
				if err := validateTextureLayers(resource.description, firstLayer, lastLayer); err != nil {
					return fmt.Errorf("sampler view %d: %w", handle, err)
				}
			}
			if previous, ok := context.samplerViews[handle]; ok {
				h.deleteSamplerView(previous)
				h.releaseResource(previous.resource)
			}
			h.retainResource(resource)
			context.samplerViews[handle] = hostSamplerView{
				resourceID: payload[1],
				resource:   resource,
				texture:    viewTexture,
				target:     viewTarget,
				format:     format,
				firstLevel: firstLevel,
				lastLevel:  lastLevel,
				firstLayer: firstLayer,
				lastLayer:  lastLayer,
				swizzle: [4]uint32{
					payload[5] & 7,
					(payload[5] >> 3) & 7,
					(payload[5] >> 6) & 7,
					(payload[5] >> 9) & 7,
				},
			}
		case 7: // VIRGL_OBJECT_SAMPLER_STATE
			if len(payload) != 9 {
				return errors.New("invalid sampler state payload")
			}
			state := hostSamplerState{
				state:   payload[1],
				lodBias: math.Float32frombits(payload[2]),
				minLOD:  math.Float32frombits(payload[3]),
				maxLOD:  math.Float32frombits(payload[4]),
			}
			for index := range state.borderColor {
				state.borderColor[index] = math.Float32frombits(payload[5+index])
			}
			h.gl.genSamplers(1, &state.id)
			h.applySamplerState(state)
			if previous, ok := context.samplerStates[handle]; ok {
				h.gl.deleteSamplers(1, &previous.id)
			}
			context.samplerStates[handle] = state
		case 8: // VIRGL_OBJECT_SURFACE
			if len(payload) != 2 && len(payload) != 5 {
				return errors.New("invalid surface payload")
			}
			if h.resources[payload[1]] == nil {
				return fmt.Errorf("surface refers to unknown resource %d", payload[1])
			}
			if previous, ok := context.surfaces[handle]; ok {
				h.releaseResource(previous.resource)
			}
			resource := h.resources[payload[1]]
			format, level, firstLayer, lastLayer := resource.description.Format, uint32(0), uint32(0), uint32(0)
			if len(payload) == 5 {
				format = payload[2]
				level = payload[3]
				firstLayer, lastLayer = payload[4]&0xffff, payload[4]>>16
			}
			if level > resource.description.LastLevel {
				return fmt.Errorf("surface mip level %d exceeds resource last level %d", level, resource.description.LastLevel)
			}
			if err := validateTextureLayers(resource.description, firstLayer, lastLayer); err != nil {
				return fmt.Errorf("surface %d: %w", handle, err)
			}
			h.retainResource(resource)
			context.surfaces[handle] = hostSurface{
				resourceID: payload[1], resource: resource, format: format, level: level,
				firstLayer: firstLayer, lastLayer: lastLayer,
			}
		case 9: // VIRGL_OBJECT_QUERY
			if len(payload) != 4 {
				return errors.New("invalid query payload")
			}
			queryType, index := payload[1]&0xffff, payload[1]>>16
			target, resultSize, err := hostQueryTarget(queryType)
			if err != nil {
				return err
			}
			resource := h.resources[payload[3]]
			offset := payload[2]
			if resource == nil || resource.buffer == 0 || offset%8 != 0 ||
				offset > uint32(len(resource.bufferBytes)) || 16 > uint32(len(resource.bufferBytes))-offset {
				return fmt.Errorf("query result range at %d is invalid for resource %d", offset, payload[3])
			}
			if previous, ok := context.queries[handle]; ok {
				h.gl.deleteQueries(1, &previous.id)
				h.releaseResource(previous.resource)
			}
			var id uint32
			h.gl.genQueries(1, &id)
			h.retainResource(resource)
			context.queries[handle] = hostQuery{
				id: id, queryType: queryType, index: index, target: target,
				resultSize: resultSize, resource: resource, offset: offset,
			}
			h.writeHostQueryState(resource, offset, 0, resultSize, 0)
		case 10: // VIRGL_OBJECT_STREAMOUT_TARGET
			if len(payload) != 4 {
				return errors.New("invalid streamout target payload")
			}
			resource := h.resources[payload[1]]
			if resource == nil || resource.buffer == 0 {
				return fmt.Errorf("streamout target refers to unknown buffer resource %d", payload[1])
			}
			offset, size := payload[2], payload[3]
			if offset%4 != 0 || size == 0 || offset > uint32(len(resource.bufferBytes)) || size > uint32(len(resource.bufferBytes))-offset {
				return fmt.Errorf("streamout target range %d..%d is invalid for %d-byte resource %d",
					offset, uint64(offset)+uint64(size), len(resource.bufferBytes), payload[1])
			}
			if previous, ok := context.streamoutTargets[handle]; ok {
				h.releaseResource(previous.resource)
			}
			h.retainResource(resource)
			context.streamoutTargets[handle] = hostStreamoutTarget{
				resourceID: payload[1], resource: resource, offset: offset, size: size,
			}
		default:
			return fmt.Errorf("unsupported object type %d", command.Object)
		}
	case 2: // VIRGL_CCMD_BIND_OBJECT
		if len(payload) != 1 {
			return errors.New("invalid bind payload")
		}
		switch command.Object {
		case 1:
			context.boundBlend = payload[0]
			if payload[0] == 0 {
				h.applyDefaultBlend()
				break
			}
			state, ok := context.blendStates[payload[0]]
			if !ok {
				return fmt.Errorf("unknown blend state %d", payload[0])
			}
			if err := h.applyBlend(state); err != nil {
				return err
			}
		case 2:
			context.boundRasterizer = payload[0]
			if payload[0] == 0 {
				h.gl.disable(glCullFace)
				for index := range context.scissors {
					h.gl.disablei(glScissorTest, uint32(index))
				}
				h.gl.disable(glProgramPointSize)
				h.gl.disable(glRasterizerDiscard)
				h.gl.disable(glDepthClamp)
				h.applyClipPlaneEnable(0)
				h.gl.pointSize(1)
				break
			}
			state, ok := context.rasterizers[payload[0]]
			if !ok {
				return fmt.Errorf("unknown rasterizer %d", payload[0])
			}
			h.applyRasterizer(context, state)
		case 5:
			if payload[0] == 0 {
				context.boundVertexElements = 0
				break
			}
			if context.vertexElements[payload[0]] == nil {
				return fmt.Errorf("unknown vertex elements %d", payload[0])
			}
			context.boundVertexElements = payload[0]
		case 3:
			context.boundDSA = payload[0]
			if payload[0] == 0 {
				h.gl.disable(glDepthTest)
				h.gl.depthMask(true)
				h.gl.disable(glStencilTest)
				h.gl.stencilMaskSeparate(glFrontAndBack, ^uint32(0))
				break
			}
			state, ok := context.depthStencilAlpha[payload[0]]
			if !ok {
				return fmt.Errorf("unknown depth/stencil/alpha state %d", payload[0])
			}
			h.applyDepthStencilAlpha(context, state)
		case 4, 6, 7, 8:
			// Other fixed-function state is represented by core GL defaults
			// for this first accelerated path.
		}
	case 3: // VIRGL_CCMD_DESTROY_OBJECT
		if len(payload) != 1 {
			return errors.New("invalid destroy payload")
		}
		switch command.Object {
		case 1:
			delete(context.blendStates, payload[0])
		case 2:
			delete(context.rasterizers, payload[0])
		case 3:
			delete(context.depthStencilAlpha, payload[0])
		case 4:
			h.deleteProgramsForShader(context, payload[0])
			delete(context.shaders, payload[0])
			delete(context.shaderAssemblies, payload[0])
		case 5:
			delete(context.vertexElements, payload[0])
		case 6:
			if view, ok := context.samplerViews[payload[0]]; ok {
				h.deleteSamplerView(view)
				h.releaseResource(view.resource)
			}
			delete(context.samplerViews, payload[0])
		case 7:
			if state, ok := context.samplerStates[payload[0]]; ok {
				h.gl.deleteSamplers(1, &state.id)
			}
			delete(context.samplerStates, payload[0])
		case 8:
			if surface, ok := context.surfaces[payload[0]]; ok {
				h.releaseResource(surface.resource)
			}
			delete(context.surfaces, payload[0])
		case 9:
			query, ok := context.queries[payload[0]]
			if !ok {
				break
			}
			if query.active {
				return fmt.Errorf("query %d is active", payload[0])
			}
			if context.conditionalQuery == payload[0] {
				h.gl.endConditionalRender()
				context.conditionalQuery = 0
			}
			h.gl.deleteQueries(1, &query.id)
			h.releaseResource(query.resource)
			delete(context.queries, payload[0])
		case 10:
			for key, object := range context.streamoutObjects {
				for _, handle := range object.handles {
					if handle != payload[0] {
						continue
					}
					if context.currentStreamout == object {
						h.gl.bindTransformFeedback(glTransformFeedback, 0)
						context.currentStreamout = nil
					}
					h.gl.deleteTransformFeedbacks(1, &object.id)
					delete(context.streamoutObjects, key)
					break
				}
			}
			if target, ok := context.streamoutTargets[payload[0]]; ok {
				h.releaseResource(target.resource)
			}
			delete(context.streamoutTargets, payload[0])
			for index, bound := range context.boundStreamoutTargets {
				if bound == payload[0] {
					context.boundStreamoutTargets[index] = 0
				}
			}
		default:
			return fmt.Errorf("unsupported object type %d", command.Object)
		}
	case 4: // VIRGL_CCMD_SET_VIEWPORT_STATE
		if len(payload) < 7 || (len(payload)-1)%6 != 0 {
			return errors.New("invalid viewport state")
		}
		start, count := payload[0], (len(payload)-1)/6
		if start >= uint32(len(context.viewports)) || int(start)+count > len(context.viewports) {
			return errors.New("viewport slots are out of range")
		}
		for index := 0; index < count; index++ {
			base := 1 + index*6
			scaleX := float64(math.Float32frombits(payload[base]))
			scaleY := float64(math.Float32frombits(payload[base+1]))
			scaleZ := float64(math.Float32frombits(payload[base+2]))
			translateX := float64(math.Float32frombits(payload[base+3]))
			translateY := float64(math.Float32frombits(payload[base+4]))
			translateZ := float64(math.Float32frombits(payload[base+5]))
			viewport := hostViewport{
				x:       int32(math.Round(math.Min(translateX-scaleX, translateX+scaleX))),
				y:       int32(math.Round(math.Min(translateY-scaleY, translateY+scaleY))),
				width:   int32(math.Round(math.Abs(scaleX * 2))),
				height:  int32(math.Round(math.Abs(scaleY * 2))),
				adjustY: 1,
				near:    translateZ - scaleZ,
				far:     translateZ + scaleZ,
			}
			if scaleY < 0 {
				viewport.adjustY = -1
			}
			slot := int(start) + index
			context.viewports[slot] = viewport
			context.viewportSet |= 1 << slot
			h.applyViewportIndexed(uint32(slot), viewport)
		}
	case 5: // VIRGL_CCMD_SET_FRAMEBUFFER_STATE
		if len(payload) < 2 {
			return errors.New("truncated framebuffer state")
		}
		colorCount := int(payload[0])
		if colorCount > len(context.colorSurfaces) || len(payload) != colorCount+2 {
			return fmt.Errorf("invalid framebuffer color surface count %d", colorCount)
		}
		var colorSurfaces [8]uint32
		for index, handle := range payload[2:] {
			if handle == 0 {
				continue
			}
			surface, ok := context.surfaces[handle]
			if !ok || surface.resource == nil || surface.resource.framebuffer == 0 {
				return fmt.Errorf("unknown color surface %d", handle)
			}
			colorSurfaces[index] = handle
		}
		context.colorSurfaces = colorSurfaces
		context.depthSurface = payload[1]
		return h.bindContextFramebuffer(context)
	case 6: // VIRGL_CCMD_SET_VERTEX_BUFFERS
		// A zero-length command is the Gallium unbind-all representation used
		// by draws whose vertex shader relies entirely on gl_VertexID.
		if len(payload)%3 != 0 {
			return errors.New("invalid vertex buffer state")
		}
		var bindings [16]hostVertexBuffer
		if len(payload)/3 > len(bindings) {
			return errors.New("too many vertex buffers")
		}
		for index := 0; index < len(payload)/3; index++ {
			resourceID := payload[index*3+2]
			if resourceID == 0 {
				continue
			}
			resource := h.resources[resourceID]
			if resource == nil || resource.buffer == 0 {
				return fmt.Errorf("vertex buffer %d refers to unknown buffer resource %d", index, resourceID)
			}
			bindings[index] = hostVertexBuffer{
				stride:     payload[index*3],
				offset:     payload[index*3+1],
				resourceID: resourceID,
				resource:   resource,
			}
		}
		for _, binding := range context.vertexBuffers {
			h.releaseResource(binding.resource)
		}
		for _, binding := range bindings {
			h.retainResource(binding.resource)
		}
		context.vertexBuffers = bindings
	case 7: // VIRGL_CCMD_CLEAR
		if len(payload) < 5 {
			return errors.New("truncated clear")
		}
		// Gallium may emit a clear without first forwarding the guest's newly
		// disabled rasterizer state. Match virglrenderer's clear path by
		// temporarily disabling native discard, then restoring the tracked state.
		rasterizerDiscard := context.boundRasterizer != 0 &&
			context.rasterizers[context.boundRasterizer].state&(1<<3) != 0
		if rasterizerDiscard {
			h.gl.disable(glRasterizerDiscard)
			defer h.gl.enable(glRasterizerDiscard)
		}
		if err := h.bindContextFramebuffer(context); err != nil {
			return err
		}
		h.gl.clearColor(
			math.Float32frombits(payload[1]),
			math.Float32frombits(payload[2]),
			math.Float32frombits(payload[3]),
			math.Float32frombits(payload[4]),
		)
		requestedColors := (payload[0] >> 2) & 0xff
		if requestedColors != 0 {
			// Gallium full clears ignore blend color masks.
			if context.boundBlend != 0 && context.blendStates[context.boundBlend].state&1 != 0 {
				for index := range context.colorSurfaces {
					h.gl.colorMaski(uint32(index), true, true, true, true)
				}
			} else {
				h.gl.colorMask(true, true, true, true)
			}
		}
		var fixedMask uint32
		if payload[0]&0x1 != 0 {
			fixedMask |= glDepthBufferBit
			// Gallium's full clear operation ignores the currently bound
			// depth write mask. OpenGL's glClear does not.
			h.gl.depthMask(true)
			if len(payload) >= 7 {
				h.gl.clearDepth(math.Float64frombits(uint64(payload[5]) | uint64(payload[6])<<32))
			}
		}
		if payload[0]&0x2 != 0 {
			fixedMask |= glStencilBufferBit
			// Gallium full clears ignore the bound stencil write masks.
			h.gl.stencilMaskSeparate(glFrontAndBack, ^uint32(0))
			if len(payload) >= 8 {
				h.gl.clearStencil(int32(payload[7]))
			}
		}
		// Gallium full clears are not restricted by rasterizer scissoring.
		scissorEnabled := context.boundRasterizer != 0 &&
			context.rasterizers[context.boundRasterizer].state&(1<<14) != 0
		if scissorEnabled {
			for index := range context.scissors {
				h.gl.disablei(glScissorTest, uint32(index))
			}
		}
		emulatedSamples, err := h.contextEmulatedIntegerSamples(context)
		if err != nil {
			return err
		}
		clearBoundAttachments := func() {
			mask := fixedMask
			var activeColors uint32
			for index, handle := range context.colorSurfaces {
				if handle != 0 {
					activeColors |= 1 << uint(index)
				}
			}
			if requestedColors == activeColors {
				mask |= glColorBufferBit
			} else {
				colorFloat := [4]float32{
					math.Float32frombits(payload[1]), math.Float32frombits(payload[2]),
					math.Float32frombits(payload[3]), math.Float32frombits(payload[4]),
				}
				colorInt := [4]int32{int32(payload[1]), int32(payload[2]), int32(payload[3]), int32(payload[4])}
				colorUint := [4]uint32{payload[1], payload[2], payload[3], payload[4]}
				for selected := requestedColors; selected != 0; selected &^= 1 << uint(bits.TrailingZeros32(selected)) {
					index := uint32(bits.TrailingZeros32(selected))
					handle := context.colorSurfaces[index]
					format := context.surfaces[handle].format
					if isSignedIntegerTextureFormat(format) {
						h.gl.clearBufferiv(glColor, int32(index), &colorInt[0])
					} else if isIntegerTextureFormat(format) {
						h.gl.clearBufferuiv(glColor, int32(index), &colorUint[0])
					} else {
						h.gl.clearBufferfv(glColor, int32(index), &colorFloat[0])
					}
				}
			}
			if mask != 0 {
				h.gl.clear(mask)
			}
		}
		if emulatedSamples == 0 {
			clearBoundAttachments()
		} else {
			for sample := uint32(0); sample < emulatedSamples; sample++ {
				if err := h.attachContextEmulatedIntegerSample(context, sample); err != nil {
					return err
				}
				clearBoundAttachments()
			}
			h.framebufferBindingValid = false
		}
		if fixedMask&glDepthBufferBit != 0 {
			h.restoreDepthWriteMask(context)
		}
		if fixedMask&glStencilBufferBit != 0 {
			h.restoreStencilWriteMasks(context)
		}
		if requestedColors != 0 {
			h.restoreColorMask(context)
		}
		if scissorEnabled {
			for index := range context.scissors {
				h.gl.enablei(glScissorTest, uint32(index))
			}
		}
	case 8: // VIRGL_CCMD_DRAW_VBO
		if len(payload) < 12 {
			return errors.New("truncated draw")
		}
		return h.draw(context, payload)
	case 9: // VIRGL_CCMD_RESOURCE_INLINE_WRITE
		if len(payload) < 11 {
			return errors.New("truncated inline resource write")
		}
		resource := h.resources[payload[0]]
		if resource == nil {
			return fmt.Errorf("inline write refers to unknown resource %d", payload[0])
		}
		data := make([]byte, 0, (len(payload)-11)*4)
		for _, word := range payload[11:] {
			data = append(data, byte(word), byte(word>>8), byte(word>>16), byte(word>>24))
		}
		return h.uploadResource(resource, resource.description, data, virtio.GPUTransfer3D{
			ResourceID:  payload[0],
			Level:       payload[1],
			Stride:      payload[3],
			LayerStride: payload[4],
			Box: virtio.GPUBox{
				X: payload[5], Y: payload[6], Z: payload[7],
				Width: payload[8], Height: payload[9], Depth: payload[10],
			},
		})
	case 10: // VIRGL_CCMD_SET_SAMPLER_VIEWS
		if len(payload) < 2 || payload[0] >= 6 || payload[1] >= 16 || len(payload)-2 > 16-int(payload[1]) {
			return errors.New("invalid sampler view binding")
		}
		stage, start := payload[0], payload[1]
		for slot := int(start); slot < len(context.boundSamplerViews[stage]); slot++ {
			context.boundSamplerViews[stage][slot] = 0
		}
		for index, handle := range payload[2:] {
			if handle != 0 {
				if _, ok := context.samplerViews[handle]; !ok {
					return fmt.Errorf("unknown sampler view %d", handle)
				}
			}
			context.boundSamplerViews[stage][int(start)+index] = handle
		}
	case 11: // VIRGL_CCMD_SET_INDEX_BUFFER
		if len(payload) != 1 && len(payload) != 3 {
			return errors.New("invalid index buffer state")
		}
		var resource *hostResource
		if payload[0] != 0 {
			resource = h.resources[payload[0]]
			if resource == nil || resource.buffer == 0 {
				return fmt.Errorf("unknown index buffer %d", payload[0])
			}
		}
		h.releaseResource(context.indexResource)
		h.retainResource(resource)
		context.indexBuffer = payload[0]
		context.indexResource = resource
		context.indexSize = 0
		context.indexOffset = 0
		if resource != nil {
			context.indexSize = payload[1]
			context.indexOffset = payload[2]
		}
	case 12: // VIRGL_CCMD_SET_CONSTANT_BUFFER
		if len(payload) < 2 {
			return errors.New("truncated constant buffer")
		}
		stage, buffer := payload[0], payload[1]
		if stage > tgsiTessEvaluation {
			// Mesa clears constant-buffer slot zero for every Gallium shader
			// stage during context initialization, including stages that the
			// capset does not expose. An empty clear has no host state to apply.
			if len(payload) == 2 {
				break
			}
			return fmt.Errorf("constant buffer stage %d is unsupported", stage)
		}
		if buffer >= uint32(len(context.constants[stage])) {
			return fmt.Errorf("constant buffer index %d is unsupported", buffer)
		}
		context.constants[stage][buffer] = make([]float32, len(payload)-2)
		for index, bits := range payload[2:] {
			context.constants[stage][buffer][index] = math.Float32frombits(bits)
		}
	case 13: // VIRGL_CCMD_SET_STENCIL_REF
		if len(payload) != 1 {
			return errors.New("invalid stencil reference")
		}
		context.stencilRef = [2]uint8{uint8(payload[0]), uint8(payload[0] >> 8)}
		if context.boundDSA != 0 {
			h.applyDepthStencilAlpha(context, context.depthStencilAlpha[context.boundDSA])
		}
	case 14: // VIRGL_CCMD_SET_BLEND_COLOR
		if len(payload) != 4 {
			return errors.New("invalid blend color")
		}
		for index, bits := range payload {
			context.blendColor[index] = math.Float32frombits(bits)
		}
		h.gl.blendColor(context.blendColor[0], context.blendColor[1], context.blendColor[2], context.blendColor[3])
	case 15: // VIRGL_CCMD_SET_SCISSOR_STATE
		if len(payload) < 1 || (len(payload)-1)%2 != 0 {
			return errors.New("invalid scissor state")
		}
		start, count := payload[0], (len(payload)-1)/2
		if start >= uint32(len(context.scissors)) || int(start)+count > len(context.scissors) {
			return errors.New("scissor slots are out of range")
		}
		for index := 0; index < count; index++ {
			minXY, maxXY := payload[1+index*2], payload[2+index*2]
			context.scissors[int(start)+index] = hostScissor{
				minX: minXY & 0xffff,
				minY: minXY >> 16,
				maxX: maxXY & 0xffff,
				maxY: maxXY >> 16,
			}
		}
		if context.boundRasterizer != 0 && context.rasterizers[context.boundRasterizer].state&(1<<14) != 0 {
			for index := 0; index < count; index++ {
				slot := uint32(int(start) + index)
				h.applyScissorIndexed(slot, context.scissors[slot])
			}
		}
	case 16: // VIRGL_CCMD_BLIT
		return h.blit(context, payload)
	case 17: // VIRGL_CCMD_RESOURCE_COPY_REGION
		return h.copyResourceRegion(context, payload)
	case 27: // VIRGL_CCMD_SET_UNIFORM_BUFFER
		if len(payload) != 5 || payload[0] > tgsiTessEvaluation || payload[1] >= 16 {
			return errors.New("invalid uniform buffer binding")
		}
		stage, index := payload[0], payload[1]
		binding := hostUniformBuffer{}
		if payload[4] != 0 {
			resource := h.resources[payload[4]]
			if resource == nil || resource.buffer == 0 {
				return fmt.Errorf("uniform buffer refers to unknown buffer resource %d", payload[4])
			}
			offset, length := payload[2], payload[3]
			if offset > uint32(len(resource.bufferBytes)) || length > uint32(len(resource.bufferBytes))-offset ||
				offset%16 != 0 || length%16 != 0 {
				return fmt.Errorf("uniform buffer range %d..%d is invalid for %d-byte resource %d",
					offset, uint64(offset)+uint64(length), len(resource.bufferBytes), payload[4])
			}
			binding = hostUniformBuffer{resourceID: payload[4], resource: resource, offset: offset, length: length}
		}
		h.releaseResource(context.uniformBuffers[stage][index].resource)
		h.retainResource(binding.resource)
		context.uniformBuffers[stage][index] = binding
	case 31: // VIRGL_CCMD_BIND_SHADER
		if len(payload) != 2 || payload[1] >= uint32(len(context.boundShaders)) {
			return errors.New("invalid shader binding")
		}
		if payload[0] == 0 {
			context.boundShaders[payload[1]] = 0
			break
		}
		if payload[1] > tgsiTessEvaluation {
			return fmt.Errorf("shader stage %d is unsupported", payload[1])
		}
		shader := context.shaders[payload[0]]
		if shader.stage != payload[1] {
			return fmt.Errorf("shader %d is not stage %d", payload[0], payload[1])
		}
		context.boundShaders[payload[1]] = payload[0]
	case 18: // VIRGL_CCMD_BIND_SAMPLER_STATES
		if len(payload) < 2 || payload[0] >= 6 || payload[1] >= 16 || len(payload)-2 > 16-int(payload[1]) {
			return errors.New("invalid sampler state binding")
		}
		stage, start := payload[0], payload[1]
		for slot := int(start); slot < len(context.boundSamplerStates[stage]); slot++ {
			context.boundSamplerStates[stage][slot] = 0
		}
		for index, handle := range payload[2:] {
			if handle != 0 {
				if _, ok := context.samplerStates[handle]; !ok {
					return fmt.Errorf("unknown sampler state %d", handle)
				}
			}
			context.boundSamplerStates[stage][int(start)+index] = handle
		}
	case 19: // VIRGL_CCMD_BEGIN_QUERY
		if len(payload) != 1 {
			return errors.New("invalid begin-query payload")
		}
		query, ok := context.queries[payload[0]]
		if !ok || query.active || query.queryType == 2 {
			return fmt.Errorf("query %d cannot begin", payload[0])
		}
		key := hostQueryActiveKey(query)
		if active := context.activeQueries[key]; active != 0 {
			return fmt.Errorf("query target %#x index %d is already active as %d", query.target, query.index, active)
		}
		if query.index != 0 {
			h.gl.beginQueryIndexed(query.target, query.index, query.id)
		} else {
			h.gl.beginQuery(query.target, query.id)
		}
		query.active, query.ended = true, false
		context.queries[payload[0]] = query
		context.activeQueries[key] = payload[0]
	case 20: // VIRGL_CCMD_END_QUERY
		if len(payload) != 1 {
			return errors.New("invalid end-query payload")
		}
		query, ok := context.queries[payload[0]]
		if !ok {
			return fmt.Errorf("unknown query %d", payload[0])
		}
		if query.queryType == 2 {
			h.gl.queryCounter(query.id, glTimestamp)
		} else {
			key := hostQueryActiveKey(query)
			if !query.active || context.activeQueries[key] != payload[0] {
				return fmt.Errorf("query %d is not active", payload[0])
			}
			if query.index != 0 {
				h.gl.endQueryIndexed(query.target, query.index)
			} else {
				h.gl.endQuery(query.target)
			}
			delete(context.activeQueries, key)
			query.active = false
		}
		query.ended = true
		context.queries[payload[0]] = query
		h.writeHostQueryState(query.resource, query.offset, 2, query.resultSize, 0)
	case 21: // VIRGL_CCMD_GET_QUERY_RESULT
		if len(payload) != 2 || payload[1] > 1 {
			return errors.New("invalid query-result payload")
		}
		query, ok := context.queries[payload[0]]
		if !ok || !query.ended || query.active {
			return fmt.Errorf("query %d has no completed result", payload[0])
		}
		// There is no asynchronous callback into a VirGL result resource. Read
		// the ordered GL value here even for no-wait, then complete the shared
		// state before the command fence is signaled.
		var result uint64
		if query.resultSize == 8 {
			h.gl.getQueryObjectui64v(query.id, glQueryResult, &result)
		} else {
			var value uint32
			h.gl.getQueryObjectuiv(query.id, glQueryResult, &value)
			result = uint64(value)
		}
		h.writeHostQueryState(query.resource, query.offset, 1, query.resultSize, result)
	case 24: // VIRGL_CCMD_SET_SAMPLE_MASK
		if len(payload) != 1 {
			return errors.New("invalid sample-mask payload")
		}
		h.gl.sampleMaski(0, payload[0])
	case 26: // VIRGL_CCMD_SET_RENDER_CONDITION
		if len(payload) != 3 || payload[1] > 1 || payload[2] > 3 {
			return errors.New("invalid render-condition payload")
		}
		if context.conditionalQuery != 0 {
			h.gl.endConditionalRender()
			context.conditionalQuery = 0
		}
		if payload[0] == 0 {
			break
		}
		query, ok := context.queries[payload[0]]
		if !ok || !query.ended || query.active {
			return fmt.Errorf("conditional query %d has no completed result", payload[0])
		}
		if payload[1] != 0 {
			return errors.New("inverted render conditions are unsupported")
		}
		modes := [...]uint32{glQueryWait, glQueryNoWait, glQueryByRegionWait, glQueryByRegionNoWait}
		h.gl.beginConditionalRender(query.id, modes[payload[2]])
		context.conditionalQuery = payload[0]
	case 25: // VIRGL_CCMD_SET_STREAMOUT_TARGETS
		if len(payload) < 1 || len(payload)-1 > len(context.boundStreamoutTargets) {
			return errors.New("invalid streamout target binding")
		}
		if payload[0]>>uint(len(payload)-1) != 0 {
			return fmt.Errorf("streamout append mask %#x exceeds %d targets", payload[0], len(payload)-1)
		}
		var bindings [4]uint32
		for index, handle := range payload[1:] {
			if handle != 0 {
				if _, ok := context.streamoutTargets[handle]; !ok {
					return fmt.Errorf("unknown streamout target %d", handle)
				}
			}
			bindings[index] = handle
		}
		context.boundStreamoutTargets = bindings
		if bindings == [4]uint32{} {
			// Unbinding targets is how Gallium represents a paused transform-
			// feedback object. Apple GL loses the native write cursor across an
			// object switch, so end exact vertex-point captures and resume them
			// later through an explicitly advanced buffer range.
			if context.currentStreamout != nil && context.currentStreamout.state == hostStreamoutPaused {
				h.gl.endTransformFeedback()
				context.currentStreamout.state = hostStreamoutNeedBegin
			}
			h.gl.bindTransformFeedback(glTransformFeedback, 0)
			context.currentStreamout = nil
			break
		}
		object := context.streamoutObjects[bindings]
		if object == nil {
			object = &hostStreamoutObject{handles: bindings, state: hostStreamoutNeedBegin, appendOffsetsValid: true}
			h.gl.genTransformFeedbacks(1, &object.id)
			h.gl.bindTransformFeedback(glTransformFeedback, object.id)
			if err := h.bindStreamoutObjectTargets(context, object); err != nil {
				return err
			}
			context.streamoutObjects[bindings] = object
		} else {
			h.gl.bindTransformFeedback(glTransformFeedback, object.id)
			if object.state == hostStreamoutNeedBegin && object.appendOffsetsValid {
				if err := h.bindStreamoutObjectTargets(context, object); err != nil {
					return err
				}
			}
		}
		context.currentStreamout = object
	case 52: // VIRGL_CCMD_LINK_SHADER
		if len(payload) != len(context.boundShaders) {
			return errors.New("invalid linked shader set")
		}
		for stage, handle := range payload {
			if handle == 0 {
				continue
			}
			shader, ok := context.shaders[handle]
			if !ok {
				return fmt.Errorf("linked shader stage %d refers to unknown shader %d", stage, handle)
			}
			if shader.stage != uint32(stage) {
				return fmt.Errorf("linked shader %d is stage %d, not %d", handle, shader.stage, stage)
			}
			if stage > tgsiTessEvaluation {
				return fmt.Errorf("linked shader stage %d is unsupported", stage)
			}
		}
	case 33: // VIRGL_CCMD_SET_MIN_SAMPLES
		if len(payload) != 1 {
			return errors.New("invalid minimum-samples payload")
		}
		samples := uint32(1)
		if handle := context.firstColorSurface(); handle != 0 {
			if surface := context.surfaces[handle]; surface.resource != nil {
				samples = max(1, surface.resource.description.Samples)
			}
		} else if surface := context.surfaces[context.depthSurface]; surface.resource != nil {
			samples = max(1, surface.resource.description.Samples)
		}
		h.gl.minSampleShading(min(1, float32(payload[0])/float32(samples)))
	case 32: // VIRGL_CCMD_SET_TESS_STATE
		if len(payload) != len(context.tessFactors) {
			return errors.New("invalid tessellation state")
		}
		for index, value := range payload {
			context.tessFactors[index] = math.Float32frombits(value)
		}
		h.gl.patchParameterfv(glPatchDefaultOuterLevel, &context.tessFactors[0])
		h.gl.patchParameterfv(glPatchDefaultInnerLevel, &context.tessFactors[4])
	case 22, 23:
		// Polygon stipple and clip-plane coefficients do not require additional
		// calls for the currently advertised stages.
	default:
		return fmt.Errorf("command is not implemented")
	}
	return nil
}

func (c *hostContext) createShader(payload []uint32) error {
	const (
		shaderContinuation = uint32(1 << 31)
		maxShaderBytes     = uint32(4 << 20)
	)
	if len(payload) < 5 {
		return errors.New("truncated shader")
	}
	handle, stage, offlen, numTokens, streamOutputs := payload[0], payload[1], payload[2], payload[3], payload[4]
	shaderStart := 5
	var outputs []tgsiStreamOutput
	var strides [4]uint32
	if streamOutputs != 0 {
		if streamOutputs > 64 || len(payload) < 9+int(streamOutputs)*2 {
			return fmt.Errorf("shader stream output count %d is invalid", streamOutputs)
		}
		copy(strides[:], payload[5:9])
		outputs = make([]tgsiStreamOutput, streamOutputs)
		for index := range outputs {
			packed := payload[9+index*2]
			outputs[index] = tgsiStreamOutput{
				registerIndex:  packed & 0xff,
				startComponent: (packed >> 8) & 0x3,
				numComponents:  (packed >> 10) & 0x7,
				buffer:         (packed >> 13) & 0x7,
				dstOffset:      packed >> 16,
				stream:         payload[10+index*2] & 0x3,
			}
		}
		shaderStart = 9 + int(streamOutputs)*2
	}
	chunk := shaderBytes(payload[shaderStart:])
	if offlen&shaderContinuation == 0 {
		totalBytes := offlen
		if totalBytes == 0 || totalBytes > maxShaderBytes {
			return fmt.Errorf("shader byte length %d is invalid", totalBytes)
		}
		paddedBytes := (totalBytes + 3) &^ 3
		if uint32(len(chunk)) > paddedBytes {
			return fmt.Errorf("shader first chunk has %d bytes, total is %d", len(chunk), totalBytes)
		}
		assembly := &hostShaderAssembly{
			stage:                     stage,
			numTokens:                 numTokens,
			totalBytes:                totalBytes,
			nextOffset:                uint32(len(chunk)),
			text:                      make([]byte, paddedBytes),
			streamOutputs:             outputs,
			streamOutputBufferStrides: strides,
		}
		copy(assembly.text, chunk)
		if assembly.nextOffset < paddedBytes {
			c.shaderAssemblies[handle] = assembly
			return nil
		}
		return c.finishShader(handle, assembly)
	}

	assembly := c.shaderAssemblies[handle]
	if assembly == nil {
		return fmt.Errorf("shader continuation for unknown handle %d", handle)
	}
	offset := offlen &^ shaderContinuation
	if stage != assembly.stage || numTokens != assembly.numTokens {
		return fmt.Errorf("shader continuation metadata changed for handle %d", handle)
	}
	if offset != assembly.nextOffset {
		return fmt.Errorf("shader continuation offset %d, want %d", offset, assembly.nextOffset)
	}
	if uint32(len(chunk)) > uint32(len(assembly.text))-offset {
		return fmt.Errorf("shader continuation exceeds length %d", assembly.totalBytes)
	}
	copy(assembly.text[offset:], chunk)
	assembly.nextOffset += uint32(len(chunk))
	if assembly.nextOffset < uint32(len(assembly.text)) {
		return nil
	}
	delete(c.shaderAssemblies, handle)
	return c.finishShader(handle, assembly)
}

func (c *hostContext) finishShader(handle uint32, assembly *hostShaderAssembly) error {
	lastDword := assembly.text[len(assembly.text)-4:]
	if !strings.ContainsRune(string(lastDword), '\x00') {
		return fmt.Errorf("shader %d is not NUL terminated", handle)
	}
	text := string(assembly.text[:assembly.totalBytes])
	if index := strings.IndexByte(text, 0); index >= 0 {
		text = text[:index]
	}
	stage, source, err := translateTGSI(text)
	if err != nil {
		return err
	}
	if stage != assembly.stage {
		return fmt.Errorf("shader payload type %d disagrees with TGSI stage %d", assembly.stage, stage)
	}
	source, varyings, maxInterleavedWorkaround, err := addTGSIStreamOutputs(text, source, assembly.streamOutputs, assembly.streamOutputBufferStrides)
	if err != nil {
		return fmt.Errorf("shader %d stream output: %w", handle, err)
	}
	c.nextShaderGeneration++
	c.shaders[handle] = hostShader{
		stage: stage, tgsi: text, source: source, generation: c.nextShaderGeneration,
		streamOutputVaryings: varyings, streamOutputBufferStrides: assembly.streamOutputBufferStrides,
		maxInterleavedWorkaround: maxInterleavedWorkaround,
		geometryOutputMode:       geometryStreamoutMode(text),
		tessEvaluationOutputMode: tessEvaluationStreamoutMode(text),
	}
	return nil
}

func geometryStreamoutMode(tgsi string) uint32 {
	for _, line := range strings.Split(tgsi, "\n") {
		switch strings.TrimSpace(line) {
		case "PROPERTY GS_OUTPUT_PRIMITIVE POINTS":
			return glPoints
		case "PROPERTY GS_OUTPUT_PRIMITIVE LINE_STRIP":
			return glLines
		case "PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP":
			return glTriangles
		}
	}
	return 0
}

func tessEvaluationStreamoutMode(tgsi string) uint32 {
	primitive := -1
	pointMode := false
	for _, line := range strings.Split(tgsi, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PROPERTY TES_PRIM_MODE ") {
			primitive, _ = strconv.Atoi(strings.TrimPrefix(line, "PROPERTY TES_PRIM_MODE "))
		}
		if line == "PROPERTY TES_POINT_MODE 1" {
			pointMode = true
		}
	}
	if pointMode {
		return glPoints
	}
	switch primitive {
	case 1:
		return glLines
	case 4, 7:
		return glTriangles
	default:
		return 0
	}
}

func shaderBytes(words []uint32) []byte {
	result := make([]byte, 0, len(words)*4)
	for _, word := range words {
		result = append(result, byte(word), byte(word>>8), byte(word>>16), byte(word>>24))
	}
	return result
}

func validateTextureLayers(description virtio.GPUResource3D, first, last uint32) error {
	if description.Target == 0 {
		return nil
	}
	if first > last {
		return fmt.Errorf("texture layer range %d..%d is inverted", first, last)
	}
	limit := uint32(1)
	switch description.Target {
	case 3:
		limit = description.Depth
	case 4:
		limit = 6
	case 6, 7, 8:
		limit = description.ArraySize
	}
	if last >= limit {
		return fmt.Errorf("texture layer range %d..%d exceeds %d layers", first, last, limit)
	}
	return nil
}

func (h *darwinHost) prepareVertexSystemEmulation(context *hostContext, elements []hostVertexElement,
	start, count uint32, indexed bool, instanceCount uint32, indexBias int32) (hostVertexSystemEmulation, error) {
	if len(elements) < 16 {
		return hostVertexSystemEmulation{}, nil
	}
	vertex := context.shaders[context.boundShaders[tgsiVertex]].source
	usesVertexID := strings.Contains(vertex, "gl_VertexID")
	usesInstanceID := strings.Contains(vertex, "gl_InstanceID")
	if !usesVertexID && !usesInstanceID {
		return hostVertexSystemEmulation{}, nil
	}
	if usesVertexID && usesInstanceID {
		return hostVertexSystemEmulation{}, errors.New("sixteen-attribute shaders using both vertex and instance IDs exceed the native host input limit")
	}

	emulation := hostVertexSystemEmulation{systemValue: emulatedVertexID}
	wantDivisor := func(divisor uint32) bool { return divisor == 0 }
	invocations := count
	if usesInstanceID {
		emulation.systemValue = emulatedInstanceID
		wantDivisor = func(divisor uint32) bool { return divisor != 0 }
		invocations = instanceCount
	}
	victim := -1
	for index := len(elements) - 1; index >= 0; index-- {
		element := elements[index]
		constant := element.bufferIndex < uint32(len(context.vertexBuffers)) &&
			context.vertexBuffers[element.bufferIndex].stride == 0
		if wantDivisor(element.instanceDivisor) || constant {
			victim = index
			break
		}
	}
	if victim < 0 {
		return hostVertexSystemEmulation{}, errors.New("native vertex system-value emulation has no compatible attribute slot")
	}
	emulation.attribute = uint8(victim)
	element := elements[victim]
	if element.bufferIndex >= uint32(len(context.vertexBuffers)) {
		return hostVertexSystemEmulation{}, fmt.Errorf("emulated vertex attribute uses invalid buffer index %d", element.bufferIndex)
	}
	binding := context.vertexBuffers[element.bufferIndex]
	if binding.resource == nil {
		return hostVertexSystemEmulation{}, fmt.Errorf("emulated vertex attribute refers to unknown buffer %d", binding.resourceID)
	}

	ids := make([]uint32, invocations)
	if emulation.systemValue == emulatedVertexID {
		if indexed {
			if context.indexResource == nil {
				return hostVertexSystemEmulation{}, fmt.Errorf("emulated vertex ID refers to unknown index buffer %d", context.indexBuffer)
			}
			for invocation := uint32(0); invocation < invocations; invocation++ {
				offset := uint64(context.indexOffset) + uint64(invocation)*uint64(context.indexSize)
				if offset+uint64(context.indexSize) > uint64(len(context.indexResource.bufferBytes)) {
					return hostVertexSystemEmulation{}, errors.New("emulated vertex ID exceeds the index buffer")
				}
				bytes := context.indexResource.bufferBytes[offset:]
				var index uint32
				switch context.indexSize {
				case 1:
					index = uint32(bytes[0])
				case 2:
					index = uint32(binary.LittleEndian.Uint16(bytes))
				case 4:
					index = binary.LittleEndian.Uint32(bytes)
				default:
					return hostVertexSystemEmulation{}, fmt.Errorf("unsupported index size %d", context.indexSize)
				}
				adjusted := int64(index) + int64(indexBias)
				if adjusted < 0 || adjusted > math.MaxUint32 {
					return hostVertexSystemEmulation{}, fmt.Errorf("emulated vertex ID %d with bias %d is out of range", index, indexBias)
				}
				ids[invocation] = uint32(adjusted)
			}
		} else {
			for invocation := range ids {
				ids[invocation] = start + uint32(invocation)
			}
		}
	} else {
		for invocation := range ids {
			ids[invocation] = uint32(invocation)
		}
	}

	maxID := uint32(0)
	for _, id := range ids {
		maxID = max(maxID, id)
	}
	if maxID >= 1<<20 {
		return hostVertexSystemEmulation{}, fmt.Errorf("native vertex system-value emulation ID %d exceeds its bounded staging buffer", maxID)
	}
	idWords := make([]uint32, (maxID+1)*2)
	tableWords := make([]uint32, emulatedVertexTableEntries*4)
	slots := make(map[uint32]uint32)
	for _, id := range ids {
		slot, ok := slots[id]
		if !ok {
			slot = uint32(len(slots))
			if slot >= emulatedVertexTableEntries {
				return hostVertexSystemEmulation{}, fmt.Errorf("native vertex system-value emulation needs more than %d unique values", emulatedVertexTableEntries)
			}
			fetch := id
			if emulation.systemValue == emulatedInstanceID && binding.stride != 0 {
				fetch = id / element.instanceDivisor
			}
			offset := binding.offset + element.offset
			if binding.stride != 0 {
				offset += fetch * binding.stride
			}
			value, err := vertexAttributeRawValue(binding.resource.bufferBytes, offset, element.format)
			if err != nil {
				return hostVertexSystemEmulation{}, fmt.Errorf("stage emulated vertex attribute %d: %w", victim, err)
			}
			for component := range value {
				tableWords[slot*4+uint32(component)] = math.Float32bits(value[component])
			}
			slots[id] = slot
		}
		idWords[id*2] = id
		idWords[id*2+1] = slot
	}
	if h.emulatedVertexIDBuffer == 0 {
		h.gl.genBuffers(1, &h.emulatedVertexIDBuffer)
		h.gl.genBuffers(1, &h.emulatedVertexTable)
	}
	h.gl.bindBuffer(glArrayBuffer, h.emulatedVertexIDBuffer)
	h.gl.bufferData(glArrayBuffer, len(idWords)*4, glPointerUint32(idWords), glStreamDraw)
	h.gl.bindBuffer(glUniformBuffer, h.emulatedVertexTable)
	h.gl.bufferData(glUniformBuffer, len(tableWords)*4, glPointerUint32(tableWords), glStreamDraw)
	return emulation, nil
}

func (h *darwinHost) applyDrawProgram(context *hostContext, program hostProgram, emulation hostVertexSystemEmulation) error {
	if h.currentProgram != program.id {
		h.gl.useProgram(program.id)
		h.currentProgram = program.id
	}
	if emulation.systemValue != emulatedVertexSystemNone {
		h.gl.bindBufferRange(glUniformBuffer, 0, h.emulatedVertexTable, 0, emulatedVertexTableEntries*16)
	}
	if program.winsysAdjustY >= 0 {
		adjustY := float32(1)
		if context.viewportSet&1 != 0 {
			adjustY = context.viewports[0].adjustY
		}
		h.gl.uniform1f(program.winsysAdjustY, adjustY)
	}
	for stage := tgsiVertex; stage <= tgsiTessEvaluation; stage++ {
		for index, location := range program.constantUniforms[stage] {
			if location < 0 {
				continue
			}
			binding := context.uniformBuffers[stage][index]
			if binding.resource != nil && binding.length >= 16 {
				bytes := binding.resource.bufferBytes[binding.offset : binding.offset+binding.length]
				h.gl.uniform4fv(location, int32(len(bytes)/16), (*float32)(unsafe.Pointer(&bytes[0])))
				continue
			}
			if len(context.constants[stage][index]) >= 4 {
				constants := context.constants[stage][index]
				h.gl.uniform4fv(location, int32(len(constants)/4), &constants[0])
			}
		}
	}
	for stage := tgsiVertex; stage <= tgsiTessEvaluation; stage++ {
		for index, bindingPoint := range program.constants[stage] {
			if bindingPoint < 0 {
				continue
			}
			blockSize := int(program.constantSizes[stage][index])
			if blockSize <= 0 {
				return fmt.Errorf("stage %d constant buffer %d has invalid uniform-block size %d", stage, index, blockSize)
			}
			binding := context.uniformBuffers[stage][index]
			if binding.resource != nil && int(binding.length) >= blockSize {
				h.publishBuffer(binding.resource)
				h.gl.bindBufferRange(glUniformBuffer, uint32(bindingPoint), binding.resource.buffer,
					int(binding.offset), blockSize)
				continue
			}

			var bytes []byte
			if binding.resource != nil && binding.length != 0 {
				bytes = make([]byte, blockSize)
				copy(bytes, binding.resource.bufferBytes[binding.offset:binding.offset+binding.length])
			} else if len(context.constants[stage][index]) != 0 {
				bytes = make([]byte, blockSize)
				for component, value := range context.constants[stage][index] {
					if component*4 >= len(bytes) {
						break
					}
					binary.LittleEndian.PutUint32(bytes[component*4:], math.Float32bits(value))
				}
			}
			if bytes == nil {
				if h.zeroUniformBuffer == 0 {
					h.gl.genBuffers(1, &h.zeroUniformBuffer)
					h.gl.bindBuffer(glUniformBuffer, h.zeroUniformBuffer)
					h.gl.bufferData(glUniformBuffer, capsetMaxUniformBlockSize, 0, glStaticDraw)
				}
				h.gl.bindBufferRange(glUniformBuffer, uint32(bindingPoint), h.zeroUniformBuffer, 0, blockSize)
				continue
			}
			staging := &h.constantStagingBuffers[stage][index]
			if *staging == 0 {
				h.gl.genBuffers(1, staging)
			}
			h.gl.bindBuffer(glUniformBuffer, *staging)
			h.gl.bufferData(glUniformBuffer, len(bytes), glPointer(bytes), glStreamDraw)
			h.gl.bindBufferRange(glUniformBuffer, uint32(bindingPoint), *staging, 0, len(bytes))
		}
	}
	for stage := tgsiVertex; stage <= tgsiTessEvaluation; stage++ {
		for index, location := range program.constantTextures[stage] {
			if location < 0 {
				continue
			}
			binding := context.uniformBuffers[stage][index]
			texture := &h.constantBufferTextures[stage][index]
			if *texture == 0 {
				h.gl.genTextures(1, texture)
			}
			textureUnit := uint32(64 + index)
			h.gl.activeTexture(glTexture0 + textureUnit)
			h.gl.bindTexture(glTextureBuffer, *texture)
			staging := &h.constantStagingBuffers[stage][index]
			data := make([]byte, capsetMaxUniformBlockSize)
			if binding.resource != nil && binding.length != 0 {
				copy(data, binding.resource.bufferBytes[binding.offset:binding.offset+binding.length])
			}
			var replacement uint32
			h.gl.genBuffers(1, &replacement)
			h.gl.bindBuffer(glTextureBuffer, replacement)
			h.gl.bufferData(glTextureBuffer, len(data), glPointer(data), glStreamDraw)
			h.gl.texBuffer(glTextureBuffer, glRGBA32UI, replacement)
			if *staging != 0 {
				h.retiredConstantBuffers = append(h.retiredConstantBuffers, *staging)
			}
			*staging = replacement
			h.gl.uniform1i(location, int32(textureUnit))
		}
	}
	// ARB_seamless_cube_map exposes one context-wide switch. VirGL carries
	// Gallium's desired value in each sampler state, so restore the switch on
	// every draw while walking the cube views that can observe it. As in the
	// reference renderer, the last bound cube sampler is authoritative when a
	// guest supplies conflicting states that native GL cannot represent.
	seamlessCubeMap := false
	for stage := tgsiVertex; stage <= tgsiTessEvaluation; stage++ {
		for slot, viewHandle := range context.boundSamplerViews[stage] {
			if viewHandle == 0 {
				continue
			}
			samplerLocation := program.samplers[stage][slot]
			lodCrossoverLocation := program.explicitLODCrossovers[stage][slot]
			sampleCountLocation := program.samplerSampleCounts[stage][slot]
			levelCountLocation := program.samplerLevelCounts[stage][slot]
			viewSwizzleLocation := program.samplerViewSwizzles[stage][slot]
			// Gallium may leave views bound after switching to a shader which
			// does not use their sampler. Texture swizzles and mip ranges live on
			// the shared native texture object, so applying such a stale view can
			// overwrite the active view used by another shader stage.
			if samplerLocation < 0 && lodCrossoverLocation < 0 && sampleCountLocation < 0 && levelCountLocation < 0 && viewSwizzleLocation < 0 {
				continue
			}
			view := context.samplerViews[viewHandle]
			texture := view.resource
			textureID, textureTarget := uint32(0), uint32(0)
			if texture != nil {
				textureID, textureTarget = texture.texture, texture.textureTarget
			}
			if view.texture != 0 {
				textureID, textureTarget = view.texture, view.target
			}
			if texture == nil || textureID == 0 {
				return fmt.Errorf("sampler view %d has no texture", viewHandle)
			}
			if view.target == glTextureBuffer {
				h.publishBuffer(texture)
			}
			textureUnit := uint32(stage*16 + slot)
			h.gl.activeTexture(glTexture0 + textureUnit)
			h.gl.bindTexture(textureTarget, textureID)
			if !texture.samplerViewConfigured || texture.appliedSamplerView != view {
				if err := h.applySamplerView(view); err != nil {
					return fmt.Errorf("sampler view %d: %w", viewHandle, err)
				}
				texture.appliedSamplerView = view
				texture.samplerViewConfigured = true
			}
			if stateHandle := context.boundSamplerStates[stage][slot]; stateHandle != 0 {
				state := context.samplerStates[stateHandle]
				h.gl.bindSampler(textureUnit, state.id)
				if textureTarget == glTextureCubeMap {
					seamlessCubeMap = state.state&(1<<19) != 0
				}
				if lodCrossoverLocation >= 0 {
					h.gl.uniform1f(lodCrossoverLocation, explicitLODCrossover(view, state))
				}
			} else {
				h.gl.bindSampler(textureUnit, 0)
				if lodCrossoverLocation >= 0 {
					h.gl.uniform1f(lodCrossoverLocation, 0)
				}
			}
			if samplerLocation >= 0 {
				h.gl.uniform1i(samplerLocation, int32(textureUnit))
			}
			if sampleCountLocation >= 0 {
				h.gl.uniform1i(sampleCountLocation, int32(max(1, texture.description.Samples)))
			}
			if levelCountLocation >= 0 {
				levels := uint32(1)
				if view.lastLevel >= view.firstLevel {
					levels = view.lastLevel - view.firstLevel + 1
				}
				h.gl.uniform1i(levelCountLocation, int32(levels))
			}
			if viewSwizzleLocation >= 0 {
				swizzle := [4]int32{0, 1, 2, 3}
				if texture.description.Samples != 0 {
					for component, value := range view.swizzle {
						if value == 3 && (view.format == virglFormatB8G8R8X8UNorm || view.format == virglFormatR8G8B8X8UNorm ||
							view.format == virglFormatB8G8R8X8SRGB || view.format == virglFormatR8G8B8X8SRGB) {
							value = 5
						}
						swizzle[component] = int32(value)
					}
				}
				h.gl.uniform4iv(viewSwizzleLocation, 1, &swizzle[0])
			}
		}
	}
	if seamlessCubeMap {
		h.gl.enable(glTextureCubeMapSeamless)
	} else {
		h.gl.disable(glTextureCubeMapSeamless)
	}
	return nil
}

func (h *darwinHost) draw(context *hostContext, payload []uint32) (err error) {
	indirect := len(payload) == 20
	if len(payload) < 4 || (len(payload) > 14 && !indirect) {
		return fmt.Errorf("draw payload has %d words", len(payload))
	}
	start, count, mode, indexed := payload[0], payload[1], payload[2], payload[3] != 0
	countFromStreamOutput := uint32(0)
	// Gallium represents glDrawTransformFeedback* as an ordinary non-indexed
	// draw whose count comes from a stream-output target. VirGL serializes that
	// target's byte size in this field; the normal DRAW_VBO count is zero.
	if !indexed && len(payload) > 11 && payload[11] != 0 {
		countFromStreamOutput = payload[11]
	}
	instanceCount, startInstance := uint32(1), uint32(0)
	indexBias := int32(0)
	if len(payload) > 4 {
		instanceCount = payload[4]
	}
	if len(payload) > 6 {
		startInstance = payload[6]
	}
	if len(payload) > 5 {
		indexBias = int32(payload[5])
	}
	if !indirect && instanceCount == 0 {
		return nil
	}
	if !indirect && instanceCount > math.MaxInt32 {
		return fmt.Errorf("instance count %d exceeds host GL", instanceCount)
	}
	if !indirect && startInstance != 0 {
		return fmt.Errorf("start instance %d is unsupported", startInstance)
	}
	var indirectBuffer *hostResource
	var indirectOffset uintptr
	if indirect {
		if payload[17] != 1 {
			return fmt.Errorf("indirect draw count %d requires multi-draw support", payload[17])
		}
		if payload[19] != 0 {
			return errors.New("indirect draw-count buffers are unsupported")
		}
		indirectBuffer = h.resources[payload[14]]
		if indirectBuffer == nil || indirectBuffer.buffer == 0 {
			return fmt.Errorf("indirect draw refers to unknown buffer %d", payload[14])
		}
		commandSize := uint32(16)
		if indexed {
			commandSize = 20
		}
		if payload[15]&3 != 0 || payload[15] > indirectBuffer.description.Width ||
			commandSize > indirectBuffer.description.Width-payload[15] {
			return fmt.Errorf("indirect draw command range %d..%d exceeds buffer size %d",
				payload[15], uint64(payload[15])+uint64(commandSize), indirectBuffer.description.Width)
		}
		baseInstanceOffset := payload[15] + commandSize - 4
		if binary.LittleEndian.Uint32(indirectBuffer.bufferBytes[baseInstanceOffset:]) != 0 {
			return errors.New("indirect draw base instance is unsupported")
		}
		indirectOffset = uintptr(payload[15])
	}
	rasterizerDiscard := context.boundRasterizer != 0 && context.rasterizers[context.boundRasterizer].state&(1<<3) != 0
	if !context.hasColorSurface() && context.depthSurface == 0 && !rasterizerDiscard {
		// The guest GL implementation performs framebuffer-completeness
		// validation before submitting VirGL commands. It can still emit the
		// rejected draw while reporting INVALID_FRAMEBUFFER_OPERATION to its
		// caller. Consuming that draw is important: rejecting the VirGL command
		// aborts the remainder of its batch, including later valid draws.
		return nil
	}
	if err := h.bindContextFramebuffer(context); err != nil {
		return err
	}
	if context.boundRasterizer != 0 {
		h.applyFrontFace(context, context.rasterizers[context.boundRasterizer].state)
	}
	elements := context.vertexElements[context.boundVertexElements]
	if countFromStreamOutput != 0 {
		start, count = 0, countFromStreamOutput
		if capacity, ok := streamOutputVertexCapacity(context, elements); ok {
			count = min(count, capacity)
		}
	}
	h.gl.bindVertexArray(h.vao)
	emulation, err := h.prepareVertexSystemEmulation(context, elements, start, count, indexed, instanceCount, indexBias)
	if err != nil {
		return err
	}
	var enabledAttributes uint32
	for index, element := range elements {
		if emulation.systemValue != emulatedVertexSystemNone && uint8(index) == emulation.attribute {
			h.gl.bindBuffer(glArrayBuffer, h.emulatedVertexIDBuffer)
			h.gl.vertexAttribPtr(uint32(index), 2, glFloat, false, 8, 0)
			divisor := uint32(0)
			if emulation.systemValue == emulatedInstanceID {
				divisor = 1
			}
			h.gl.vertexAttribDivisor(uint32(index), divisor)
			h.gl.enableVertexAttrib(uint32(index))
			enabledAttributes |= 1 << uint(index)
			continue
		}
		if element.bufferIndex >= uint32(len(context.vertexBuffers)) {
			return fmt.Errorf("vertex element %d uses invalid buffer index %d", index, element.bufferIndex)
		}
		binding := context.vertexBuffers[element.bufferIndex]
		buffer := binding.resource
		if buffer == nil || buffer.buffer == 0 {
			return fmt.Errorf("vertex element %d refers to unknown buffer %d", index, binding.resourceID)
		}
		if components, dataType, signed, integer := integerVertexFormat(element.format); integer {
			if binding.stride == 0 {
				value, _, err := integerVertexAttributeValue(buffer.bufferBytes, binding.offset+element.offset, element.format)
				if err != nil {
					return fmt.Errorf("vertex element %d constant integer value: %w", index, err)
				}
				h.gl.disableVertexAttrib(uint32(index))
				h.gl.vertexAttribDivisor(uint32(index), 0)
				if signed {
					h.gl.vertexAttribI4i(uint32(index), int32(value[0]), int32(value[1]), int32(value[2]), int32(value[3]))
				} else {
					h.gl.vertexAttribI4ui(uint32(index), value[0], value[1], value[2], value[3])
				}
				continue
			}
			h.publishBuffer(buffer)
			h.gl.bindBuffer(glArrayBuffer, buffer.buffer)
			h.gl.vertexAttribIPtr(uint32(index), components, dataType, int32(binding.stride),
				uintptr(binding.offset+element.offset))
			h.gl.vertexAttribDivisor(uint32(index), element.instanceDivisor)
			h.gl.enableVertexAttrib(uint32(index))
			enabledAttributes |= 1 << uint(index)
			continue
		}
		components, dataType, normalized, ok := vertexFormat(element.format)
		if !ok {
			return fmt.Errorf("vertex element %d uses unsupported format %d", index, element.format)
		}
		if binding.stride == 0 {
			// A zero-stride vertex buffer supplies one current attribute value,
			// but multiple vertex elements may still select different values
			// from that binding. Mesa uses this for constants such as color and
			// point size packed into one buffer.
			value, err := constantVertexAttribute(buffer.bufferBytes, binding.offset+element.offset, element.format)
			if err != nil {
				return fmt.Errorf("vertex element %d constant value: %w", index, err)
			}
			h.gl.disableVertexAttrib(uint32(index))
			h.gl.vertexAttribDivisor(uint32(index), 0)
			h.gl.vertexAttrib4f(uint32(index), value[0], value[1], value[2], value[3])
			continue
		}
		h.publishBuffer(buffer)
		h.gl.bindBuffer(glArrayBuffer, buffer.buffer)
		h.gl.vertexAttribPtr(uint32(index), components, dataType, normalized, int32(binding.stride),
			uintptr(binding.offset+element.offset))
		h.gl.vertexAttribDivisor(uint32(index), element.instanceDivisor)
		h.gl.enableVertexAttrib(uint32(index))
		enabledAttributes |= 1 << uint(index)
	}
	retiredAttributes := h.enabledVertexAttributes &^ enabledAttributes
	for retiredAttributes != 0 {
		index := uint32(bits.TrailingZeros32(retiredAttributes))
		h.gl.disableVertexAttrib(index)
		h.gl.vertexAttribDivisor(index, 0)
		retiredAttributes &^= 1 << index
	}
	h.enabledVertexAttributes = enabledAttributes

	var pointSpriteCoordinates uint32
	if mode == 0 && context.boundRasterizer != 0 {
		rasterizer := context.rasterizers[context.boundRasterizer]
		if rasterizer.state&(1<<7) != 0 {
			pointSpriteCoordinates = rasterizer.spriteCoordinateEnable
		}
	}
	program, err := h.programFor(context, pointSpriteCoordinates, emulation, false)
	if err != nil {
		return err
	}
	if err := h.applyDrawProgram(context, program, emulation); err != nil {
		return err
	}

	var glMode uint32
	switch mode {
	case 0:
		glMode = glPoints
	case 1:
		glMode = glLines
	case 2:
		glMode = glLineLoop
	case 3:
		glMode = glLineStrip
	case 4:
		glMode = glTriangles
	case 5:
		glMode = glTriangleStrip
	case 6:
		glMode = glTriangleFan
	case 10:
		glMode = glLinesAdjacency
	case 11:
		glMode = glLineStripAdjacency
	case 12:
		glMode = glTrianglesAdjacency
	case 13:
		glMode = glTriangleStripAdjacency
	case 14:
		if len(payload) < 13 || payload[12] == 0 || payload[12] > 32 {
			return fmt.Errorf("patch draw has invalid control-point count")
		}
		glMode = glPatches
		h.gl.patchParameteri(glPatchVertices, int32(payload[12]))
	default:
		return fmt.Errorf("unsupported primitive mode %d", mode)
	}
	emulatedSamples, err := h.contextEmulatedIntegerSamples(context)
	if err != nil {
		return err
	}
	if emulatedSamples != 0 {
		for _, handle := range context.boundStreamoutTargets {
			if handle != 0 {
				return errors.New("streamout with layered integer multisample rendering is not implemented")
			}
		}
	}
	outputVertices, outputVerticesExact := transformFeedbackOutputVertexCount(glMode, count)
	outputVerticesExact = outputVerticesExact && !indirect && !indexed
	if outputVerticesExact {
		vertices := outputVertices * uint64(instanceCount)
		if vertices > math.MaxUint32 {
			return fmt.Errorf("streamout output vertex count %d exceeds host tracking", vertices)
		}
		outputVertices = vertices
	}
	streamout, err := h.beginStreamout(context, glMode, uint32(outputVertices), outputVerticesExact)
	if err != nil {
		return err
	}
	streamoutEnded := false
	if streamout != nil {
		defer func() {
			if streamoutEnded {
				return
			}
			endErr := h.endStreamout(streamout)
			if err == nil {
				err = endErr
			} else if endErr != nil {
				err = errors.Join(err, endErr)
			}
		}()
	}
	var drawCall func()
	primitiveRestart := false
	if indexed {
		buffer := context.indexResource
		if buffer == nil || buffer.buffer == 0 {
			return fmt.Errorf("draw refers to unknown index buffer %d", context.indexBuffer)
		}
		h.publishBuffer(buffer)
		var indexType uint32
		switch context.indexSize {
		case 1:
			indexType = glUnsignedByte
		case 2:
			indexType = glUnsignedShort
		case 4:
			indexType = glUnsignedInt
		default:
			return fmt.Errorf("unsupported index size %d", context.indexSize)
		}
		h.gl.bindBuffer(glElementArrayBuffer, buffer.buffer)
		primitiveRestart = payload[7] != 0
		if primitiveRestart {
			h.gl.enable(glPrimitiveRestart)
			h.gl.primitiveRestartIndex(payload[8])
		}
		// VirGL's index-buffer binding already carries the byte offset for the
		// draw. Unlike non-indexed draws, DRAW_VBO.start is not added again by
		// the reference renderer; doing so selects unrelated indices from Mesa's
		// shared streaming buffer as soon as a draw has a nonzero start.
		offset := uintptr(context.indexOffset)
		drawCall = func() {
			if indirect {
				h.gl.drawElementsIndirect(glMode, indexType, indirectOffset)
			} else if instanceCount > 1 && indexBias != 0 {
				h.gl.drawElementsInstancedBaseVertex(glMode, int32(count), indexType, offset, int32(instanceCount), indexBias)
			} else if instanceCount > 1 {
				h.gl.drawElementsInstanced(glMode, int32(count), indexType, offset, int32(instanceCount))
			} else if indexBias != 0 {
				h.gl.drawElementsBaseVertex(glMode, int32(count), indexType, offset, indexBias)
			} else {
				h.gl.drawElements(glMode, int32(count), indexType, offset)
			}
		}
	} else {
		drawCall = func() {
			if indirect {
				h.gl.drawArraysIndirect(glMode, indirectOffset)
			} else if instanceCount > 1 {
				h.gl.drawArraysInstanced(glMode, int32(start), int32(count), int32(instanceCount))
			} else {
				h.gl.drawArrays(glMode, int32(start), int32(count))
			}
		}
	}
	if indirect {
		h.publishBuffer(indirectBuffer)
		h.gl.bindBuffer(glDrawIndirectBuffer, indirectBuffer.buffer)
		defer h.gl.bindBuffer(glDrawIndirectBuffer, 0)
	}
	if emulatedSamples == 0 {
		drawCall()
		// Apple's software fallback captures point-emitting geometry produced by
		// a patch draw, but drops its rasterization while transform feedback is
		// active. Finish (or pause) capture and replay the side-effect-free GL
		// 4.1 draw once for framebuffer/depth output. Transform-feedback writes
		// still come only from the first draw.
		geometry := context.shaders[context.boundShaders[tgsiGeometry]]
		needsPointPatchRasterization := streamout != nil && glMode == glPatches &&
			geometry.stage == tgsiGeometry && geometry.geometryOutputMode == glPoints &&
			!rasterizerDiscard && (context.firstColorSurface() != 0 || context.depthSurface != 0)
		if needsPointPatchRasterization {
			// Pausing leaves Apple's EVAL_PROG transform-feedback fallback in a
			// non-rasterizing state, so this path must fully end the capture.
			streamout.persistent = false
			if endErr := h.endStreamout(streamout); endErr != nil {
				return endErr
			}
			streamoutEnded = true
			rasterProgram, programErr := h.programFor(context, pointSpriteCoordinates, emulation, true)
			if programErr != nil {
				return programErr
			}
			if programErr := h.applyDrawProgram(context, rasterProgram, emulation); programErr != nil {
				return programErr
			}
			drawCall()
		}
	} else {
		for sample := uint32(0); sample < emulatedSamples; sample++ {
			if err := h.attachContextEmulatedIntegerSample(context, sample); err != nil {
				return err
			}
			drawCall()
		}
		h.framebufferBindingValid = false
	}
	if primitiveRestart {
		h.gl.disable(glPrimitiveRestart)
	}
	for stage := range program.constantTextures {
		for index, location := range program.constantTextures[stage] {
			if location >= 0 {
				h.gl.activeTexture(glTexture0 + uint32(64+index))
				h.gl.bindTexture(glTextureBuffer, h.constantBufferTextures[stage][index])
				h.gl.texBuffer(glTextureBuffer, glRGBA32UI, 0)
				h.gl.bindTexture(glTextureBuffer, 0)
				h.gl.bindBuffer(glTextureBuffer, 0)
				return nil
			}
		}
	}
	return nil
}

func hostQueryActiveKey(query hostQuery) uint64 {
	return uint64(query.target)<<32 | uint64(query.index)
}

func hostQueryTarget(queryType uint32) (uint32, uint32, error) {
	switch queryType {
	case 0:
		return glSamplesPassed, 8, nil
	case 1:
		return glAnySamplesPassed, 4, nil
	case 2:
		return glTimestamp, 8, nil
	case 4:
		return glTimeElapsed, 8, nil
	case 5:
		return glPrimitivesGenerated, 4, nil
	case 6:
		return glTransformFeedbackPrimitivesWritten, 4, nil
	case 11:
		return glAnySamplesPassedConservative, 4, nil
	default:
		return 0, 0, fmt.Errorf("query type %d is unsupported", queryType)
	}
}

func (h *darwinHost) writeHostQueryState(resource *hostResource, offset, state, resultSize uint32, result uint64) {
	bytes := resource.bufferBytes[offset : offset+16]
	binary.LittleEndian.PutUint32(bytes[0:4], state)
	binary.LittleEndian.PutUint32(bytes[4:8], resultSize)
	binary.LittleEndian.PutUint64(bytes[8:16], result)
	h.markBufferDirty(resource, offset, 16)
}

func (c *hostContext) hasStreamoutTargets() bool {
	for _, handle := range c.boundStreamoutTargets {
		if handle != 0 {
			return true
		}
	}
	return false
}

func (h *darwinHost) bindStreamoutObjectTargets(context *hostContext, object *hostStreamoutObject) error {
	for index, handle := range object.handles {
		if handle == 0 {
			h.gl.bindBufferRange(glTransformFeedbackBuffer, uint32(index), 0, 0, 0)
			continue
		}
		target, ok := context.streamoutTargets[handle]
		if !ok || target.resource == nil || target.resource.buffer == 0 {
			return fmt.Errorf("streamout object target %d is unavailable", handle)
		}
		written := object.writtenBytes[index]
		if written >= target.size {
			return fmt.Errorf("streamout target %d has no space after %d captured bytes", handle, written)
		}
		h.publishBuffer(target.resource)
		h.gl.bindBufferRange(glTransformFeedbackBuffer, uint32(index), target.resource.buffer,
			int(target.offset+written), int(target.size-written))
	}
	return nil
}

func transformFeedbackOutputVertexCount(mode uint32, count uint32) (uint64, bool) {
	vertices := uint64(count)
	switch mode {
	case glPoints:
		return vertices, true
	case glLines:
		return vertices - vertices%2, true
	case glLineLoop:
		if vertices < 2 {
			return 0, true
		}
		return vertices * 2, true
	case glLineStrip:
		if vertices < 2 {
			return 0, true
		}
		return (vertices - 1) * 2, true
	case glTriangles:
		return vertices - vertices%3, true
	case glTriangleStrip, glTriangleFan:
		if vertices < 3 {
			return 0, true
		}
		return (vertices - 2) * 3, true
	default:
		return 0, false
	}
}

func (h *darwinHost) beginStreamout(context *hostContext, drawMode, outputVertices uint32, outputVerticesExact bool) (*hostActiveStreamout, error) {
	if !context.hasStreamoutTargets() {
		return nil, nil
	}
	shaderHandle := context.boundShaders[tgsiGeometry]
	if shaderHandle == 0 {
		shaderHandle = context.boundShaders[tgsiTessEvaluation]
	}
	if shaderHandle == 0 {
		shaderHandle = context.boundShaders[tgsiVertex]
	}
	shader := context.shaders[shaderHandle]
	if len(shader.streamOutputVaryings) == 0 {
		return nil, fmt.Errorf("streamout targets are bound without outputs on final pre-fragment shader %d", shaderHandle)
	}
	if context.currentStreamout == nil {
		return nil, errors.New("streamout targets have no native transform-feedback object")
	}
	active := &hostActiveStreamout{object: context.currentStreamout}
	boundTargets := 0
	for _, handle := range context.boundStreamoutTargets {
		if handle != 0 {
			boundTargets++
		}
	}
	// Apple's multi-buffer transform-feedback path is reliable when each draw
	// is ended. Exact vertex-stage captures below preserve guest resume
	// semantics by advancing every bound range before the next begin.
	active.persistent = boundTargets == 1
	active.advanceOffsetsValid = outputVerticesExact && shader.stage == tgsiVertex
	if active.advanceOffsetsValid {
		for index, stride := range shader.streamOutputBufferStrides {
			advance := uint64(outputVertices) * uint64(stride) * 4
			if advance > math.MaxUint32 {
				return nil, fmt.Errorf("streamout target %d advances by %d bytes", index, advance)
			}
			active.advanceBytes[index] = uint32(advance)
		}
	}
	if shader.maxInterleavedWorkaround {
		if context.boundStreamoutTargets[0] == 0 || context.boundStreamoutTargets[1] != 0 ||
			context.boundStreamoutTargets[2] != 0 || context.boundStreamoutTargets[3] != 0 {
			return nil, errors.New("64-component interleaved streamout requires exactly one target")
		}
		target := context.streamoutTargets[context.boundStreamoutTargets[0]]
		if target.resource == nil || target.resource.buffer == 0 {
			return nil, errors.New("64-component interleaved streamout target is unavailable")
		}
		active.maxInterleavedWorkaround = true
		active.target = target
		active.vertexCapacity = target.size / (64 * 4)
		if active.vertexCapacity == 0 {
			return nil, fmt.Errorf("64-component interleaved streamout target has only %d bytes", target.size)
		}
		h.gl.genBuffers(2, &active.stagingBuffers[0])
		h.gl.bindBuffer(glArrayBuffer, active.stagingBuffers[0])
		h.gl.bufferData(glArrayBuffer, int(active.vertexCapacity*60*4), 0, glStreamDraw)
		h.gl.bindBuffer(glArrayBuffer, active.stagingBuffers[1])
		h.gl.bufferData(glArrayBuffer, int(active.vertexCapacity*4*4), 0, glStreamDraw)
		h.gl.bindBufferRange(glTransformFeedbackBuffer, 0, active.stagingBuffers[0], 0, int(active.vertexCapacity*60*4))
		h.gl.bindBufferRange(glTransformFeedbackBuffer, 1, active.stagingBuffers[1], 0, int(active.vertexCapacity*4*4))
	} else {
		for index, handle := range context.boundStreamoutTargets {
			if handle == 0 {
				if shader.streamOutputBufferStrides[index] != 0 {
					return nil, fmt.Errorf("streamout shader buffer %d has no bound target", index)
				}
				continue
			}
			target, ok := context.streamoutTargets[handle]
			if !ok || target.resource == nil || target.resource.buffer == 0 {
				return nil, fmt.Errorf("bound streamout target %d is unavailable", handle)
			}
		}
	}
	if !active.persistent && active.object.state == hostStreamoutNeedBegin && active.object.appendOffsetsValid {
		if err := h.bindStreamoutObjectTargets(context, active.object); err != nil {
			return nil, err
		}
	}
	mode := shader.geometryOutputMode
	// GL_POINTS is zero, so a point-emitting geometry shader cannot use zero as
	// an "unset" sentinel. Geometry TGSI has already been validated to carry a
	// supported output primitive; only derive the capture primitive from the
	// draw mode when the final pre-fragment stage is not geometry.
	if shader.stage == tgsiTessEvaluation {
		mode = shader.tessEvaluationOutputMode
	} else if shader.stage != tgsiGeometry {
		mode = uint32(glPoints)
		switch drawMode {
		case glPoints:
			mode = glPoints
		case glLines, glLineLoop, glLineStrip:
			mode = glLines
		case glTriangles, glTriangleStrip, glTriangleFan:
			mode = glTriangles
		default:
			if active.maxInterleavedWorkaround {
				h.gl.deleteBuffers(2, &active.stagingBuffers[0])
			}
			return nil, fmt.Errorf("primitive mode %#x cannot use streamout with shader %d stage %d",
				drawMode, shaderHandle, shader.stage)
		}
	}
	if active.object.state == hostStreamoutPaused && active.persistent && !active.maxInterleavedWorkaround {
		h.gl.resumeTransformFeedback()
	} else {
		h.gl.beginTransformFeedback(mode)
	}
	return active, nil
}

func (h *darwinHost) endStreamout(active *hostActiveStreamout) error {
	if active.maxInterleavedWorkaround || !active.persistent {
		h.gl.endTransformFeedback()
		active.object.state = hostStreamoutNeedBegin
	} else {
		h.gl.pauseTransformFeedback()
		active.object.state = hostStreamoutPaused
	}
	if !active.maxInterleavedWorkaround {
		if !active.advanceOffsetsValid {
			active.object.appendOffsetsValid = false
			return nil
		}
		if active.object.appendOffsetsValid {
			for index, advance := range active.advanceBytes {
				if advance > math.MaxUint32-active.object.writtenBytes[index] {
					active.object.appendOffsetsValid = false
					return fmt.Errorf("streamout target %d captured byte count overflow", index)
				}
				active.object.writtenBytes[index] += advance
			}
		}
		return nil
	}
	defer h.gl.deleteBuffers(2, &active.stagingBuffers[0])
	leading := make([]byte, int(active.vertexCapacity*60*4))
	trailing := make([]byte, int(active.vertexCapacity*4*4))
	h.gl.bindBuffer(glArrayBuffer, active.stagingBuffers[0])
	h.gl.getBufferSubData(glArrayBuffer, 0, len(leading), glPointer(leading))
	h.gl.bindBuffer(glArrayBuffer, active.stagingBuffers[1])
	h.gl.getBufferSubData(glArrayBuffer, 0, len(trailing), glPointer(trailing))

	interleavedBytes := int(active.vertexCapacity * 64 * 4)
	interleaved := make([]byte, interleavedBytes)
	for vertex := uint32(0); vertex < active.vertexCapacity; vertex++ {
		copy(interleaved[int(vertex*64*4):int(vertex*64*4+60*4)], leading[int(vertex*60*4):int((vertex+1)*60*4)])
		copy(interleaved[int(vertex*64*4+60*4):int((vertex+1)*64*4)], trailing[int(vertex*4*4):int((vertex+1)*4*4)])
	}
	start := int(active.target.offset)
	copy(active.target.resource.bufferBytes[start:start+interleavedBytes], interleaved)
	h.gl.bindBuffer(glArrayBuffer, active.target.resource.buffer)
	h.gl.bufferSubData(glArrayBuffer, start, len(interleaved), glPointer(interleaved))
	return nil
}

func framebufferAttachmentForSurface(surface hostSurface) (hostFramebufferAttachment, error) {
	if surface.resource == nil || surface.resource.texture == 0 {
		return hostFramebufferAttachment{}, errors.New("surface has no texture")
	}
	if surface.firstLayer != surface.lastLayer {
		layers := uint32(0)
		switch surface.resource.description.Target {
		case 3:
			layers = surface.resource.description.Depth >> surface.level
			if layers == 0 {
				layers = 1
			}
		case 4:
			layers = 6
		case 6, 7, 8:
			layers = surface.resource.description.ArraySize
		}
		if layers == 0 || surface.firstLayer != 0 || surface.lastLayer+1 != layers {
			return hostFramebufferAttachment{}, fmt.Errorf("layered surface range %d..%d does not cover all %d texture layers",
				surface.firstLayer, surface.lastLayer, layers)
		}
		return hostFramebufferAttachment{
			texture: surface.resource.texture, target: surface.resource.description.Target,
			level: surface.level, layered: true,
		}, nil
	}
	return hostFramebufferAttachment{
		texture: surface.resource.texture,
		target:  surface.resource.description.Target,
		level:   surface.level,
		layer:   surface.firstLayer,
	}, nil
}

func (h *darwinHost) attachFramebufferSurface(target, attachment uint32, surface hostSurface) error {
	binding, err := framebufferAttachmentForSurface(surface)
	if err != nil {
		return err
	}
	if binding.layered {
		if surface.resource.emulatedIntegerMSAA {
			return errors.New("layered integer multisample framebuffer rendering is not implemented")
		}
		h.gl.framebufferTextureAll(target, attachment, surface.resource.texture, int32(binding.level))
		return nil
	}
	return h.attachTextureLayer(target, attachment, surface.resource, binding.level, binding.layer)
}

func (h *darwinHost) attachTextureLayer(target, attachment uint32, resource *hostResource, level, layer uint32) error {
	if resource.emulatedIntegerMSAA {
		return h.attachEmulatedIntegerSample(target, attachment, resource, layer, 0)
	}
	switch resource.description.Target {
	case 1:
		if layer != 0 {
			return fmt.Errorf("1D texture layer %d is invalid", layer)
		}
		h.gl.framebufferTexture1D(target, attachment, glTexture1D, resource.texture, int32(level))
	case 2, 5:
		if layer != 0 {
			return fmt.Errorf("non-array texture layer %d is invalid", layer)
		}
		h.gl.framebufferTexture(target, attachment, resource.textureTarget, resource.texture, int32(level))
	case 3:
		levelDepth := resource.description.Depth >> level
		if levelDepth == 0 {
			levelDepth = 1
		}
		if layer >= levelDepth {
			return fmt.Errorf("3D texture layer %d exceeds %d slices", layer, levelDepth)
		}
		h.gl.framebufferTextureLayer(target, attachment, resource.texture, int32(level), int32(layer))
	case 4:
		if layer >= 6 {
			return fmt.Errorf("cube texture face %d is invalid", layer)
		}
		h.gl.framebufferTexture(target, attachment, glTextureCubeMapPositiveX+layer, resource.texture, int32(level))
	case 6, 7, 8:
		if layer >= resource.description.ArraySize {
			return fmt.Errorf("array texture layer %d exceeds %d layers", layer, resource.description.ArraySize)
		}
		h.gl.framebufferTextureLayer(target, attachment, resource.texture, int32(level), int32(layer))
	default:
		return fmt.Errorf("framebuffer texture target %d is unsupported", resource.description.Target)
	}
	return nil
}

func (h *darwinHost) attachEmulatedIntegerSample(target, attachment uint32, resource *hostResource, layer, sample uint32) error {
	if !resource.emulatedIntegerMSAA {
		return errors.New("resource does not use layered integer multisample storage")
	}
	layers := uint32(1)
	if resource.description.Target == 7 {
		layers = resource.description.ArraySize
	}
	if layer >= layers {
		return fmt.Errorf("integer multisample array layer %d exceeds %d layers", layer, layers)
	}
	if sample >= resource.description.Samples {
		return fmt.Errorf("integer multisample sample %d exceeds %d samples", sample, resource.description.Samples)
	}
	nativeLayer := layer*resource.description.Samples + sample
	h.gl.framebufferTextureLayer(target, attachment, resource.texture, 0, int32(nativeLayer))
	return nil
}

func (h *darwinHost) contextEmulatedIntegerSamples(context *hostContext) (uint32, error) {
	var samples uint32
	for _, handle := range context.colorSurfaces {
		if handle == 0 {
			continue
		}
		surface := context.surfaces[handle]
		if surface.resource == nil || !surface.resource.emulatedIntegerMSAA {
			continue
		}
		if samples != 0 && samples != surface.resource.description.Samples {
			return 0, errors.New("bound integer multisample surfaces use different sample counts")
		}
		samples = surface.resource.description.Samples
	}
	if samples != 0 && context.depthSurface != 0 {
		return 0, errors.New("layered integer multisample color with a depth/stencil surface is not implemented")
	}
	return samples, nil
}

func (h *darwinHost) attachContextEmulatedIntegerSample(context *hostContext, sample uint32) error {
	for index, handle := range context.colorSurfaces {
		if handle == 0 {
			continue
		}
		surface := context.surfaces[handle]
		if surface.resource == nil || !surface.resource.emulatedIntegerMSAA {
			continue
		}
		if err := h.attachEmulatedIntegerSample(glFramebuffer, glColorAttachment0+uint32(index), surface.resource, surface.firstLayer, sample); err != nil {
			return err
		}
	}
	if status := h.gl.checkFramebuffer(glFramebuffer); status != glFramebufferComplete {
		return fmt.Errorf("VirGL integer multisample framebuffer status %#x", status)
	}
	return nil
}

func (h *darwinHost) bindContextFramebuffer(context *hostContext) error {
	h.applyFramebufferSRGB(context)
	firstColorSurface := context.firstColorSurface()
	if firstColorSurface == 0 {
		if context.depthSurface != 0 {
			surface, ok := context.surfaces[context.depthSurface]
			if !ok {
				return fmt.Errorf("unknown bound depth surface %d", context.depthSurface)
			}
			target := surface.resource
			if target == nil || (!target.depth && !target.stencil) || target.texture == 0 {
				return fmt.Errorf("bound depth/stencil surface %d has no depth or stencil texture", context.depthSurface)
			}
			depthSurfaceBinding, err := framebufferAttachmentForSurface(surface)
			if err != nil {
				return fmt.Errorf("bound depth/stencil surface %d: %w", context.depthSurface, err)
			}
			attachment := uint32(glDepthAttachment)
			if target.depth && target.stencil || target.packedStencil {
				attachment = glDepthStencilAttachment
			} else if target.stencil {
				attachment = glStencilAttachment
			}
			if h.framebufferBindingValid && h.boundFramebuffer == h.depthOnlyFBO &&
				h.boundDepthSurface == depthSurfaceBinding && h.boundDepthAttachment == attachment {
				return nil
			}
			h.gl.bindFramebuffer(glFramebuffer, h.depthOnlyFBO)
			h.gl.framebufferTexture(glFramebuffer, glDepthAttachment, glTexture2D, 0, 0)
			h.gl.framebufferTexture(glFramebuffer, glStencilAttachment, glTexture2D, 0, 0)
			h.gl.framebufferTexture(glFramebuffer, glDepthStencilAttachment, glTexture2D, 0, 0)
			if err := h.attachFramebufferSurface(glFramebuffer, attachment, surface); err != nil {
				return err
			}
			h.gl.drawBuffer(glNone)
			h.gl.readBuffer(glNone)
			if status := h.gl.checkFramebuffer(glFramebuffer); status != glFramebufferComplete {
				return fmt.Errorf("VirGL depth-only framebuffer status %#x", status)
			}
			h.framebufferBindingValid = true
			h.boundFramebuffer = h.depthOnlyFBO
			h.boundColorAttachments = [8]hostFramebufferAttachment{}
			h.boundDepthTexture = target.texture
			h.boundDepthAttachment = attachment
			h.boundDepthSurface = depthSurfaceBinding
			return nil
		}
		if h.framebufferBindingValid && h.boundFramebuffer == h.discardFBO {
			return nil
		}
		h.gl.bindFramebuffer(glFramebuffer, h.discardFBO)
		h.framebufferBindingValid = true
		h.boundFramebuffer = h.discardFBO
		h.boundColorAttachments = [8]hostFramebufferAttachment{}
		h.boundDepthTexture = 0
		h.boundDepthAttachment = 0
		h.boundDepthSurface = hostFramebufferAttachment{}
		return nil
	}
	surface, ok := context.surfaces[firstColorSurface]
	if !ok {
		return fmt.Errorf("unknown bound color surface %d", firstColorSurface)
	}
	target := surface.resource
	if target == nil {
		return fmt.Errorf("bound color surface %d refers to missing resource %d", firstColorSurface, surface.resourceID)
	}
	if target.framebuffer == 0 {
		return fmt.Errorf(
			"bound color surface %d resource %d has no framebuffer (target=%d format=%d size=%dx%d depth=%t)",
			firstColorSurface, surface.resourceID, target.description.Target, target.description.Format,
			target.description.Width, target.description.Height, target.depth,
		)
	}
	var depthTexture, depthAttachment uint32
	var depthSurfaceBinding hostFramebufferAttachment
	var boundDepthSurface hostSurface
	if context.depthSurface != 0 {
		depthSurface, ok := context.surfaces[context.depthSurface]
		if !ok {
			return fmt.Errorf("unknown bound depth surface %d", context.depthSurface)
		}
		depthResource := depthSurface.resource
		if depthResource == nil || (!depthResource.depth && !depthResource.stencil) || depthResource.texture == 0 {
			return fmt.Errorf("bound depth/stencil surface %d is not a depth or stencil texture", context.depthSurface)
		}
		depthTexture = depthResource.texture
		boundDepthSurface = depthSurface
		var err error
		depthSurfaceBinding, err = framebufferAttachmentForSurface(depthSurface)
		if err != nil {
			return fmt.Errorf("bound depth/stencil surface %d: %w", context.depthSurface, err)
		}
		depthAttachment = glDepthAttachment
		if depthResource.depth && depthResource.stencil || depthResource.packedStencil {
			depthAttachment = glDepthStencilAttachment
		} else if depthResource.stencil {
			depthAttachment = glStencilAttachment
		}
	}
	var colorAttachments [8]hostFramebufferAttachment
	for index, handle := range context.colorSurfaces {
		if handle == 0 {
			continue
		}
		colorSurface, ok := context.surfaces[handle]
		if !ok || colorSurface.resource == nil || colorSurface.resource.texture == 0 {
			return fmt.Errorf("unknown bound color surface %d at slot %d", handle, index)
		}
		attachment, err := framebufferAttachmentForSurface(colorSurface)
		if err != nil {
			return fmt.Errorf("bound color surface %d at slot %d: %w", handle, index, err)
		}
		colorAttachments[index] = attachment
	}
	if h.framebufferBindingValid && h.boundFramebuffer == target.framebuffer &&
		h.boundColorAttachments == colorAttachments &&
		h.boundDepthSurface == depthSurfaceBinding && h.boundDepthAttachment == depthAttachment {
		return nil
	}
	h.gl.bindFramebuffer(glFramebuffer, target.framebuffer)
	// Framebuffer objects are context-local in the GL model used by VirGL.
	// This backend multiplexes VirGL subcontexts through one native context,
	// so a framebuffer object's attachment state may have been changed by the
	// previously active subcontext. Restore the selected subcontext's complete
	// attachment set whenever its framebuffer is rebound.
	h.gl.framebufferTexture(glFramebuffer, glDepthAttachment, glTexture2D, 0, 0)
	h.gl.framebufferTexture(glFramebuffer, glStencilAttachment, glTexture2D, 0, 0)
	h.gl.framebufferTexture(glFramebuffer, glDepthStencilAttachment, glTexture2D, 0, 0)
	var drawBuffers [8]uint32
	for index := range context.colorSurfaces {
		attachment := glColorAttachment0 + uint32(index)
		h.gl.framebufferTexture(glFramebuffer, attachment, glTexture2D, 0, 0)
		handle := context.colorSurfaces[index]
		if handle == 0 {
			drawBuffers[index] = glNone
			continue
		}
		if err := h.attachFramebufferSurface(glFramebuffer, attachment, context.surfaces[handle]); err != nil {
			return err
		}
		drawBuffers[index] = attachment
	}
	h.gl.drawBuffers(int32(len(drawBuffers)), &drawBuffers[0])
	if depthTexture != 0 {
		if err := h.attachFramebufferSurface(glFramebuffer, depthAttachment, boundDepthSurface); err != nil {
			return err
		}
	}
	if status := h.gl.checkFramebuffer(glFramebuffer); status != glFramebufferComplete {
		return fmt.Errorf("VirGL framebuffer status %#x", status)
	}
	h.framebufferBindingValid = true
	h.boundFramebuffer = target.framebuffer
	h.boundColorAttachments = colorAttachments
	h.boundDepthTexture = depthTexture
	h.boundDepthAttachment = depthAttachment
	h.boundDepthSurface = depthSurfaceBinding
	return nil
}

func (h *darwinHost) applyFramebufferSRGB(context *hostContext) {
	for _, handle := range context.colorSurfaces {
		surface, ok := context.surfaces[handle]
		if !ok {
			continue
		}
		if isSRGBFormat(surface.format) {
			h.gl.enable(glFramebufferSRGB)
			return
		}
	}
	h.gl.disable(glFramebufferSRGB)
}

func isSRGBFormat(format uint32) bool {
	switch format {
	case virglFormatR8G8B8SRGB, virglFormatA8B8G8R8SRGB,
		virglFormatB8G8R8A8SRGB, virglFormatB8G8R8X8SRGB,
		virglFormatR8G8B8A8SRGB, virglFormatR8G8B8X8SRGB:
		return true
	default:
		return false
	}
}

func (h *darwinHost) activateContext(context *hostContext) error {
	if context == nil {
		return errors.New("VirGL context has no selected subcontext")
	}
	if h.activeContext == context {
		return nil
	}
	if context.boundBlend == 0 {
		h.applyDefaultBlend()
	} else {
		state, ok := context.blendStates[context.boundBlend]
		if !ok {
			return fmt.Errorf("unknown bound blend state %d", context.boundBlend)
		}
		if err := h.applyBlend(state); err != nil {
			return err
		}
	}
	if context.boundRasterizer == 0 {
		h.gl.disable(glCullFace)
		for index := range context.scissors {
			h.gl.disablei(glScissorTest, uint32(index))
		}
		h.gl.disable(glProgramPointSize)
		h.gl.disable(glPolygonOffsetFill)
		h.gl.disable(glRasterizerDiscard)
		h.gl.disable(glDepthClamp)
		h.applyClipPlaneEnable(0)
		h.gl.pointSize(1)
	} else {
		state, ok := context.rasterizers[context.boundRasterizer]
		if !ok {
			return fmt.Errorf("unknown bound rasterizer %d", context.boundRasterizer)
		}
		h.applyRasterizer(context, state)
	}
	if context.boundDSA == 0 {
		h.gl.disable(glDepthTest)
		h.gl.depthMask(true)
		h.gl.disable(glStencilTest)
		h.gl.stencilMaskSeparate(glFrontAndBack, ^uint32(0))
	} else {
		state, ok := context.depthStencilAlpha[context.boundDSA]
		if !ok {
			return fmt.Errorf("unknown bound depth/stencil/alpha state %d", context.boundDSA)
		}
		h.applyDepthStencilAlpha(context, state)
	}
	h.gl.blendColor(context.blendColor[0], context.blendColor[1], context.blendColor[2], context.blendColor[3])
	for slots := context.viewportSet; slots != 0; {
		index := uint32(bits.TrailingZeros16(slots))
		h.applyViewportIndexed(index, context.viewports[index])
		slots &^= 1 << index
	}
	h.activeContext = context
	return nil
}

func (h *darwinHost) applyViewportIndexed(index uint32, viewport hostViewport) {
	h.gl.viewportIndexedf(index, float32(viewport.x), float32(viewport.y), float32(viewport.width), float32(viewport.height))
	h.gl.depthRangeIndexed(index, viewport.near, viewport.far)
}

func (h *darwinHost) restoreDepthWriteMask(context *hostContext) {
	if context.boundDSA == 0 {
		h.gl.depthMask(true)
		return
	}
	h.gl.depthMask(context.depthStencilAlpha[context.boundDSA].state&(1<<1) != 0)
}

func (h *darwinHost) restoreStencilWriteMasks(context *hostContext) {
	if context.boundDSA == 0 {
		h.gl.stencilMaskSeparate(glFrontAndBack, ^uint32(0))
		return
	}
	state := context.depthStencilAlpha[context.boundDSA]
	front := state.stencil[0]
	back := front
	if state.stencil[1]&1 != 0 {
		back = state.stencil[1]
	}
	h.gl.stencilMaskSeparate(glFront, (front>>21)&0xff)
	h.gl.stencilMaskSeparate(glBack, (back>>21)&0xff)
}

func (h *darwinHost) programFor(context *hostContext, pointSpriteCoordinates uint32, emulation hostVertexSystemEmulation, disableStreamout bool) (hostProgram, error) {
	vertexHandle := context.boundShaders[tgsiVertex]
	fragmentHandle := context.boundShaders[tgsiFragment]
	geometryHandle := context.boundShaders[tgsiGeometry]
	tessControlHandle := context.boundShaders[tgsiTessControl]
	tessEvaluationHandle := context.boundShaders[tgsiTessEvaluation]
	vertex := context.shaders[vertexHandle]
	fragment := context.shaders[fragmentHandle]
	geometry := context.shaders[geometryHandle]
	tessControl := context.shaders[tessControlHandle]
	tessEvaluation := context.shaders[tessEvaluationHandle]
	if vertex.source == "" || fragment.source == "" {
		return hostProgram{}, errors.New("draw has incomplete shader state")
	}
	if tessControl.source != "" && tessEvaluation.source == "" {
		return hostProgram{}, errors.New("tessellation control shader requires an evaluation shader")
	}
	outputClasses := context.fragmentOutputClasses()
	dualSource := context.usesDualSourceBlend()
	var signedVertexInputs, unsignedVertexInputs uint16
	for index, element := range context.vertexElements[context.boundVertexElements] {
		_, _, signed, integer := integerVertexFormat(element.format)
		if !integer || index >= 16 || (emulation.systemValue != emulatedVertexSystemNone && uint8(index) == emulation.attribute) {
			continue
		}
		if signed {
			signedVertexInputs |= 1 << index
		} else {
			unsignedVertexInputs |= 1 << index
		}
	}
	key := hostProgramKey{
		context: context, vertexHandle: vertexHandle, fragmentHandle: fragmentHandle, geometryHandle: geometryHandle,
		tessControlHandle: tessControlHandle, tessEvaluationHandle: tessEvaluationHandle,
		vertexGeneration: vertex.generation, fragmentGeneration: fragment.generation, geometryGeneration: geometry.generation,
		tessControlGeneration: tessControl.generation, tessEvaluationGeneration: tessEvaluation.generation,
		pointSpriteCoordinates: pointSpriteCoordinates, fragmentOutputClasses: outputClasses,
		dualSourceBlend: dualSource, emulatedVertexSystemValue: emulation.systemValue,
		emulatedVertexAttribute: emulation.attribute,
		signedVertexInputs:      signedVertexInputs, unsignedVertexInputs: unsignedVertexInputs,
		disableStreamout: disableStreamout,
	}
	if program, ok := h.programs[key]; ok {
		h.programUseSequence++
		program.lastUsed = h.programUseSequence
		h.programs[key] = program
		return program, nil
	}
	h.evictPrograms(maxHostPrograms - 1)
	fragmentSource := pointSpriteFragmentSource(fragment.source, pointSpriteCoordinates)
	vertexSource := vertex.source
	if geometry.source == "" && tessControl.source == "" && tessEvaluation.source == "" &&
		!strings.Contains(vertex.tgsi, "DCL SAMP[") && uniformOnlyLargeFragmentTGSI(fragment.tgsi) {
		if hoistedVertex, hoistedFragment, ok := hoistUniformFragmentSource(vertexSource, fragmentSource); ok {
			vertexSource, fragmentSource = hoistedVertex, hoistedFragment
		}
	}
	fragmentSource = typedFragmentOutputSource(fragmentSource, outputClasses, dualSource)
	vertexSource = emulateVertexSystemValueSource(vertexSource, emulation)
	vertexSource = typedVertexInputSource(vertexSource, signedVertexInputs, unsignedVertexInputs)
	geometrySource := geometry.source
	tessControlSource := tessControl.source
	tessEvaluationSource := tessEvaluation.source
	streamOutputVaryings := vertex.streamOutputVaryings
	if tessEvaluationSource != "" {
		vertexSource = strings.Replace(vertexSource, "    gl_Position.y *= uWinsysAdjustY;\n", "", 1)
		if tessControlSource != "" {
			vertexSource = linkTGSIInterfaces(vertexSource, tessControlSource)
			tessControlSource = linkTGSIInterfaces(tessControlSource, tessEvaluationSource)
		} else {
			vertexSource = linkTGSIInterfaces(vertexSource, tessEvaluationSource)
		}
		streamOutputVaryings = tessEvaluation.streamOutputVaryings
	}
	if geometrySource != "" {
		// The host framebuffer-origin adjustment belongs to the final
		// pre-fragment stage. Applying it in the vertex shader would expose
		// host coordinates through geometry inputs and transform feedback,
		// then apply the adjustment a second time at EmitVertex.
		vertexSource = strings.Replace(vertexSource, "    gl_Position.y *= uWinsysAdjustY;\n", "", 1)
		if tessEvaluationSource != "" {
			tessEvaluationSource = strings.Replace(tessEvaluationSource, "    gl_Position.y *= uWinsysAdjustY;\n", "", 1)
			tessEvaluationSource = linkTGSIInterfaces(tessEvaluationSource, geometrySource)
		} else {
			vertexSource = linkTGSIInterfaces(vertexSource, geometrySource)
		}
		geometrySource = linkTGSIInterfaces(geometrySource, fragmentSource)
		streamOutputVaryings = geometry.streamOutputVaryings
	} else if tessEvaluationSource != "" {
		tessEvaluationSource = linkTGSIInterfaces(tessEvaluationSource, fragmentSource)
	} else {
		vertexSource = linkTGSIInterfaces(vertexSource, fragmentSource)
	}
	if disableStreamout {
		streamOutputVaryings = nil
	}
	id, err := h.gl.compileProgramWithTessellationGeometryTransformFeedback(vertexSource, tessControlSource, tessEvaluationSource, geometrySource, fragmentSource, streamOutputVaryings)
	if err != nil {
		return hostProgram{}, err
	}
	h.programUseSequence++
	program := hostProgram{
		id:            id,
		lastUsed:      h.programUseSequence,
		winsysAdjustY: uniformLocation(h.gl, id, "uWinsysAdjustY"),
	}
	for stage := range program.constants {
		for buffer := range program.constants[stage] {
			program.constants[stage][buffer] = -1
			program.constantUniforms[stage][buffer] = -1
			program.constantTextures[stage][buffer] = -1
		}
	}
	constantBindingPoint := uint32(1) // binding zero is reserved for vertex-system emulation
	for stage := tgsiVertex; stage <= tgsiTessEvaluation; stage++ {
		for buffer := range program.constants[stage] {
			name := append([]byte(tgsiConstantBlockName(uint32(stage), buffer)), 0)
			blockIndex := h.gl.getUniformBlockIndex(id, &name[0])
			if blockIndex == ^uint32(0) {
				program.constantUniforms[stage][buffer] = uniformLocation(h.gl, id, tgsiConstantName(uint32(stage), buffer)+"[0]")
				if program.constantUniforms[stage][buffer] < 0 {
					program.constantTextures[stage][buffer] = uniformLocation(h.gl, id, tgsiConstantTextureName(uint32(stage), buffer))
				}
				continue
			}
			var blockSize int32
			h.gl.getActiveUniformBlockiv(id, blockIndex, glUniformBlockDataSize, &blockSize)
			if blockSize <= 0 || blockSize > capsetMaxUniformBlockSize {
				h.gl.deleteProgram(id)
				return hostProgram{}, fmt.Errorf("VirGL constant block %s has invalid native size %d", name[:len(name)-1], blockSize)
			}
			h.gl.uniformBlockBinding(id, blockIndex, constantBindingPoint)
			program.constants[stage][buffer] = int32(constantBindingPoint)
			program.constantSizes[stage][buffer] = blockSize
			constantBindingPoint++
		}
		for slot := range program.samplers[stage] {
			program.samplers[stage][slot] = uniformLocation(h.gl, id, tgsiSamplerName(uint32(stage), slot))
			program.explicitLODCrossovers[stage][slot] = uniformLocation(h.gl, id, tgsiSamplerLODCrossoverName(uint32(stage), slot))
			program.samplerSampleCounts[stage][slot] = uniformLocation(h.gl, id, tgsiSamplerSampleCountName(uint32(stage), slot))
			program.samplerLevelCounts[stage][slot] = uniformLocation(h.gl, id, tgsiSamplerLevelCountName(uint32(stage), slot))
			program.samplerViewSwizzles[stage][slot] = uniformLocation(h.gl, id, tgsiSamplerViewSwizzleName(uint32(stage), slot))
		}
	}
	h.programs[key] = program
	return program, nil
}

func (c *hostContext) fragmentOutputClasses() (classes [8]uint8) {
	for index, handle := range c.colorSurfaces {
		if handle == 0 {
			continue
		}
		format := c.surfaces[handle].format
		if isSignedIntegerTextureFormat(format) {
			classes[index] = fragmentOutputSInt
		} else if isIntegerTextureFormat(format) {
			classes[index] = fragmentOutputUInt
		}
	}
	return classes
}

func (c *hostContext) usesDualSourceBlend() bool {
	if c.boundBlend == 0 {
		return false
	}
	target := c.blendStates[c.boundBlend].renderTargets[0]
	for _, factor := range [...]uint32{(target >> 4) & 0x1f, (target >> 9) & 0x1f, (target >> 17) & 0x1f, (target >> 22) & 0x1f} {
		if factor == 9 || factor == 10 || factor == 0x19 || factor == 0x1a {
			return true
		}
	}
	return false
}

func (h *darwinHost) applyRasterizer(context *hostContext, rasterizer hostRasterizer) {
	state := rasterizer.state
	// Gallium names this bit depth_clip: clipping is the ordinary enabled
	// state, while clearing it requests ARB_depth_clamp behavior.
	if state&(1<<1) != 0 {
		h.gl.disable(glDepthClamp)
	} else {
		h.gl.enable(glDepthClamp)
	}
	if state&(1<<3) != 0 {
		h.gl.enable(glRasterizerDiscard)
	} else {
		h.gl.disable(glRasterizerDiscard)
	}
	if state&(1<<25) != 0 {
		h.gl.enable(glMultisample)
		h.gl.enable(glSampleMask)
	} else {
		h.gl.disable(glMultisample)
		h.gl.disable(glSampleMask)
	}
	if state&(1<<31) != 0 {
		h.gl.enable(glSampleShading)
	} else {
		h.gl.disable(glSampleShading)
	}
	h.applyClipPlaneEnable(rasterizer.clipPlaneEnable)
	switch (state >> 8) & 0x3 {
	case 0:
		h.gl.disable(glCullFace)
	case 1:
		h.gl.enable(glCullFace)
		h.gl.cullFace(glFront)
	case 2:
		h.gl.enable(glCullFace)
		h.gl.cullFace(glBack)
	case 3:
		h.gl.enable(glCullFace)
		h.gl.cullFace(glFrontAndBack)
	}
	h.applyFrontFace(context, state)
	if state&(1<<14) != 0 {
		for index := range context.scissors {
			h.gl.enablei(glScissorTest, uint32(index))
			h.applyScissorIndexed(uint32(index), context.scissors[index])
		}
	} else {
		for index := range context.scissors {
			h.gl.disablei(glScissorTest, uint32(index))
		}
	}
	if state&(1<<24) != 0 {
		h.gl.enable(glProgramPointSize)
	} else {
		h.gl.disable(glProgramPointSize)
		// Gallium commonly leaves point_size at zero for rasterizer states
		// used only by triangle draws. GL_POINT_SIZE rejects zero even though
		// the value cannot affect those draws, leaving a sticky host GL error.
		h.gl.pointSize(min(64, max(1, rasterizer.pointSize)))
	}
	if state&(1<<7) != 0 {
		origin := int32(glLowerLeft)
		if state&(1<<6) != 0 {
			origin = glUpperLeft
		}
		h.gl.pointParameteri(glPointSpriteCoordOrigin, origin)
	}
	if state&(1<<20) != 0 {
		h.gl.enable(glPolygonOffsetFill)
		h.gl.polygonOffset(rasterizer.offsetScale, rasterizer.offsetUnits)
	} else {
		h.gl.disable(glPolygonOffsetFill)
	}
}

func (h *darwinHost) applyClipPlaneEnable(mask uint8) {
	for index := uint32(0); index < 8; index++ {
		capability := uint32(glClipDistance0 + index)
		if mask&(1<<index) != 0 {
			h.gl.enable(capability)
		} else {
			h.gl.disable(capability)
		}
	}
}

func (h *darwinHost) applyFrontFace(context *hostContext, rasterizer uint32) {
	frontCCW := rasterizer&(1<<15) != 0
	if !context.framebufferOriginUpperLeft() {
		frontCCW = !frontCCW
	}
	if frontCCW {
		h.gl.frontFace(glCCW)
	} else {
		h.gl.frontFace(glCW)
	}
}

func (c *hostContext) framebufferOriginUpperLeft() bool {
	if colorSurface := c.firstColorSurface(); colorSurface != 0 {
		if surface := c.surfaces[colorSurface]; surface.resource != nil {
			return surface.resource.description.Flags&1 != 0
		}
	}
	if c.depthSurface != 0 {
		if surface := c.surfaces[c.depthSurface]; surface.resource != nil {
			return surface.resource.description.Flags&1 != 0
		}
	}
	return false
}

func (c *hostContext) hasColorSurface() bool {
	return c.firstColorSurface() != 0
}

func (c *hostContext) firstColorSurface() uint32 {
	for _, surface := range c.colorSurfaces {
		if surface != 0 {
			return surface
		}
	}
	return 0
}

func (h *darwinHost) applyDepthStencilAlpha(context *hostContext, state hostDepthStencilAlpha) {
	if state.state&1 == 0 {
		h.gl.disable(glDepthTest)
	} else {
		h.gl.enable(glDepthTest)
		functions := [...]uint32{glNever, glLess, glEqual, glLEqual, glGreater, glNotEqual, glGEqual, glAlways}
		h.gl.depthFunc(functions[(state.state>>2)&7])
	}
	h.gl.depthMask(state.state&(1<<1) != 0)

	front := state.stencil[0]
	back := front
	backReference := context.stencilRef[1]
	if state.stencil[1]&1 != 0 {
		back = state.stencil[1]
	} else {
		// A disabled separate back-face state means the complete front state
		// applies to both faces. That includes stencil_refs[0]; using the stale
		// back reference silently breaks replacement operations on back faces.
		backReference = context.stencilRef[0]
	}
	if front&1 == 0 {
		h.gl.disable(glStencilTest)
		return
	}
	h.gl.enable(glStencilTest)
	functions := [...]uint32{glNever, glLess, glEqual, glLEqual, glGreater, glNotEqual, glGEqual, glAlways}
	operations := [...]uint32{glKeep, glZero, glReplace, glIncrement, glDecrement, glIncrementWrap, glDecrementWrap, glInvert}
	applyFace := func(face uint32, packed uint32, reference uint8) {
		h.gl.stencilFuncSeparate(face, functions[(packed>>1)&7], int32(reference), (packed>>13)&0xff)
		h.gl.stencilOpSeparate(face,
			operations[(packed>>4)&7], operations[(packed>>10)&7], operations[(packed>>7)&7])
		h.gl.stencilMaskSeparate(face, (packed>>21)&0xff)
	}
	applyFace(glFront, front, context.stencilRef[0])
	applyFace(glBack, back, backReference)
}

func (h *darwinHost) applySamplerState(state hostSamplerState) {
	wrap := func(value uint32) int32 {
		switch value {
		case 0:
			return glRepeat
		case 3:
			return glClampToBorder
		case 4:
			return glMirroredRepeat
		default:
			return glClampToEdge
		}
	}
	h.gl.samplerParameteri(state.id, glTextureWrapS, wrap(state.state&7))
	h.gl.samplerParameteri(state.id, glTextureWrapT, wrap((state.state>>3)&7))
	h.gl.samplerParameteri(state.id, glTextureWrapR, wrap((state.state>>6)&7))
	linearMin := state.state&(1<<9) != 0
	var minFilter int32
	switch (state.state >> 11) & 3 {
	case 2: // PIPE_TEX_MIPFILTER_NONE
		if linearMin {
			minFilter = glLinear
		} else {
			minFilter = glNearest
		}
	case 0: // PIPE_TEX_MIPFILTER_NEAREST
		if linearMin {
			minFilter = glLinearMipmapNearest
		} else {
			minFilter = glNearestMipmapNearest
		}
	case 1: // PIPE_TEX_MIPFILTER_LINEAR
		if linearMin {
			minFilter = glLinearMipmapLinear
		} else {
			minFilter = glNearestMipmapLinear
		}
	default:
		minFilter = glNearest
	}
	magFilter := int32(glNearest)
	if state.state&(1<<13) != 0 {
		magFilter = glLinear
	}
	h.gl.samplerParameteri(state.id, glTextureMinFilter, minFilter)
	h.gl.samplerParameteri(state.id, glTextureMagFilter, magFilter)
	h.gl.samplerParameterf(state.id, glTextureLODBias, state.lodBias)
	h.gl.samplerParameterf(state.id, glTextureMinLOD, state.minLOD)
	h.gl.samplerParameterf(state.id, glTextureMaxLOD, state.maxLOD)
	anisotropy := float32((state.state >> 20) & 0x1f)
	if anisotropy < 1 {
		anisotropy = 1
	} else if anisotropy > capsetMaxAnisotropy {
		anisotropy = capsetMaxAnisotropy
	}
	h.gl.samplerParameterf(state.id, glTextureMaxAnisotropyExt, anisotropy)
	h.gl.samplerParameterfv(state.id, glTextureBorderColor, &state.borderColor[0])
	if state.state&(1<<15) != 0 {
		h.gl.samplerParameteri(state.id, glTextureCompareMode, glCompareRefToTexture)
		functions := [...]int32{glNever, glLess, glEqual, glLEqual, glGreater, glNotEqual, glGEqual, glAlways}
		h.gl.samplerParameteri(state.id, glTextureCompareFunc, functions[(state.state>>16)&7])
	} else {
		h.gl.samplerParameteri(state.id, glTextureCompareMode, glNone)
	}
}

// OpenGL ES uses a half-level minification/magnification crossover when the
// magnification filter is linear and the minification image filter is nearest
// with mipmaps. Apple's desktop GL driver instead switches explicit-LOD cube
// lookups at zero. Shaders subtract this value only on the magnification side
// of the crossover, preserving the original LOD for mip selection.
func explicitLODCrossover(view hostSamplerView, state hostSamplerState) float32 {
	if view.resource == nil || view.resource.description.Target != 4 ||
		state.state&(1<<9) != 0 || state.state&(1<<13) == 0 ||
		(state.state>>11)&3 == 2 {
		return 0
	}
	return 0.5
}

func (h *darwinHost) applySamplerView(view hostSamplerView) error {
	if view.resource == nil {
		return errors.New("sampler view has no resource")
	}
	if view.target == glTextureBuffer {
		return nil
	}
	if view.resource.textureTarget == 0 {
		return errors.New("sampler view has no texture target")
	}
	target := view.resource.textureTarget
	if view.resource.description.Samples != 0 {
		// Apple's GL 4.1 driver ignores multisample texture swizzle state.
		// Multisample TGSI fetches apply the view swizzle in translated GLSL.
		return nil
	}
	h.gl.texParameteri(target, glTextureBaseLevel, int32(view.firstLevel))
	h.gl.texParameteri(target, glTextureMaxLevel, int32(view.lastLevel))
	swizzles := [...]int32{glRed, glGreen, glBlue, glAlpha, glZero, glOne}
	parameters := [...]uint32{glTextureSwizzleR, glTextureSwizzleG, glTextureSwizzleB, glTextureSwizzleA}
	for index, value := range view.swizzle {
		if value >= uint32(len(swizzles)) {
			return fmt.Errorf("invalid component swizzle %d", value)
		}
		swizzle := swizzles[value]
		// Gallium's X8 formats carry padding, not alpha. The format table's
		// RGB1 swizzle is applied before the sampler-view swizzle, so any view
		// component that selects A must observe 1.0 regardless of the padding
		// byte supplied by the guest.
		if value == 3 && (view.format == 2 || view.format == 134) {
			swizzle = glOne
		}
		h.gl.texParameteri(target, parameters[index], swizzle)
	}
	return nil
}

func (h *darwinHost) applyDefaultBlend() {
	h.gl.disable(glBlend)
	h.gl.disable(glDither)
	h.gl.colorMask(true, true, true, true)
	for index := uint32(0); index < 8; index++ {
		h.gl.disablei(glBlend, index)
		h.gl.colorMaski(index, true, true, true, true)
	}
}

func (h *darwinHost) applyBlend(state hostBlendState) error {
	if state.state&(1<<2) != 0 {
		h.gl.enable(glDither)
	} else {
		h.gl.disable(glDither)
	}
	factor := func(value uint32) (uint32, bool) {
		switch value {
		case 1:
			return glOne, true
		case 2:
			return glSrcColor, true
		case 3:
			return glSrcAlpha, true
		case 4:
			return glDstAlpha, true
		case 5:
			return glDstColor, true
		case 6:
			return glSrcAlphaSaturate, true
		case 7:
			return glConstantColor, true
		case 8:
			return glConstantAlpha, true
		case 9:
			return glSrc1Color, true
		case 10:
			return glSrc1Alpha, true
		case 0x11:
			return glZero, true
		case 0x12:
			return glOneMinusSrcColor, true
		case 0x13:
			return glOneMinusSrcAlpha, true
		case 0x14:
			return glOneMinusDstAlpha, true
		case 0x15:
			return glOneMinusDstColor, true
		case 0x17:
			return glOneMinusConstantColor, true
		case 0x18:
			return glOneMinusConstantAlpha, true
		case 0x19:
			return glOneMinusSrc1Color, true
		case 0x1a:
			return glOneMinusSrc1Alpha, true
		default:
			return 0, false
		}
	}
	equation := func(value uint32) (uint32, bool) {
		equations := [...]uint32{glFuncAdd, glFuncSubtract, glFuncReverseSubtract, glMin, glMax}
		if value >= uint32(len(equations)) {
			return 0, false
		}
		return equations[value], true
	}
	independent := state.state&1 != 0
	applyTarget := func(index uint32, target uint32) error {
		mask := (target >> 27) & 0xf
		if independent {
			h.gl.colorMaski(index, mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0)
		} else {
			h.gl.colorMask(mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0)
		}
		if target&1 == 0 {
			if independent {
				h.gl.disablei(glBlend, index)
			} else {
				h.gl.disable(glBlend)
			}
			return nil
		}
		rgbSource, ok := factor((target >> 4) & 0x1f)
		if !ok {
			return fmt.Errorf("target %d has unsupported RGB source blend factor %d", index, (target>>4)&0x1f)
		}
		rgbDestination, ok := factor((target >> 9) & 0x1f)
		if !ok {
			return fmt.Errorf("target %d has unsupported RGB destination blend factor %d", index, (target>>9)&0x1f)
		}
		alphaSource, ok := factor((target >> 17) & 0x1f)
		if !ok {
			return fmt.Errorf("target %d has unsupported alpha source blend factor %d", index, (target>>17)&0x1f)
		}
		alphaDestination, ok := factor((target >> 22) & 0x1f)
		if !ok {
			return fmt.Errorf("target %d has unsupported alpha destination blend factor %d", index, (target>>22)&0x1f)
		}
		rgbEquation, ok := equation((target >> 1) & 7)
		if !ok {
			return fmt.Errorf("target %d has unsupported RGB blend equation %d", index, (target>>1)&7)
		}
		alphaEquation, ok := equation((target >> 14) & 7)
		if !ok {
			return fmt.Errorf("target %d has unsupported alpha blend equation %d", index, (target>>14)&7)
		}
		if independent {
			h.gl.blendFuncSeparatei(index, rgbSource, rgbDestination, alphaSource, alphaDestination)
			h.gl.blendEquationSeparatei(index, rgbEquation, alphaEquation)
			h.gl.enablei(glBlend, index)
		} else {
			h.gl.blendFuncSeparate(rgbSource, rgbDestination, alphaSource, alphaDestination)
			h.gl.blendEquationSeparate(rgbEquation, alphaEquation)
			h.gl.enable(glBlend)
		}
		return nil
	}
	if !independent {
		return applyTarget(0, state.renderTargets[0])
	}
	for index, target := range state.renderTargets {
		if err := applyTarget(uint32(index), target); err != nil {
			return err
		}
	}
	return nil
}

func (h *darwinHost) restoreColorMask(context *hostContext) {
	if context.boundBlend == 0 {
		h.gl.colorMask(true, true, true, true)
		return
	}
	state := context.blendStates[context.boundBlend]
	if state.state&1 == 0 {
		target := state.renderTargets[0]
		mask := (target >> 27) & 0xf
		h.gl.colorMask(mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0)
		return
	}
	for index, target := range state.renderTargets {
		mask := (target >> 27) & 0xf
		h.gl.colorMaski(uint32(index), mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0)
	}
}

func (h *darwinHost) applyScissor(scissor hostScissor) {
	h.applyScissorIndexed(0, scissor)
}

func (h *darwinHost) applyScissorIndexed(index uint32, scissor hostScissor) {
	width, height := int32(scissor.maxX-scissor.minX), int32(scissor.maxY-scissor.minY)
	if scissor.maxX < scissor.minX {
		width = 0
	}
	if scissor.maxY < scissor.minY {
		height = 0
	}
	h.gl.scissorIndexed(index, int32(scissor.minX), int32(scissor.minY), width, height)
}

func (h *darwinHost) restoreScissor(context *hostContext) {
	if context.boundRasterizer != 0 && context.rasterizers[context.boundRasterizer].state&(1<<14) != 0 {
		for index := range context.scissors {
			h.gl.enablei(glScissorTest, uint32(index))
			h.applyScissorIndexed(uint32(index), context.scissors[index])
		}
		return
	}
	for index := range context.scissors {
		h.gl.disablei(glScissorTest, uint32(index))
	}
}

func (h *darwinHost) blit(context *hostContext, payload []uint32) error {
	if len(payload) != 21 {
		return errors.New("invalid blit payload")
	}
	dst, src := h.resources[payload[3]], h.resources[payload[12]]
	if dst == nil || dst.texture == 0 || src == nil || src.texture == 0 {
		return errors.New("blit requires texture resources")
	}
	dstLevel, srcLevel := payload[4], payload[13]
	if dstLevel > dst.description.LastLevel || srcLevel > src.description.LastLevel {
		return fmt.Errorf("blit mip levels source %d/%d destination %d/%d are out of range",
			srcLevel, src.description.LastLevel, dstLevel, dst.description.LastLevel)
	}
	h.framebufferBindingValid = false
	dstZ, srcZ := payload[8], payload[17]
	validLayer := func(resource *hostResource, layer uint32) bool {
		switch resource.description.Target {
		case 1:
			return layer == 0
		case 2, 5:
			return layer == 0
		case 3:
			return layer < resource.description.Depth
		case 4:
			return layer < 6
		case 6, 7, 8:
			return layer < resource.description.ArraySize
		default:
			return false
		}
	}
	if !validLayer(src, srcZ) || !validLayer(dst, dstZ) {
		return fmt.Errorf("texture blit layers source %d target %d and destination %d target %d are invalid",
			srcZ, src.description.Target, dstZ, dst.description.Target)
	}
	srcLayers, dstLayers := payload[20], payload[11]
	if srcLayers == 0 || dstLayers == 0 {
		return errors.New("texture blit requires nonzero source and destination layer counts")
	}
	if srcLayers > 1 || dstLayers > 1 {
		for layer := uint32(0); layer < dstLayers; layer++ {
			// Match nearest filtering at destination-layer pixel centers. Gallium
			// permits the source and destination depth ranges to differ even though
			// native framebuffer blits operate on one attached layer at a time.
			sourceLayer := uint32((uint64(layer)*2 + 1) * uint64(srcLayers) / (2 * uint64(dstLayers)))
			if sourceLayer >= srcLayers {
				sourceLayer = srcLayers - 1
			}
			layerPayload := append([]uint32(nil), payload...)
			layerPayload[8] = dstZ + layer
			layerPayload[11] = 1
			layerPayload[17] = srcZ + sourceLayer
			layerPayload[20] = 1
			if !validLayer(src, layerPayload[17]) || !validLayer(dst, layerPayload[8]) {
				return fmt.Errorf("texture blit layer %d exceeds source or destination bounds", layer)
			}
			if err := h.blit(context, layerPayload); err != nil {
				return err
			}
		}
		return nil
	}
	mask := uint32(0)
	if payload[0]&0xf != 0 {
		mask |= glColorBufferBit
	}
	if payload[0]&0x10 != 0 {
		mask |= glDepthBufferBit
	}
	if payload[0]&0x20 != 0 {
		mask |= glStencilBufferBit
	}
	attachment := uint32(glColorAttachment0)
	if src.depth {
		attachment = glDepthAttachment
		if src.stencil {
			attachment = glDepthStencilAttachment
		}
	} else if src.stencil {
		attachment = glStencilAttachment
		if src.packedStencil {
			attachment = glDepthStencilAttachment
		}
	}
	if src.depth != dst.depth || src.stencil != dst.stencil {
		return errors.New("blit requires matching source and destination aspects")
	}
	if handled, err := h.blitSharedExponentToRGBA32F(context, dst, src, payload); handled {
		return err
	}
	// Apple's GL-on-Metal driver can corrupt the color texture formerly
	// attached to a reused temporary draw FBO when the next operation replaces
	// it with a packed depth/stencil attachment and performs a scissored blit.
	// Keep each VirGL blit's attachment lifetime independent. Resource-transfer
	// and copy paths retain the shared temporary FBOs because they do not switch
	// a draw FBO directly between color and packed depth/stencil operations.
	var blitFBOs [2]uint32
	h.gl.genFramebuffers(2, &blitFBOs[0])
	defer h.gl.deleteFramebuffers(2, &blitFBOs[0])
	readFBO, drawFBO := blitFBOs[0], blitFBOs[1]
	clearAttachments := func(target uint32) {
		h.gl.framebufferTexture(target, glColorAttachment0, glTexture2D, 0, 0)
		h.gl.framebufferTexture(target, glDepthAttachment, glTexture2D, 0, 0)
		h.gl.framebufferTexture(target, glStencilAttachment, glTexture2D, 0, 0)
		h.gl.framebufferTexture(target, glDepthStencilAttachment, glTexture2D, 0, 0)
	}
	h.gl.bindFramebuffer(glReadFramebuffer, readFBO)
	clearAttachments(glReadFramebuffer)
	if err := h.attachTextureLayer(glReadFramebuffer, attachment, src, srcLevel, srcZ); err != nil {
		return err
	}
	if status := h.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
		return fmt.Errorf("VirGL blit source framebuffer status %#x (resource %d format %#x target %d level %d layer %d flags %#x; destination %d format %#x target %d level %d layer %d flags %#x; control %#x source box %d,%d %dx%d destination box %d,%d %dx%d)",
			status, payload[12], src.description.Format, src.description.Target, srcLevel, srcZ, src.description.Flags,
			payload[3], dst.description.Format, dst.description.Target, dstLevel, dstZ, dst.description.Flags,
			payload[0], payload[15], payload[16], payload[18], payload[19], payload[6], payload[7], payload[9], payload[10])
	}
	h.gl.bindFramebuffer(glDrawFramebuffer, drawFBO)
	clearAttachments(glDrawFramebuffer)
	if err := h.attachTextureLayer(glDrawFramebuffer, attachment, dst, dstLevel, dstZ); err != nil {
		return err
	}
	if attachment == glColorAttachment0 {
		h.gl.drawBuffer(glColorAttachment0)
	} else {
		h.gl.drawBuffer(glNone)
	}
	if status := h.gl.checkFramebuffer(glDrawFramebuffer); status != glFramebufferComplete {
		return fmt.Errorf("VirGL blit destination framebuffer status %#x", status)
	}
	filter := uint32(glNearest)
	if (payload[0]>>8)&3 != 0 {
		filter = glLinear
	}
	if payload[0]&(1<<10) != 0 {
		minXY, maxXY := payload[1], payload[2]
		h.gl.enable(glScissorTest)
		h.applyScissor(hostScissor{
			minX: minXY & 0xffff,
			minY: minXY >> 16,
			maxX: maxXY & 0xffff,
			maxY: maxXY >> 16,
		})
	} else {
		h.gl.disable(glScissorTest)
	}
	srcY1, srcY2 := virglBlitY(src, srcLevel, int32(payload[16]), int32(payload[19]))
	dstY1, dstY2 := virglBlitY(dst, dstLevel, int32(payload[7]), int32(payload[10]))
	handledSRGB, err := h.blitSRGBColor(dst, src, payload, srcY1, srcY2, dstY1, dstY2, filter)
	if err != nil {
		return err
	}
	if !handledSRGB {
		h.gl.blitFramebuffer(
			int32(payload[15]), srcY1, int32(payload[15])+int32(payload[18]), srcY2,
			int32(payload[6]), dstY1, int32(payload[6])+int32(payload[9]), dstY2,
			mask, filter,
		)
	}
	h.restoreScissor(context)
	// Shader-assisted blits temporarily replace fixed-function state. Force a
	// complete tracked-state restore before returning to guest rendering.
	h.activeContext = nil
	if err := h.activateContext(context); err != nil {
		return err
	}
	return h.bindContextFramebuffer(context)
}

func (h *darwinHost) blitSRGBColor(dst, src *hostResource, payload []uint32, srcY1, srcY2, dstY1, dstY2 int32, filter uint32) (bool, error) {
	if !isSRGBFormat(src.description.Format) && !isSRGBFormat(dst.description.Format) {
		return false, nil
	}
	if payload[0]&0x3f != 0xf || payload[0]&(1<<12) != 0 ||
		src.description.Target != 2 || dst.description.Target != 2 ||
		src.description.Samples != 0 || dst.description.Samples != 0 ||
		src.depth || src.stencil || dst.depth || dst.stencil {
		return false, nil
	}

	srcX1 := int32(payload[15])
	srcX2 := srcX1 + int32(payload[18])
	dstX1 := int32(payload[6])
	dstX2 := dstX1 + int32(payload[9])
	minMax := func(a, b int32) (int32, int32) {
		if a <= b {
			return a, b
		}
		return b, a
	}
	dstMinX, dstMaxX := minMax(dstX1, dstX2)
	dstMinY, dstMaxY := minMax(dstY1, dstY2)
	if dstMinX == dstMaxX || dstMinY == dstMaxY {
		return true, nil
	}

	levelDimension := func(value, level uint32) float32 {
		result := value >> level
		if result == 0 {
			result = 1
		}
		return float32(result)
	}
	srcWidth := levelDimension(src.description.Width, payload[13])
	srcHeight := levelDimension(src.description.Height, payload[13])
	u1, u2 := float32(srcX1)/srcWidth, float32(srcX2)/srcWidth
	v1, v2 := float32(srcY1)/srcHeight, float32(srcY2)/srcHeight
	if dstX2 < dstX1 {
		u1, u2 = u2, u1
	}
	if dstY2 < dstY1 {
		v1, v2 = v2, v1
	}

	const vertex = `#version 410 core
out vec2 textureCoordinate;
uniform vec4 sourceRectangle;
void main() {
	vec2 unit = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	textureCoordinate = mix(sourceRectangle.xy, sourceRectangle.zw, unit);
	gl_Position = vec4(unit * 2.0 - 1.0, 0.0, 1.0);
}`
	const fragment = `#version 410 core
in vec2 textureCoordinate;
layout(location = 0) out vec4 fragmentColor;
uniform sampler2D sourceTexture;
uniform float sourceLevel;
void main() { fragmentColor = textureLod(sourceTexture, textureCoordinate, sourceLevel); }`
	program, err := h.gl.compileProgram(vertex, fragment)
	if err != nil {
		return true, fmt.Errorf("compile sRGB blit program: %w", err)
	}
	defer h.gl.deleteProgram(program)

	var sampler uint32
	h.gl.genSamplers(1, &sampler)
	defer h.gl.deleteSamplers(1, &sampler)
	h.gl.samplerParameteri(sampler, glTextureWrapS, glClampToEdge)
	h.gl.samplerParameteri(sampler, glTextureWrapT, glClampToEdge)
	h.gl.samplerParameteri(sampler, glTextureMinFilter, int32(filter))
	h.gl.samplerParameteri(sampler, glTextureMagFilter, int32(filter))
	h.gl.bindSampler(0, sampler)
	defer h.gl.bindSampler(0, 0)
	h.gl.activeTexture(glTexture0)
	h.gl.bindTexture(glTexture2D, src.texture)
	h.gl.useProgram(program)
	if location := uniformLocation(h.gl, program, "sourceRectangle"); location >= 0 {
		rectangle := [4]float32{u1, v1, u2, v2}
		h.gl.uniform4fv(location, 1, &rectangle[0])
	}
	if location := uniformLocation(h.gl, program, "sourceTexture"); location >= 0 {
		h.gl.uniform1i(location, 0)
	}
	if location := uniformLocation(h.gl, program, "sourceLevel"); location >= 0 {
		h.gl.uniform1f(location, float32(payload[13]))
	}
	if isSRGBFormat(dst.description.Format) {
		h.gl.enable(glFramebufferSRGB)
	} else {
		h.gl.disable(glFramebufferSRGB)
	}
	for index := uint32(0); index < 8; index++ {
		h.gl.disablei(glBlend, index)
	}
	h.gl.disable(glDepthTest)
	h.gl.disable(glStencilTest)
	h.gl.disable(glCullFace)
	h.gl.disable(glRasterizerDiscard)
	h.gl.colorMask(true, true, true, true)
	h.gl.viewport(dstMinX, dstMinY, dstMaxX-dstMinX, dstMaxY-dstMinY)
	h.gl.bindVertexArray(h.vao)
	h.gl.drawArrays(glTriangles, 0, 3)
	return true, nil
}

// blitSharedExponentToRGBA32F handles Mesa's staging conversion for
// glGetTexImage(GL_RGB9_E5). Apple's OpenGL 4.1 driver can sample and transfer
// RGB9_E5 textures, but reports GL_FRAMEBUFFER_UNSUPPORTED when a 2D image or
// 3D slice is attached to a read framebuffer. Mesa implements the format
// conversion as a VirGL blit into RGBA32F, so perform that exact conversion
// through the texture transfer API when the framebuffer path is unavailable.
func (h *darwinHost) blitSharedExponentToRGBA32F(context *hostContext, dst, src *hostResource, payload []uint32) (bool, error) {
	if src.description.Format != virglFormatR9G9B9E5Float ||
		dst.description.Format != virglFormatR32G32B32A32Float {
		return false, nil
	}
	if payload[0] != 0xf || src.depth || src.stencil || dst.depth || dst.stencil ||
		(src.description.Target != 2 && src.description.Target != 3 && src.description.Target != 5) ||
		(dst.description.Target != 2 && dst.description.Target != 5) ||
		(src.textureTarget != glTexture2D && src.textureTarget != glTexture3D) || dst.textureTarget != glTexture2D ||
		src.description.Samples != 0 || dst.description.Samples != 0 ||
		payload[8] != 0 ||
		src.description.Flags&1 != dst.description.Flags&1 {
		return false, nil
	}

	srcWidth, srcHeight := int64(payload[18]), int64(payload[19])
	dstWidth, dstHeight := int64(payload[9]), int64(payload[10])
	if srcWidth != dstWidth || srcHeight != dstHeight || srcWidth == 0 || srcHeight == 0 ||
		srcWidth > math.MaxInt32 || srcHeight > math.MaxInt32 {
		return false, nil
	}
	levelDimension := func(value, level uint32) int64 {
		result := value >> level
		if result == 0 {
			result = 1
		}
		return int64(result)
	}
	srcLevel, dstLevel := payload[13], payload[4]
	srcLevelWidth, srcLevelHeight := levelDimension(src.description.Width, srcLevel), levelDimension(src.description.Height, srcLevel)
	dstLevelWidth, dstLevelHeight := levelDimension(dst.description.Width, dstLevel), levelDimension(dst.description.Height, dstLevel)
	srcX, srcY, srcZ := int64(payload[15]), int64(payload[16]), int64(payload[17])
	dstX, dstY := int64(payload[6]), int64(payload[7])
	if srcX > srcLevelWidth || srcWidth > srcLevelWidth-srcX ||
		srcY > srcLevelHeight || srcHeight > srcLevelHeight-srcY ||
		dstX > dstLevelWidth || dstWidth > dstLevelWidth-dstX ||
		dstY > dstLevelHeight || dstHeight > dstLevelHeight-dstY {
		return true, errors.New("shared-exponent texture blit is out of bounds")
	}

	srcLevelDepth := int64(1)
	if src.description.Target == 3 {
		srcLevelDepth = levelDimension(src.description.Depth, srcLevel)
	}
	full := make([]byte, int(srcLevelWidth*srcLevelHeight*srcLevelDepth*3*4))
	h.gl.bindTexture(src.textureTarget, src.texture)
	h.drainGLErrors()
	h.gl.getTexImage(src.textureTarget, int32(srcLevel), glRGB, glFloat, glPointer(full))
	if glError := h.gl.getError(); glError != 0 {
		return true, fmt.Errorf("VirGL shared-exponent blit readback GL error %#x (resource %d target %d native target %#x level %d slice %d level size %dx%dx%d source box %d,%d %dx%d)",
			glError, src.description.ID, src.description.Target, src.textureTarget, srcLevel, srcZ,
			srcLevelWidth, srcLevelHeight, srcLevelDepth, srcX, srcY, srcWidth, srcHeight)
	}
	rowBytes := int(srcWidth * 3 * 4)
	region := make([]byte, rowBytes*int(srcHeight))
	for row := int64(0); row < srcHeight; row++ {
		source := int(((srcZ*srcLevelHeight+srcY+row)*srcLevelWidth + srcX) * 3 * 4)
		copy(region[int(row)*rowBytes:], full[source:source+rowBytes])
	}

	h.gl.bindTexture(dst.textureTarget, dst.texture)
	h.drainGLErrors()
	h.gl.texSubImage2D(dst.textureTarget, int32(dstLevel), int32(dstX), int32(dstY),
		int32(dstWidth), int32(dstHeight), glRGB, glFloat, glPointer(region))
	if glError := h.gl.getError(); glError != 0 {
		return true, fmt.Errorf("VirGL shared-exponent blit upload GL error %#x", glError)
	}
	return true, h.bindContextFramebuffer(context)
}

// OpenGL error flags are context-global and survive until queried. Localized
// checks around driver workarounds must not attribute an older command's flag
// to the transfer they are about to validate.
func (h *darwinHost) drainGLErrors() {
	for range 16 {
		if h.gl.getError() == 0 {
			return
		}
	}
}

func virglBlitY(resource *hostResource, level uint32, y, height int32) (int32, int32) {
	if resource.description.Flags&1 == 0 {
		return y + height, y
	}
	resourceHeight := int32(resource.description.Height >> level)
	if resourceHeight == 0 {
		resourceHeight = 1
	}
	return resourceHeight - y - height, resourceHeight - y
}

func (h *darwinHost) copyResourceRegion(context *hostContext, payload []uint32) error {
	if len(payload) != 13 {
		return errors.New("invalid resource copy region payload")
	}
	dst, src := h.resources[payload[0]], h.resources[payload[5]]
	if dst == nil || src == nil {
		return errors.New("resource copy region refers to an unknown resource")
	}
	dstLevel, srcLevel := payload[1], payload[6]
	if dstLevel > dst.description.LastLevel || srcLevel > src.description.LastLevel {
		return fmt.Errorf("resource copy mip levels source %d/%d destination %d/%d are out of range",
			srcLevel, src.description.LastLevel, dstLevel, dst.description.LastLevel)
	}
	dstX, dstY, dstZ := payload[2], payload[3], payload[4]
	srcX, srcY, srcZ := payload[7], payload[8], payload[9]
	width, height, depth := payload[10], payload[11], payload[12]

	if src.buffer != 0 || dst.buffer != 0 {
		if src.buffer == 0 || dst.buffer == 0 || srcLevel != 0 || dstLevel != 0 ||
			srcY != 0 || srcZ != 0 || dstY != 0 || dstZ != 0 || height != 1 || depth != 1 {
			return errors.New("buffer copy region must describe a one-dimensional buffer range")
		}
		if uint64(srcX)+uint64(width) > uint64(src.description.Width) ||
			uint64(dstX)+uint64(width) > uint64(dst.description.Width) {
			return errors.New("buffer copy region is out of bounds")
		}
		if width == 0 {
			return nil
		}
		bytes := append([]byte(nil), src.bufferBytes[int(srcX):int(srcX+width)]...)
		copy(dst.bufferBytes[int(dstX):int(dstX+width)], bytes)
		h.markBufferDirty(dst, dstX, width)
		return nil
	}
	if src.texture == 0 || dst.texture == 0 {
		return errors.New("resource copy region requires matching buffer or texture resources")
	}
	layerCount := func(resource *hostResource) uint32 {
		switch resource.description.Target {
		case 2:
			return 1
		case 3:
			return resource.description.Depth
		case 4:
			return 6
		case 7, 8:
			return resource.description.ArraySize
		default:
			return 0
		}
	}
	srcLayers, dstLayers := layerCount(src), layerCount(dst)
	if depth == 0 {
		return nil
	}
	if srcZ > srcLayers || depth > srcLayers-srcZ || dstZ > dstLayers || depth > dstLayers-dstZ {
		return fmt.Errorf("texture copy layer ranges source %d..%d target %d and destination %d..%d target %d are invalid",
			srcZ, srcZ+depth-1, src.description.Target, dstZ, dstZ+depth-1, dst.description.Target)
	}
	levelDimension := func(value, level uint32) uint32 {
		value >>= level
		if value == 0 {
			return 1
		}
		return value
	}
	srcWidth, srcHeight := levelDimension(src.description.Width, srcLevel), levelDimension(src.description.Height, srcLevel)
	dstWidth, dstHeight := levelDimension(dst.description.Width, dstLevel), levelDimension(dst.description.Height, dstLevel)
	if uint64(srcX)+uint64(width) > uint64(srcWidth) ||
		uint64(srcY)+uint64(height) > uint64(srcHeight) ||
		uint64(dstX)+uint64(width) > uint64(dstWidth) ||
		uint64(dstY)+uint64(height) > uint64(dstHeight) {
		return errors.New("texture copy region is out of bounds")
	}
	if src.depth != dst.depth || src.stencil != dst.stencil {
		return errors.New("resource copy region requires compatible texture formats")
	}
	if handled, err := h.copySharedExponentTexture(dst, src, dstLevel, srcLevel,
		dstX, dstY, dstZ, srcX, srcY, srcZ, width, height, depth); handled {
		return err
	}

	attachment := uint32(glColorAttachment0)
	mask := uint32(glColorBufferBit)
	if src.depth {
		attachment = glDepthAttachment
		mask = glDepthBufferBit
		if src.stencil {
			attachment = glDepthStencilAttachment
			mask |= glStencilBufferBit
		}
	} else if src.stencil {
		attachment = glStencilAttachment
		if src.packedStencil {
			attachment = glDepthStencilAttachment
		}
		mask = glStencilBufferBit
	}
	h.framebufferBindingValid = false
	h.gl.disable(glScissorTest)
	srcY1, srcY2 := virglCopyY(src, srcLevel, int32(srcY), int32(height))
	dstY1, dstY2 := virglCopyY(dst, dstLevel, int32(dstY), int32(height))
	for layer := uint32(0); layer < depth; layer++ {
		h.gl.bindFramebuffer(glReadFramebuffer, h.blitReadFBO)
		h.gl.framebufferTexture(glReadFramebuffer, glColorAttachment0, glTexture2D, 0, 0)
		h.gl.framebufferTexture(glReadFramebuffer, glDepthAttachment, glTexture2D, 0, 0)
		h.gl.framebufferTexture(glReadFramebuffer, glStencilAttachment, glTexture2D, 0, 0)
		h.gl.framebufferTexture(glReadFramebuffer, glDepthStencilAttachment, glTexture2D, 0, 0)
		if err := h.attachTextureLayer(glReadFramebuffer, attachment, src, srcLevel, srcZ+layer); err != nil {
			return err
		}
		if status := h.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("VirGL copy source framebuffer status %#x (resource %d format %#x target %d native target %#x level %d layer %d flags %#x; destination %d format %#x target %d native target %#x level %d layer %d flags %#x; source box %d,%d,%d %dx%dx%d destination %d,%d,%d)",
				status, src.description.ID, src.description.Format, src.description.Target, src.textureTarget,
				srcLevel, srcZ+layer, src.description.Flags,
				dst.description.ID, dst.description.Format, dst.description.Target, dst.textureTarget,
				dstLevel, dstZ+layer, dst.description.Flags,
				srcX, srcY, srcZ, width, height, depth, dstX, dstY, dstZ)
		}
		h.gl.bindFramebuffer(glDrawFramebuffer, h.blitDrawFBO)
		h.gl.framebufferTexture(glDrawFramebuffer, glColorAttachment0, glTexture2D, 0, 0)
		h.gl.framebufferTexture(glDrawFramebuffer, glDepthAttachment, glTexture2D, 0, 0)
		h.gl.framebufferTexture(glDrawFramebuffer, glStencilAttachment, glTexture2D, 0, 0)
		h.gl.framebufferTexture(glDrawFramebuffer, glDepthStencilAttachment, glTexture2D, 0, 0)
		if err := h.attachTextureLayer(glDrawFramebuffer, attachment, dst, dstLevel, dstZ+layer); err != nil {
			return err
		}
		if status := h.gl.checkFramebuffer(glDrawFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("VirGL copy destination framebuffer status %#x (resource %d format %#x target %d native target %#x level %d layer %d flags %#x; source %d format %#x target %d native target %#x level %d layer %d flags %#x; destination box %d,%d,%d %dx%dx%d source %d,%d,%d)",
				status, dst.description.ID, dst.description.Format, dst.description.Target, dst.textureTarget,
				dstLevel, dstZ+layer, dst.description.Flags,
				src.description.ID, src.description.Format, src.description.Target, src.textureTarget,
				srcLevel, srcZ+layer, src.description.Flags,
				dstX, dstY, dstZ, width, height, depth, srcX, srcY, srcZ)
		}
		h.gl.blitFramebuffer(
			int32(srcX), srcY1, int32(srcX+width), srcY2,
			int32(dstX), dstY1, int32(dstX+width), dstY2,
			mask, glNearest,
		)
	}
	h.restoreScissor(context)
	return h.bindContextFramebuffer(context)
}

// copySharedExponentTexture bypasses framebuffer blits for RGB9_E5. Apple's
// OpenGL driver supports native packed uploads, readback, and sampling for this
// format, but rejects it as a framebuffer attachment. Mesa uses resource-copy
// commands while specifying RGB9_E5 textures, so preserve their packed texels
// through the texture transfer API instead.
func (h *darwinHost) copySharedExponentTexture(dst, src *hostResource,
	dstLevel, srcLevel, dstX, dstY, dstZ, srcX, srcY, srcZ, width, height, depth uint32) (bool, error) {
	if src.description.Format != virglFormatR9G9B9E5Float ||
		dst.description.Format != virglFormatR9G9B9E5Float ||
		src.description.Samples != 0 || dst.description.Samples != 0 ||
		src.description.Flags&1 != dst.description.Flags&1 {
		return false, nil
	}

	levelDimension := func(value, level uint32) uint32 {
		value >>= level
		if value == 0 {
			return 1
		}
		return value
	}
	srcWidth := levelDimension(src.description.Width, srcLevel)
	srcHeight := levelDimension(src.description.Height, srcLevel)
	srcLayerCount := uint32(1)
	switch src.description.Target {
	case 2, 5:
	case 3:
		srcLayerCount = levelDimension(src.description.Depth, srcLevel)
	case 4:
		srcLayerCount = 6
	case 7, 8:
		srcLayerCount = src.description.ArraySize
	default:
		return false, nil
	}
	switch dst.description.Target {
	case 2, 3, 4, 5, 7, 8:
	default:
		return false, nil
	}

	readLayer := func(layer uint32) ([]byte, error) {
		imageTarget := src.textureTarget
		storageLayers := srcLayerCount
		storageLayer := layer
		if src.description.Target == 4 {
			imageTarget = glTextureCubeMapPositiveX + layer
			storageLayers = 1
			storageLayer = 0
		}
		full := make([]byte, int(srcWidth)*int(srcHeight)*int(storageLayers)*4)
		h.gl.bindTexture(src.textureTarget, src.texture)
		h.drainGLErrors()
		h.gl.getTexImage(imageTarget, int32(srcLevel), glRGB, glUnsignedInt5999Rev, glPointer(full))
		if glError := h.gl.getError(); glError != 0 {
			return nil, fmt.Errorf("VirGL shared-exponent copy readback GL error %#x", glError)
		}
		rowBytes := int(width) * 4
		region := make([]byte, rowBytes*int(height))
		for row := uint32(0); row < height; row++ {
			sourceOffset := ((int(storageLayer)*int(srcHeight)+int(srcY+row))*int(srcWidth) + int(srcX)) * 4
			copy(region[int(row)*rowBytes:], full[sourceOffset:sourceOffset+rowBytes])
		}
		return region, nil
	}

	for layer := uint32(0); layer < depth; layer++ {
		region, err := readLayer(srcZ + layer)
		if err != nil {
			return true, err
		}
		h.gl.bindTexture(dst.textureTarget, dst.texture)
		h.drainGLErrors()
		switch dst.description.Target {
		case 3, 7, 8:
			h.gl.texSubImage3D(dst.textureTarget, int32(dstLevel), int32(dstX), int32(dstY), int32(dstZ+layer),
				int32(width), int32(height), 1, glRGB, glUnsignedInt5999Rev, glPointer(region))
		case 4:
			h.gl.texSubImage2D(glTextureCubeMapPositiveX+dstZ+layer, int32(dstLevel), int32(dstX), int32(dstY),
				int32(width), int32(height), glRGB, glUnsignedInt5999Rev, glPointer(region))
		default:
			h.gl.texSubImage2D(dst.textureTarget, int32(dstLevel), int32(dstX), int32(dstY),
				int32(width), int32(height), glRGB, glUnsignedInt5999Rev, glPointer(region))
		}
		if glError := h.gl.getError(); glError != 0 {
			return true, fmt.Errorf("VirGL shared-exponent copy upload GL error %#x", glError)
		}
	}
	return true, nil
}

func virglCopyY(resource *hostResource, level uint32, y, height int32) (int32, int32) {
	if resource.description.Flags&1 == 0 {
		return y, y + height
	}
	resourceHeight := int32(resource.description.Height >> level)
	if resourceHeight == 0 {
		resourceHeight = 1
	}
	return resourceHeight - y - height, resourceHeight - y
}

type darwinTextureFormatDescription struct {
	internal      int32
	external      uint32
	dataType      uint32
	render        bool
	depth         bool
	stencil       bool
	packedStencil bool
}

func describeDarwinTextureFormat(format uint32) (darwinTextureFormatDescription, bool) {
	color := func(internal int32, external, dataType uint32) (darwinTextureFormatDescription, bool) {
		return darwinTextureFormatDescription{internal: internal, external: external, dataType: dataType, render: true}, true
	}
	switch format {
	case virglFormatB8G8R8A8UNorm, virglFormatB8G8R8X8UNorm:
		return color(glRGBA8, glBGRA, glUnsignedByte)
	case virglFormatR8G8B8A8UNorm, virglFormatX8B8G8R8UNorm, virglFormatR8G8B8X8UNorm:
		return color(glRGBA8, glRGBA, glUnsignedByte)
	case virglFormatA8B8G8R8UNorm:
		return color(glRGBA8, glRGBA, glUnsignedInt8888)
	case virglFormatR8UNorm:
		return color(glR8, glRed, glUnsignedByte)
	case virglFormatR8G8UNorm:
		return color(glRG8, glRG, glUnsignedByte)
	case virglFormatR8G8B8UNorm:
		return color(glRGB8, glRGB, glUnsignedByte)
	case virglFormatR16UNorm:
		return color(glR16, glRed, glUnsignedShort)
	case virglFormatR16G16UNorm:
		return color(glRG16, glRG, glUnsignedShort)
	case virglFormatR16G16B16UNorm:
		return color(glRGB16, glRGB, glUnsignedShort)
	case virglFormatR16G16B16A16UNorm:
		return color(glRGBA16, glRGBA, glUnsignedShort)
	case virglFormatR8SNorm:
		return color(glR8SNorm, glRed, glByte)
	case virglFormatR8G8SNorm:
		return color(glRG8SNorm, glRG, glByte)
	case virglFormatR8G8B8SNorm:
		return color(glRGB8SNorm, glRGB, glByte)
	case virglFormatR8G8B8A8SNorm:
		return color(glRGBA8SNorm, glRGBA, glByte)
	case virglFormatR16SNorm:
		return color(glR16SNorm, glRed, glShort)
	case virglFormatR16G16SNorm:
		return color(glRG16SNorm, glRG, glShort)
	case virglFormatR16G16B16SNorm:
		return color(glRGB16SNorm, glRGB, glShort)
	case virglFormatR16G16B16A16SNorm:
		return color(glRGBA16SNorm, glRGBA, glShort)
	case virglFormatR8UInt:
		return color(glR8UI, glRedInteger, glUnsignedByte)
	case virglFormatR8G8UInt:
		return color(glRG8UI, glRGInteger, glUnsignedByte)
	case virglFormatR8G8B8UInt:
		return color(glRGB8UI, glRGBInteger, glUnsignedByte)
	case virglFormatR8G8B8A8UInt:
		return color(glRGBA8UI, glRGBAInteger, glUnsignedByte)
	case virglFormatR8SInt:
		return color(glR8I, glRedInteger, glByte)
	case virglFormatR8G8SInt:
		return color(glRG8I, glRGInteger, glByte)
	case virglFormatR8G8B8SInt:
		return color(glRGB8I, glRGBInteger, glByte)
	case virglFormatR8G8B8A8SInt:
		return color(glRGBA8I, glRGBAInteger, glByte)
	case virglFormatR16UInt:
		return color(glR16UI, glRedInteger, glUnsignedShort)
	case virglFormatR16G16UInt:
		return color(glRG16UI, glRGInteger, glUnsignedShort)
	case virglFormatR16G16B16UInt:
		return color(glRGB16UI, glRGBInteger, glUnsignedShort)
	case virglFormatR16G16B16A16UInt:
		return color(glRGBA16UI, glRGBAInteger, glUnsignedShort)
	case virglFormatR16SInt:
		return color(glR16I, glRedInteger, glShort)
	case virglFormatR16G16SInt:
		return color(glRG16I, glRGInteger, glShort)
	case virglFormatR16G16B16SInt:
		return color(glRGB16I, glRGBInteger, glShort)
	case virglFormatR16G16B16A16SInt:
		return color(glRGBA16I, glRGBAInteger, glShort)
	case virglFormatR16Float:
		return color(glR16F, glRed, glHalfFloat)
	case virglFormatR16G16Float:
		return color(glRG16F, glRG, glHalfFloat)
	case virglFormatR16G16B16Float:
		return color(glRGB16F, glRGB, glHalfFloat)
	case virglFormatR16G16B16A16Float:
		return color(glRGBA16F, glRGBA, glHalfFloat)
	case virglFormatR32Float:
		return color(glR32F, glRed, glFloat)
	case virglFormatR32G32Float:
		return color(glRG32F, glRG, glFloat)
	case virglFormatR32G32B32Float:
		return color(glRGB32F, glRGB, glFloat)
	case virglFormatR32G32B32A32Float:
		return color(glRGBA32F, glRGBA, glFloat)
	case virglFormatR32G32B32A32UInt:
		return color(glRGBA32UI, glRGBAInteger, glUnsignedInt)
	case virglFormatR32G32B32A32SInt:
		return color(glRGBA32I, glRGBAInteger, glInt)
	case virglFormatR32UInt:
		return color(glR32UI, glRedInteger, glUnsignedInt)
	case virglFormatR32G32UInt:
		return color(glRG32UI, glRGInteger, glUnsignedInt)
	case virglFormatR32G32B32UInt:
		return color(glRGB32UI, glRGBInteger, glUnsignedInt)
	case virglFormatR32SInt:
		return color(glR32I, glRedInteger, glInt)
	case virglFormatR32G32SInt:
		return color(glRG32I, glRGInteger, glInt)
	case virglFormatR32G32B32SInt:
		return color(glRGB32I, glRGBInteger, glInt)
	case virglFormatR8G8B8SRGB:
		return color(glSRGB8, glRGB, glUnsignedByte)
	case virglFormatR8G8B8A8SRGB:
		return color(glSRGB8Alpha8, glRGBA, glUnsignedByte)
	case virglFormatA8B8G8R8SRGB:
		return color(glSRGB8Alpha8, glRGBA, glUnsignedInt8888)
	case virglFormatB8G8R8A8SRGB:
		return color(glSRGB8Alpha8, glBGRA, glUnsignedByte)
	case virglFormatB8G8R8X8SRGB:
		return color(glSRGB8Alpha8, glBGRA, glUnsignedByte)
	case virglFormatR8G8B8X8SRGB:
		return color(glSRGB8Alpha8, glRGBA, glUnsignedByte)
	case virglFormatR10G10B10A2UNorm:
		return color(glRGB10A2, glRGBA, glUnsignedInt2101010Rev)
	case virglFormatB10G10R10A2UNorm:
		return color(glRGB10A2, glBGRA, glUnsignedInt2101010Rev)
	case virglFormatR10G10B10A2UInt:
		return color(glRGB10A2UI, glRGBAInteger, glUnsignedInt2101010Rev)
	case virglFormatB10G10R10A2UInt:
		return color(glRGB10A2UI, glBGRAInteger, glUnsignedInt2101010Rev)
	case virglFormatR11G11B10Float:
		return color(glR11FG11FB10F, glRGB, glUnsignedInt10F11F11FRev)
	case virglFormatR9G9B9E5Float:
		return darwinTextureFormatDescription{internal: glRGB9E5, external: glRGB, dataType: glUnsignedInt5999Rev}, true
	case virglFormatZ16UNorm:
		return darwinTextureFormatDescription{internal: glDepthComponent16, external: glDepthComponent, dataType: glUnsignedShort, depth: true}, true
	case virglFormatZ32Float:
		return darwinTextureFormatDescription{internal: glDepthComponent32F, external: glDepthComponent, dataType: glFloat, depth: true}, true
	case virglFormatZ24UNormS8UInt:
		return darwinTextureFormatDescription{internal: glDepth24Stencil8, external: glDepthStencil, dataType: glUnsignedInt248, depth: true, stencil: true}, true
	case virglFormatZ32FloatS8X24UInt:
		return darwinTextureFormatDescription{internal: glDepth32FStencil8, external: glDepthStencil, dataType: glFloat32UnsignedInt248Rev, depth: true, stencil: true}, true
	case virglFormatZ24X8UNorm:
		return darwinTextureFormatDescription{internal: glDepthComponent24, external: glDepthComponent, dataType: glUnsignedInt, depth: true}, true
	case virglFormatS8UInt:
		// macOS's frozen OpenGL 4.1 profile cannot allocate the later
		// GL_STENCIL_INDEX8 texture storage. Back a logical S8 VirGL resource
		// with packed D24S8 and expose only its stencil aspect.
		return darwinTextureFormatDescription{internal: glDepth24Stencil8, external: glStencilIndex, dataType: glUnsignedByte, stencil: true, packedStencil: true}, true
	default:
		return darwinTextureFormatDescription{}, false
	}
}

func vertexFormat(format uint32) (components int32, dataType uint32, normalized bool, ok bool) {
	switch {
	case format == 8:
		return 4, glUnsignedInt2101010Rev, true, true
	case format == 123:
		return 4, glUnsignedInt2101010Rev, false, true
	case format == 172:
		return 4, glInt2101010Rev, false, true
	case format == 173:
		return 4, glInt2101010Rev, true, true
	case format >= 28 && format <= 31:
		return int32(format - 27), glFloat, false, true
	case format >= 32 && format <= 35:
		return int32(format - 31), glUnsignedInt, true, true
	case format >= 36 && format <= 39:
		return int32(format - 35), glUnsignedInt, false, true
	case format >= 40 && format <= 43:
		return int32(format - 39), glInt, true, true
	case format >= 44 && format <= 47:
		return int32(format - 43), glInt, false, true
	case format >= 48 && format <= 51:
		return int32(format - 47), glUnsignedShort, true, true
	case format >= 52 && format <= 55:
		return int32(format - 51), glUnsignedShort, false, true
	case format >= 56 && format <= 59:
		return int32(format - 55), glShort, true, true
	case format >= 60 && format <= 63:
		return int32(format - 59), glShort, false, true
	case format >= 64 && format <= 67:
		return int32(format - 63), glUnsignedByte, true, true
	case format >= 69 && format <= 72:
		return int32(format - 68), glUnsignedByte, false, true
	case format >= 74 && format <= 77:
		return int32(format - 73), glByte, true, true
	case format >= 82 && format <= 85:
		return int32(format - 81), glByte, false, true
	case format >= 87 && format <= 90:
		return int32(format - 86), glFixed, false, true
	case format >= 91 && format <= 94:
		return int32(format - 90), glHalfFloat, false, true
	case format >= virglFormatR32UInt && format <= virglFormatR32G32B32A32UInt:
		// TGSI has one typeless 32-bit register file. Integer vertex values,
		// including the paired words Mesa uses for 64-bit attributes, must reach
		// the translated vec4 inputs without numeric conversion so the shader's
		// floatBitsToUint operation recovers the original words.
		return int32(format - virglFormatR32UInt + 1), glFloat, false, true
	case format >= virglFormatR32SInt && format <= virglFormatR32G32B32A32SInt:
		return int32(format - virglFormatR32SInt + 1), glFloat, false, true
	default:
		return 0, 0, false, false
	}
}

func integerVertexFormat(format uint32) (components int32, dataType uint32, signed bool, ok bool) {
	switch {
	case format >= virglFormatR8UInt && format <= virglFormatR8G8B8A8UInt:
		return int32(format - virglFormatR8UInt + 1), glUnsignedByte, false, true
	case format >= virglFormatR8SInt && format <= virglFormatR8G8B8A8SInt:
		return int32(format - virglFormatR8SInt + 1), glByte, true, true
	case format >= virglFormatR16UInt && format <= virglFormatR16G16B16A16UInt:
		return int32(format - virglFormatR16UInt + 1), glUnsignedShort, false, true
	case format >= virglFormatR16SInt && format <= virglFormatR16G16B16A16SInt:
		return int32(format - virglFormatR16SInt + 1), glShort, true, true
	case format >= virglFormatR32UInt && format <= virglFormatR32G32B32A32UInt:
		return int32(format - virglFormatR32UInt + 1), glUnsignedInt, false, true
	case format >= virglFormatR32SInt && format <= virglFormatR32G32B32A32SInt:
		return int32(format - virglFormatR32SInt + 1), glInt, true, true
	default:
		return 0, 0, false, false
	}
}

func vertexFormatByteSize(format uint32) (uint32, bool) {
	components, dataType, _, ok := integerVertexFormat(format)
	if !ok {
		components, dataType, _, ok = vertexFormat(format)
	}
	if !ok {
		return 0, false
	}
	if dataType == glUnsignedInt2101010Rev || dataType == glInt2101010Rev {
		return 4, true
	}
	componentBytes := uint32(4)
	switch dataType {
	case glByte, glUnsignedByte:
		componentBytes = 1
	case glShort, glUnsignedShort, glHalfFloat:
		componentBytes = 2
	}
	return uint32(components) * componentBytes, true
}

func streamOutputVertexCapacity(context *hostContext, elements []hostVertexElement) (uint32, bool) {
	capacity, found := uint32(math.MaxUint32), false
	for _, element := range elements {
		if element.bufferIndex >= uint32(len(context.vertexBuffers)) || element.instanceDivisor != 0 {
			continue
		}
		binding := context.vertexBuffers[element.bufferIndex]
		if binding.resource == nil || binding.stride == 0 {
			continue
		}
		size, ok := vertexFormatByteSize(element.format)
		if !ok {
			continue
		}
		offset := uint64(binding.offset) + uint64(element.offset)
		length := uint64(len(binding.resource.bufferBytes))
		var available uint32
		if offset+uint64(size) <= length {
			available = 1 + uint32((length-offset-uint64(size))/uint64(binding.stride))
		}
		capacity, found = min(capacity, available), true
	}
	return capacity, found
}

func integerVertexAttributeValue(data []byte, offset, format uint32) ([4]uint32, bool, error) {
	components, dataType, signed, ok := integerVertexFormat(format)
	if !ok {
		return [4]uint32{}, false, nil
	}
	componentBytes := uint32(1)
	if dataType == glUnsignedShort || dataType == glShort {
		componentBytes = 2
	} else if dataType == glUnsignedInt || dataType == glInt {
		componentBytes = 4
	}
	end := uint64(offset) + uint64(components)*uint64(componentBytes)
	if end > uint64(len(data)) {
		return [4]uint32{}, true, fmt.Errorf("range %d..%d exceeds %d-byte buffer", offset, end, len(data))
	}
	value := [4]uint32{0, 0, 0, 1}
	for component := uint32(0); component < uint32(components); component++ {
		start := offset + component*componentBytes
		switch componentBytes {
		case 1:
			if signed {
				value[component] = uint32(int32(int8(data[start])))
			} else {
				value[component] = uint32(data[start])
			}
		case 2:
			raw := binary.LittleEndian.Uint16(data[start:])
			if signed {
				value[component] = uint32(int32(int16(raw)))
			} else {
				value[component] = uint32(raw)
			}
		case 4:
			value[component] = binary.LittleEndian.Uint32(data[start:])
		}
	}
	return value, true, nil
}

func vertexAttributeRawValue(data []byte, offset, format uint32) ([4]float32, error) {
	integer, ok, err := integerVertexAttributeValue(data, offset, format)
	if err != nil {
		return [4]float32{}, err
	}
	if ok {
		return [4]float32{
			math.Float32frombits(integer[0]), math.Float32frombits(integer[1]),
			math.Float32frombits(integer[2]), math.Float32frombits(integer[3]),
		}, nil
	}
	return constantVertexAttribute(data, offset, format)
}

func constantVertexAttribute(data []byte, offset, format uint32) ([4]float32, error) {
	components, dataType, normalized, ok := vertexFormat(format)
	if !ok {
		return [4]float32{}, fmt.Errorf("unsupported format %d", format)
	}
	if dataType == glUnsignedInt2101010Rev || dataType == glInt2101010Rev {
		end := uint64(offset) + 4
		if end > uint64(len(data)) {
			return [4]float32{}, fmt.Errorf("range %d..%d exceeds %d-byte buffer", offset, end, len(data))
		}
		raw := binary.LittleEndian.Uint32(data[offset:])
		values := [4]float32{
			float32(raw & 0x3ff), float32(raw >> 10 & 0x3ff),
			float32(raw >> 20 & 0x3ff), float32(raw >> 30 & 0x3),
		}
		if dataType == glInt2101010Rev {
			values = [4]float32{
				float32(int32(raw<<22) >> 22), float32(int32(raw<<12) >> 22),
				float32(int32(raw<<2) >> 22), float32(int32(raw) >> 30),
			}
		}
		if normalized {
			if dataType == glUnsignedInt2101010Rev {
				values = [4]float32{values[0] / 1023, values[1] / 1023, values[2] / 1023, values[3] / 3}
			} else {
				values = [4]float32{max(values[0]/511, -1), max(values[1]/511, -1), max(values[2]/511, -1), max(values[3], -1)}
			}
		}
		return values, nil
	}
	componentBytes := 1
	if dataType == glFloat || dataType == glFixed || dataType == glUnsignedInt || dataType == glInt {
		componentBytes = 4
	} else if dataType == glUnsignedShort || dataType == glShort || dataType == glHalfFloat {
		componentBytes = 2
	}
	end := uint64(offset) + uint64(components)*uint64(componentBytes)
	if end > uint64(len(data)) {
		return [4]float32{}, fmt.Errorf("range %d..%d exceeds %d-byte buffer", offset, end, len(data))
	}
	value := [4]float32{0, 0, 0, 1}
	for component := 0; component < int(components); component++ {
		start := int(offset) + component*componentBytes
		switch dataType {
		case glFloat:
			value[component] = math.Float32frombits(binary.LittleEndian.Uint32(data[start:]))
		case glFixed:
			value[component] = float32(int32(binary.LittleEndian.Uint32(data[start:]))) / 65536
		case glUnsignedInt:
			raw := binary.LittleEndian.Uint32(data[start:])
			value[component] = float32(raw)
			if normalized {
				value[component] = float32(float64(raw) / float64(math.MaxUint32))
			}
		case glInt:
			raw := int32(binary.LittleEndian.Uint32(data[start:]))
			value[component] = float32(raw)
			if normalized {
				value[component] = max(float32(float64(raw)/float64(math.MaxInt32)), -1)
			}
		case glHalfFloat:
			value[component] = halfFloat32(binary.LittleEndian.Uint16(data[start:]))
		case glUnsignedShort:
			raw := binary.LittleEndian.Uint16(data[start:])
			value[component] = float32(raw)
			if normalized {
				value[component] /= 65535
			}
		case glShort:
			raw := int16(binary.LittleEndian.Uint16(data[start:]))
			value[component] = float32(raw)
			if normalized {
				value[component] = max(value[component]/32767, -1)
			}
		case glUnsignedByte:
			value[component] = float32(data[start])
			if normalized {
				value[component] /= 255
			}
		case glByte:
			value[component] = float32(int8(data[start]))
			if normalized {
				value[component] = max(value[component]/127, -1)
			}
		}
	}
	return value, nil
}

func halfFloat32(value uint16) float32 {
	sign := uint32(value&0x8000) << 16
	exponent := uint32(value>>10) & 0x1f
	mantissa := uint32(value & 0x03ff)
	if exponent == 0 {
		if mantissa == 0 {
			return math.Float32frombits(sign)
		}
		exponent = 113
		for mantissa&0x0400 == 0 {
			mantissa <<= 1
			exponent--
		}
		mantissa &= 0x03ff
	} else if exponent == 0x1f {
		exponent = 0xff
	} else {
		exponent += 112
	}
	return math.Float32frombits(sign | exponent<<23 | mantissa<<13)
}

func (h *darwinHost) retainResource(resource *hostResource) {
	if resource != nil {
		resource.references++
	}
}

func (h *darwinHost) publishBuffer(resource *hostResource) {
	if resource == nil || resource.buffer == 0 || !resource.bufferDirty {
		return
	}
	h.gl.bindBuffer(glArrayBuffer, resource.buffer)
	start, end := resource.bufferDirtyStart, resource.bufferDirtyEnd
	if start == 0 && end == uint32(len(resource.bufferBytes)) {
		h.gl.bufferData(glArrayBuffer, len(resource.bufferBytes), glPointer(resource.bufferBytes), glStreamDraw)
	} else {
		bytes := resource.bufferBytes[int(start):int(end)]
		h.gl.bufferSubData(glArrayBuffer, int(start), len(bytes), glPointer(bytes))
	}
	resource.bufferDirty = false
}

func (h *darwinHost) markBufferDirty(resource *hostResource, start, size uint32) {
	if size == 0 {
		return
	}
	end := start + size
	if !resource.bufferDirty {
		resource.bufferDirty = true
		resource.bufferDirtyStart = start
		resource.bufferDirtyEnd = end
		return
	}
	if start < resource.bufferDirtyStart {
		resource.bufferDirtyStart = start
	}
	if end > resource.bufferDirtyEnd {
		resource.bufferDirtyEnd = end
	}
}

func (h *darwinHost) releaseResource(resource *hostResource) {
	if resource == nil {
		return
	}
	resource.references--
	if resource.references > 0 {
		return
	}
	h.deleteResource(resource)
	delete(h.allResources, resource)
}

func (h *darwinHost) deleteSamplerView(view hostSamplerView) {
	if view.texture != 0 {
		h.gl.deleteTextures(1, &view.texture)
	}
}

func (h *darwinHost) releaseContextResources(context *hostContext) {
	for _, subcontext := range context.subcontexts {
		h.releaseContextResources(subcontext)
	}
	if context.conditionalQuery != 0 {
		h.gl.endConditionalRender()
		context.conditionalQuery = 0
	}
	if context.currentStreamout != nil {
		h.gl.bindTransformFeedback(glTransformFeedback, 0)
		context.currentStreamout = nil
	}
	for _, object := range context.streamoutObjects {
		h.gl.deleteTransformFeedbacks(1, &object.id)
	}
	for _, surface := range context.surfaces {
		h.releaseResource(surface.resource)
	}
	for _, view := range context.samplerViews {
		h.deleteSamplerView(view)
		h.releaseResource(view.resource)
	}
	for _, target := range context.streamoutTargets {
		h.releaseResource(target.resource)
	}
	for _, query := range context.queries {
		h.gl.deleteQueries(1, &query.id)
		h.releaseResource(query.resource)
	}
	for _, state := range context.samplerStates {
		h.gl.deleteSamplers(1, &state.id)
	}
	for _, binding := range context.vertexBuffers {
		h.releaseResource(binding.resource)
	}
	for stage := range context.uniformBuffers {
		for _, binding := range context.uniformBuffers[stage] {
			h.releaseResource(binding.resource)
		}
	}
	h.releaseResource(context.indexResource)
	h.deleteContextPrograms(context)
	if h.activeContext == context {
		h.activeContext = nil
	}
}

func (h *darwinHost) deleteContextPrograms(context *hostContext) {
	for key, program := range h.programs {
		if key.context == context {
			h.deleteProgram(key, program)
		}
	}
}

func (h *darwinHost) deleteProgramsForShader(context *hostContext, handle uint32) {
	for key, program := range h.programs {
		if key.context == context && (key.vertexHandle == handle || key.fragmentHandle == handle || key.geometryHandle == handle ||
			key.tessControlHandle == handle || key.tessEvaluationHandle == handle) {
			h.deleteProgram(key, program)
		}
	}
}

func (h *darwinHost) evictPrograms(limit int) {
	for len(h.programs) > limit {
		var oldestKey hostProgramKey
		var oldestProgram hostProgram
		found := false
		for key, program := range h.programs {
			if program.id == h.currentProgram {
				continue
			}
			if !found || program.lastUsed < oldestProgram.lastUsed {
				oldestKey = key
				oldestProgram = program
				found = true
			}
		}
		if !found {
			return
		}
		h.deleteProgram(oldestKey, oldestProgram)
	}
}

func (h *darwinHost) deleteProgram(key hostProgramKey, program hostProgram) {
	if h.currentProgram == program.id {
		h.gl.useProgram(0)
		h.currentProgram = 0
	}
	h.gl.deleteProgram(program.id)
	delete(h.programs, key)
}

func (h *darwinHost) deleteResource(resource *hostResource) {
	if resource.framebuffer != 0 {
		h.gl.deleteFramebuffers(1, &resource.framebuffer)
	}
	if resource.texture != 0 {
		h.gl.deleteTextures(1, &resource.texture)
	}
	if resource.buffer != 0 {
		h.gl.deleteBuffers(1, &resource.buffer)
	}
}

func (h *darwinHost) releaseGLObjects() {
	h.releaseNativeFrames()
	for resource := range h.allResources {
		h.deleteResource(resource)
	}
	if h.vao != 0 {
		h.gl.deleteVertexArrays(1, &h.vao)
	}
	if h.emulatedVertexIDBuffer != 0 {
		h.gl.deleteBuffers(1, &h.emulatedVertexIDBuffer)
	}
	if h.emulatedVertexTable != 0 {
		h.gl.deleteBuffers(1, &h.emulatedVertexTable)
	}
	for stage := range h.constantStagingBuffers {
		for buffer := range h.constantStagingBuffers[stage] {
			if h.constantStagingBuffers[stage][buffer] != 0 {
				h.gl.deleteBuffers(1, &h.constantStagingBuffers[stage][buffer])
			}
		}
	}
	for stage := range h.constantBufferTextures {
		for buffer := range h.constantBufferTextures[stage] {
			if h.constantBufferTextures[stage][buffer] != 0 {
				h.gl.deleteTextures(1, &h.constantBufferTextures[stage][buffer])
			}
		}
	}
	for index := range h.retiredConstantBuffers {
		h.gl.deleteBuffers(1, &h.retiredConstantBuffers[index])
	}
	if h.zeroUniformBuffer != 0 {
		h.gl.deleteBuffers(1, &h.zeroUniformBuffer)
	}
	if h.blitReadFBO != 0 {
		h.gl.deleteFramebuffers(1, &h.blitReadFBO)
	}
	if h.blitDrawFBO != 0 {
		h.gl.deleteFramebuffers(1, &h.blitDrawFBO)
	}
	if h.depthOnlyFBO != 0 {
		h.gl.deleteFramebuffers(1, &h.depthOnlyFBO)
	}
	if h.discardFBO != 0 {
		h.gl.deleteFramebuffers(1, &h.discardFBO)
	}
	if h.discardTexture != 0 {
		h.gl.deleteTextures(1, &h.discardTexture)
	}
	for _, program := range h.programs {
		h.gl.deleteProgram(program.id)
	}
}

func (h *darwinHost) releaseNativeFrames() {
	for index := range h.nativeFrames {
		frame := &h.nativeFrames[index]
		if frame.producerFence != 0 {
			h.gl.deleteSync(frame.producerFence)
		}
		if frame.consumerFence != 0 {
			h.gl.deleteSync(frame.consumerFence)
		}
		if frame.texture != 0 {
			h.gl.deleteTextures(1, &frame.texture)
		}
		*frame = hostNativeFrame{}
	}
}

func (h *darwinHost) close() error {
	h.once.Do(func() {
		close(h.stop)
		<-h.done
	})
	return nil
}
