package virgl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

const (
	eglNone                    = 0x3038
	eglExtensions              = 0x3055
	eglSurfaceType             = 0x3033
	eglRenderableType          = 0x3040
	eglOpenGLBit               = 0x0008
	eglOpenGLAPI               = 0x30a2
	eglContextMajorVersion     = 0x3098
	eglContextMinorVersion     = 0x30fb
	eglContextProfileMask      = 0x30fd
	eglContextCoreProfileBit   = 0x0001
	eglPlatformGBM             = 0x31d7
	eglPlatformSurfacelessMesa = 0x31dd
)

type eglAPI struct {
	getError           func() uint32
	queryString        func(uintptr, int32) string
	getPlatformDisplay func(uint32, uintptr, uintptr) uintptr
	initialize         func(uintptr, *int32, *int32) uint32
	terminate          func(uintptr) uint32
	bindAPI            func(uint32) uint32
	chooseConfig       func(uintptr, *int32, *uintptr, int32, *int32) uint32
	createContext      func(uintptr, uintptr, uintptr, *int32) uintptr
	destroyContext     func(uintptr, uintptr) uint32
	makeCurrent        func(uintptr, uintptr, uintptr, uintptr) uint32
	getProcAddress     func(string) uintptr
	releaseThread      func() uint32
}

// EGL shares the surfaceless display across contexts in this process.
var eglDisplays = struct {
	sync.Mutex
	users map[uintptr]int
}{users: make(map[uintptr]int)}

func NewHostRenderer() (virtio.GPURenderer, error) {
	host, err := newLinuxHost(false)
	if err != nil {
		return nil, err
	}
	return newHostRenderer(host)
}

func NewHostRendererWithShareGroup(shareContext, sharePixelFormat uintptr) (virtio.GPURenderer, error) {
	if shareContext != 0 || sharePixelFormat != 0 {
		return nil, errors.New("Linux VirGL does not support shared-context presentation")
	}
	return NewHostRenderer()
}

func newReplayHost() (*openGLHost, error) {
	return newLinuxHost(os.Getenv("LIBGL_ALWAYS_SOFTWARE") == "1")
}

func newLinuxHost(software bool) (*openGLHost, error) {
	return newOpenGLHost(func() (*hostGL, func(), error) {
		if software {
			return openEGLContext("")
		}
		if node := os.Getenv("VMSH_VIRGL_RENDER_NODE"); node != "" {
			return openEGLContext(node)
		}
		nodes, err := filepath.Glob("/dev/dri/renderD*")
		if err != nil {
			return nil, nil, fmt.Errorf("find VirGL render nodes: %w", err)
		}
		if len(nodes) == 0 {
			return nil, nil, errors.New("VirGL requires an accessible DRM render node in /dev/dri")
		}
		var failures []error
		for _, node := range nodes {
			gl, cleanup, err := openEGLContext(node)
			if err == nil {
				return gl, cleanup, nil
			}
			failures = append(failures, err)
		}
		return nil, nil, errors.Join(failures...)
	}, false)
}

// Keep the driver loaders mapped for the life of the process. Each renderer
// still destroys its EGL context, display reference, GBM device and DRM fd.
var loadEGL = sync.OnceValues(func() (eglAPI, error) {
	lib, err := purego.Dlopen("libEGL.so.1", purego.RTLD_LOCAL|purego.RTLD_NOW)
	if err != nil {
		return eglAPI{}, fmt.Errorf("load EGL for VirGL: %w", err)
	}
	var egl eglAPI
	for _, entry := range []struct {
		name string
		fn   any
	}{
		{"eglGetError", &egl.getError},
		{"eglQueryString", &egl.queryString},
		{"eglGetPlatformDisplay", &egl.getPlatformDisplay},
		{"eglInitialize", &egl.initialize},
		{"eglTerminate", &egl.terminate},
		{"eglBindAPI", &egl.bindAPI},
		{"eglChooseConfig", &egl.chooseConfig},
		{"eglCreateContext", &egl.createContext},
		{"eglDestroyContext", &egl.destroyContext},
		{"eglMakeCurrent", &egl.makeCurrent},
		{"eglGetProcAddress", &egl.getProcAddress},
		{"eglReleaseThread", &egl.releaseThread},
	} {
		address, err := purego.Dlsym(lib, entry.name)
		if err != nil {
			_ = purego.Dlclose(lib)
			return eglAPI{}, err
		}
		purego.RegisterFunc(entry.fn, address)
	}
	return egl, nil
})

type gbmAPI struct {
	createDevice  func(int32) uintptr
	destroyDevice func(uintptr)
}

var loadGBM = sync.OnceValues(func() (gbmAPI, error) {
	lib, err := purego.Dlopen("libgbm.so.1", purego.RTLD_LOCAL|purego.RTLD_NOW)
	if err != nil {
		return gbmAPI{}, fmt.Errorf("load GBM: %w", err)
	}
	createAddress, err := purego.Dlsym(lib, "gbm_create_device")
	if err != nil {
		_ = purego.Dlclose(lib)
		return gbmAPI{}, err
	}
	destroyAddress, err := purego.Dlsym(lib, "gbm_device_destroy")
	if err != nil {
		_ = purego.Dlclose(lib)
		return gbmAPI{}, err
	}
	var gbm gbmAPI
	purego.RegisterFunc(&gbm.createDevice, createAddress)
	purego.RegisterFunc(&gbm.destroyDevice, destroyAddress)
	return gbm, nil
})

// openEGLContext is called on the renderer thread. An empty node selects the
// Mesa surfaceless platform for software replay.
func openEGLContext(node string) (_ *hostGL, _ func(), err error) {
	egl, err := loadEGL()
	if err != nil {
		return nil, nil, err
	}
	var (
		display, context     uintptr
		device               uintptr
		gbm                  gbmAPI
		file                 *os.File
		initialized, current bool
	)
	cleanup := func() {
		if current {
			egl.makeCurrent(display, 0, 0, 0)
		}
		if context != 0 {
			egl.destroyContext(display, context)
		}
		if initialized {
			releaseEGLDisplay(egl, display)
		}
		egl.releaseThread()
		if device != 0 {
			gbm.destroyDevice(device)
		}
		if file != nil {
			_ = file.Close()
		}
	}
	source := node
	if source == "" {
		source = "Mesa surfaceless"
	}
	defer func() {
		if err != nil {
			cleanup()
			err = fmt.Errorf("VirGL EGL on %s: %w", source, err)
		}
	}()
	eglError := func(operation string) error {
		return fmt.Errorf("%s failed (EGL error %#x)", operation, egl.getError())
	}

	platform := uint32(eglPlatformSurfacelessMesa)
	extensions := strings.Fields(egl.queryString(0, eglExtensions))
	if node != "" {
		platform = eglPlatformGBM
		if !slices.Contains(extensions, "EGL_KHR_platform_gbm") && !slices.Contains(extensions, "EGL_MESA_platform_gbm") {
			return nil, nil, errors.New("EGL does not support the GBM platform")
		}
		file, err = os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			return nil, nil, err
		}
		gbm, err = loadGBM()
		if err != nil {
			return nil, nil, err
		}
		device = gbm.createDevice(int32(file.Fd()))
		if device == 0 {
			return nil, nil, errors.New("create GBM device")
		}
	} else if !slices.Contains(extensions, "EGL_MESA_platform_surfaceless") {
		return nil, nil, errors.New("EGL does not support the Mesa surfaceless platform")
	}

	display = egl.getPlatformDisplay(platform, device, 0)
	if display == 0 {
		return nil, nil, eglError("get platform display")
	}
	if err := retainEGLDisplay(egl, display); err != nil {
		return nil, nil, err
	}
	initialized = true
	if egl.bindAPI(eglOpenGLAPI) == 0 {
		return nil, nil, eglError("bind desktop OpenGL API")
	}
	configAttrs := [...]int32{eglSurfaceType, 0, eglRenderableType, eglOpenGLBit, eglNone}
	var config uintptr
	var count int32
	if egl.chooseConfig(display, &configAttrs[0], &config, 1, &count) == 0 {
		return nil, nil, eglError("choose OpenGL config")
	}
	if count == 0 {
		return nil, nil, errors.New("no EGL config supports desktop OpenGL")
	}
	contextAttrs := [...]int32{
		eglContextMajorVersion, 4, eglContextMinorVersion, 1,
		eglContextProfileMask, eglContextCoreProfileBit, eglNone,
	}
	context = egl.createContext(display, config, 0, &contextAttrs[0])
	if context == 0 {
		return nil, nil, eglError("create OpenGL 4.1 core context")
	}
	if egl.makeCurrent(display, 0, 0, context) == 0 {
		return nil, nil, eglError("make offscreen context current")
	}
	current = true

	resolve := func(name string) (uintptr, error) {
		address := egl.getProcAddress(name)
		if address == 0 {
			return 0, errors.New("entry point unavailable")
		}
		return address, nil
	}
	gl, err := loadHostGL(resolve)
	if err != nil {
		return nil, nil, err
	}
	if err := checkLinuxGL(gl, resolve, node == ""); err != nil {
		return nil, nil, err
	}
	return gl, cleanup, nil
}

func retainEGLDisplay(egl eglAPI, display uintptr) error {
	eglDisplays.Lock()
	defer eglDisplays.Unlock()
	if eglDisplays.users[display] == 0 {
		var major, minor int32
		if egl.initialize(display, &major, &minor) == 0 {
			return fmt.Errorf("initialize display failed (EGL error %#x)", egl.getError())
		}
		if major < 1 || (major == 1 && minor < 5) {
			egl.terminate(display)
			return fmt.Errorf("EGL %d.%d is below the required 1.5", major, minor)
		}
	}
	eglDisplays.users[display]++
	return nil
}

func releaseEGLDisplay(egl eglAPI, display uintptr) {
	eglDisplays.Lock()
	defer eglDisplays.Unlock()
	eglDisplays.users[display]--
	if eglDisplays.users[display] == 0 {
		egl.terminate(display)
		delete(eglDisplays.users, display)
	}
}

func checkLinuxGL(gl *hostGL, resolve func(string) (uintptr, error), allowSoftware bool) error {
	getStringAddr, err := resolve("glGetString")
	if err != nil {
		return err
	}
	getIntegerAddr, err := resolve("glGetIntegerv")
	if err != nil {
		return err
	}
	var getString func(uint32) string
	var getInteger func(uint32, *int32)
	purego.RegisterFunc(&getString, getStringAddr)
	purego.RegisterFunc(&getInteger, getIntegerAddr)

	gl.renderer = getString(0x1f01) // GL_RENDERER
	if gl.renderer == "" {
		return errors.New("current OpenGL context returned no renderer identity")
	}
	renderer := strings.ToLower(gl.renderer)
	if !allowSoftware && (strings.Contains(renderer, "llvmpipe") || strings.Contains(renderer, "softpipe") ||
		strings.Contains(renderer, "swrast") || strings.Contains(renderer, "software rasterizer")) {
		return fmt.Errorf("hardware OpenGL required, got %q", gl.renderer)
	}

	// Keep the existing VirGL profile bounded by the selected driver.
	for _, limit := range []struct {
		name string
		enum uint32
		min  int32
	}{
		{"GL_MAX_TEXTURE_SIZE", 0x0d33, capsetMaxTexture2D},
		{"GL_MAX_CUBE_MAP_TEXTURE_SIZE", 0x851c, capsetMaxTexture2D},
		{"GL_MAX_3D_TEXTURE_SIZE", 0x8073, capsetMaxTexture3D},
		{"GL_MAX_ARRAY_TEXTURE_LAYERS", 0x88ff, capsetMaxArrayLayers},
		{"GL_MAX_TEXTURE_BUFFER_SIZE", 0x8c2b, capsetMaxTextureBuffer},
		{"GL_MAX_UNIFORM_BLOCK_SIZE", 0x8a30, capsetMaxUniformBlockSize},
		{"GL_MAX_DRAW_BUFFERS", 0x8824, 8},
		{"GL_MAX_COLOR_ATTACHMENTS", 0x8cdf, 8},
		{"GL_MAX_SAMPLES", 0x8d57, 4},
		{"GL_MAX_VIEWPORTS", 0x825b, 16},
	} {
		var value int32
		getInteger(limit.enum, &value)
		if value < limit.min {
			return fmt.Errorf("%s is %d on %q, VirGL requires at least %d", limit.name, value, gl.renderer, limit.min)
		}
	}
	var anisotropy float32
	gl.getFloatv(0x84ff, &anisotropy) // GL_MAX_TEXTURE_MAX_ANISOTROPY
	if anisotropy < capsetMaxAnisotropy {
		return fmt.Errorf("anisotropic filtering limit is %g on %q, VirGL requires %d", anisotropy, gl.renderer, capsetMaxAnisotropy)
	}
	// The default GL_FIXED_ONLY clamps negative signed-normalized texels
	// during glReadPixels. Guest transfers need the signed values.
	clampColorAddr, err := resolve("glClampColor")
	if err != nil {
		return err
	}
	var clampColor func(uint32, uint32)
	purego.RegisterFunc(&clampColor, clampColorAddr)
	clampColor(0x891c, 0) // GL_CLAMP_READ_COLOR, GL_FALSE

	var alignment int32
	getInteger(0x8a34, &alignment) // GL_UNIFORM_BUFFER_OFFSET_ALIGNMENT
	if code := gl.getError(); code != 0 || alignment <= 0 {
		return fmt.Errorf("invalid OpenGL limits on %q (alignment %d, GL error %#x)", gl.renderer, alignment, code)
	}
	gl.uniformBufferAlignment = uint32(alignment)
	return nil
}
