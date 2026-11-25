package virgl

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"golang.org/x/sys/windows"
)

var (
	wglDLL               = windows.NewLazySystemDLL("opengl32.dll")
	wglCreateContext     = wglDLL.NewProc("wglCreateContext")
	wglDeleteContext     = wglDLL.NewProc("wglDeleteContext")
	wglMakeCurrent       = wglDLL.NewProc("wglMakeCurrent")
	wglGetProcAddress    = wglDLL.NewProc("wglGetProcAddress")
	wglUser32            = windows.NewLazySystemDLL("user32.dll")
	wglCreateWindow      = wglUser32.NewProc("CreateWindowExW")
	wglDestroyWindow     = wglUser32.NewProc("DestroyWindow")
	wglGetDC             = wglUser32.NewProc("GetDC")
	wglReleaseDC         = wglUser32.NewProc("ReleaseDC")
	wglGDI32             = windows.NewLazySystemDLL("gdi32.dll")
	wglChoosePixelFormat = wglGDI32.NewProc("ChoosePixelFormat")
	wglSetPixelFormat    = wglGDI32.NewProc("SetPixelFormat")
)

const (
	wglContextMajorVersion = 0x2091
	wglContextMinorVersion = 0x2092
	wglContextProfileMask  = 0x9126
	wglContextCoreProfile  = 0x0001
)

// PIXELFORMATDESCRIPTOR. The unused fields are zero for the offscreen surface.
type wglPixelFormatDescriptor struct {
	size      uint16
	version   uint16
	flags     uint32
	pixelType uint8
	colorBits uint8
	_         [18]byte
	layerMask uint32
	visible   uint32
	damage    uint32
}

func NewHostRenderer() (virtio.GPURenderer, error) {
	host, err := newDefaultHost()
	if err != nil {
		return nil, err
	}
	return newHostRenderer(host)
}

func NewHostRendererWithShareGroup(shareContext, sharePixelFormat uintptr) (virtio.GPURenderer, error) {
	if shareContext != 0 || sharePixelFormat != 0 {
		return nil, errors.New("Windows VirGL does not support shared-context presentation")
	}
	return NewHostRenderer()
}

func newReplayHost() (*openGLHost, error) {
	return newDefaultHost()
}

func newDefaultHost() (*openGLHost, error) {
	return newOpenGLHost(openWGLContext, false)
}

// openWGLContext and its cleanup run on the renderer's locked OS thread.
func openWGLContext() (_ *hostGL, _ func(), err error) {
	const (
		windowStyle   = 0x86000000 // WS_POPUP | WS_CLIPSIBLINGS | WS_CLIPCHILDREN
		drawToWindow  = 0x00000004 // PFD_DRAW_TO_WINDOW
		supportOpenGL = 0x00000020 // PFD_SUPPORT_OPENGL
		doubleBuffer  = 0x00000001 // PFD_DOUBLEBUFFER
	)
	if err := wglDLL.Load(); err != nil {
		return nil, nil, fmt.Errorf("load Windows OpenGL: %w", err)
	}
	class, _ := windows.UTF16PtrFromString("STATIC")
	window, _, callErr := wglCreateWindow.Call(0, uintptr(unsafe.Pointer(class)), 0, windowStyle, 0, 0, 1, 1, 0, 0, 0, 0)
	if window == 0 {
		return nil, nil, fmt.Errorf("create hidden WGL window: %w", callErr)
	}
	dc, _, callErr := wglGetDC.Call(window)
	var bootstrap, context uintptr
	cleanup := func() {
		if bootstrap != 0 || context != 0 {
			wglMakeCurrent.Call(0, 0)
		}
		if context != 0 {
			wglDeleteContext.Call(context)
		}
		if bootstrap != 0 {
			wglDeleteContext.Call(bootstrap)
		}
		if dc != 0 {
			wglReleaseDC.Call(window, dc)
		}
		wglDestroyWindow.Call(window)
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	if dc == 0 {
		return nil, nil, fmt.Errorf("get WGL window device context: %w", callErr)
	}
	format := wglPixelFormatDescriptor{
		size:      uint16(unsafe.Sizeof(wglPixelFormatDescriptor{})),
		version:   1,
		flags:     drawToWindow | supportOpenGL | doubleBuffer,
		colorBits: 32,
	}
	pixelFormat, _, callErr := wglChoosePixelFormat.Call(dc, uintptr(unsafe.Pointer(&format)))
	if pixelFormat == 0 {
		return nil, nil, fmt.Errorf("choose WGL pixel format: %w", callErr)
	}
	if ok, _, callErr := wglSetPixelFormat.Call(dc, pixelFormat, uintptr(unsafe.Pointer(&format))); ok == 0 {
		return nil, nil, fmt.Errorf("set WGL pixel format: %w", callErr)
	}
	bootstrap, _, callErr = wglCreateContext.Call(dc)
	if bootstrap == 0 {
		return nil, nil, fmt.Errorf("create bootstrap WGL context: %w", callErr)
	}
	if ok, _, callErr := wglMakeCurrent.Call(dc, bootstrap); ok == 0 {
		return nil, nil, fmt.Errorf("activate bootstrap WGL context: %w", callErr)
	}
	createContext, err := resolveWGLProc("wglCreateContextAttribsARB")
	if err != nil {
		return nil, nil, fmt.Errorf("Windows VirGL requires a hardware OpenGL 4.1 core driver in this Windows session: %w", err)
	}
	attribs := [...]int32{
		wglContextMajorVersion, 4, wglContextMinorVersion, 1,
		wglContextProfileMask, wglContextCoreProfile, 0,
	}
	var createCoreContext func(uintptr, uintptr, *int32) uintptr
	purego.RegisterFunc(&createCoreContext, createContext)
	context = createCoreContext(dc, 0, &attribs[0])
	if context == 0 {
		return nil, nil, errors.New("create WGL OpenGL 4.1 core context")
	}
	if ok, _, callErr := wglMakeCurrent.Call(dc, context); ok == 0 {
		return nil, nil, fmt.Errorf("activate WGL core context: %w", callErr)
	}
	if ok, _, callErr := wglDeleteContext.Call(bootstrap); ok == 0 {
		return nil, nil, fmt.Errorf("delete bootstrap WGL context: %w", callErr)
	}
	bootstrap = 0

	gl, err := loadHostGL(resolveWGLProc)
	if err != nil {
		return nil, nil, err
	}
	if err := checkHostGL(gl, resolveWGLProc, false); err != nil {
		return nil, nil, err
	}
	return gl, cleanup, nil
}

func resolveWGLProc(name string) (uintptr, error) {
	cname, err := windows.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	address, _, _ := wglGetProcAddress.Call(uintptr(unsafe.Pointer(cname)))
	// Some drivers return sentinel values for unavailable functions.
	if address > 3 && address != ^uintptr(0) {
		return address, nil
	}
	proc := wglDLL.NewProc(name)
	if err := proc.Find(); err != nil {
		return 0, fmt.Errorf("%s is unavailable: %w", name, err)
	}
	return proc.Addr(), nil
}
