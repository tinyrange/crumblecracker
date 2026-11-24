package virgl

import (
	"errors"
	"fmt"
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
)

func NewHostRenderer() (virtio.GPURenderer, error) {
	return NewHostRendererWithShareGroup(0, 0)
}

func NewHostRendererWithShareGroup(shareContext, sharePixelFormat uintptr) (virtio.GPURenderer, error) {
	host, err := newDarwinHost(shareContext, sharePixelFormat)
	if err != nil {
		return nil, err
	}
	return newHostRenderer(host)
}

func newReplayHost() (*openGLHost, error) { return newDarwinHost() }

func newDarwinHost(shareGroup ...uintptr) (*openGLHost, error) {
	var shareContext, sharePixelFormat uintptr
	if len(shareGroup) > 0 {
		shareContext = shareGroup[0]
	}
	if len(shareGroup) > 1 {
		sharePixelFormat = shareGroup[1]
	}
	return newOpenGLHost(func() (*hostGL, func(), error) {
		return openDarwinContext(shareContext, sharePixelFormat)
	}, shareContext != 0 && sharePixelFormat != 0)
}

func openDarwinContext(shareContext, sharePixelFormat uintptr) (*hostGL, func(), error) {
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_GLOBAL|purego.RTLD_LAZY); err != nil {
		return nil, nil, fmt.Errorf("load AppKit for VirGL: %w", err)
	}
	handle, err := purego.Dlopen("/System/Library/Frameworks/OpenGL.framework/OpenGL", purego.RTLD_GLOBAL|purego.RTLD_LAZY)
	if err != nil {
		return nil, nil, fmt.Errorf("load OpenGL for VirGL: %w", err)
	}
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
		return nil, nil, errors.New("create VirGL autorelease pool")
	}
	closePool := func() { pool.Send(release) }

	attributes := []uint32{
		nsOpenGLPFAAccelerated,
		nsOpenGLPFAColorSize, 24,
		nsOpenGLPFADepthSize, 24,
		nsOpenGLPFAStencilSize, 8,
		nsOpenGLPFAOpenGLProfile, nsOpenGLProfileVersion41Core,
		0,
	}
	closeFormat := func() {}
	format := objc.ID(sharePixelFormat)
	if format == 0 {
		format = objc.ID(objc.GetClass("NSOpenGLPixelFormat")).Send(alloc)
		format = format.Send(initWithAttributes, unsafe.Pointer(&attributes[0]))
		if format == 0 {
			closePool()
			return nil, nil, errDarwinAcceleratedOpenGLUnavailable
		}
		closeFormat = func() { format.Send(release) }
	}

	context := objc.ID(objc.GetClass("NSOpenGLContext")).Send(alloc)
	context = context.Send(initWithFormat, format, objc.ID(shareContext))
	if context == 0 {
		closeFormat()
		closePool()
		return nil, nil, errors.New("create VirGL OpenGL 4.1 context")
	}
	context.Send(makeCurrent)

	closeContext := func() {
		objc.ID(objc.GetClass("NSOpenGLContext")).Send(clearCurrent)
		context.Send(release)
		closeFormat()
		closePool()
	}
	gl, err := loadHostGL(func(name string) (uintptr, error) {
		return purego.Dlsym(handle, name)
	})
	if err != nil {
		closeContext()
		return nil, nil, err
	}
	return gl, closeContext, nil
}
