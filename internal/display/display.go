// Package display defines the host-facing interface for a graphical VM.
package display

import (
	"image"
)

// FramebufferUpdate is an XRGB8888 framebuffer snapshot. Pixels contain only
// Rect, tightly packed by row, with each pixel in blue, green, red, unused
// byte order.
type FramebufferUpdate struct {
	Width      int
	Height     int
	Generation uint64
	Rect       image.Rectangle
	Pixels     []byte
}

// CursorUpdate is the guest-provided ARGB cursor image. Pixels are tightly
// packed in blue, green, red, alpha byte order.
type CursorUpdate struct {
	X, Y               int
	PositionGeneration uint64
	Width              int
	Height             int
	HotX               int
	HotY               int
	Visible            bool
	Generation         uint64
	Pixels             []byte
}

// OpenGLFrame is a leased texture produced in the presentation context's
// OpenGL share group. The producer fence orders guest rendering before
// sampling. Release must be called exactly once; consumerFence may identify a
// fence inserted after the final sampling command, or zero when the frame was
// never sampled.
type OpenGLFrame struct {
	Width, Height int
	Generation    uint64
	Damage        image.Rectangle
	Texture       uint32
	ProducerFence uintptr
	release       func(uintptr)
}

func NewOpenGLFrame(
	width, height int,
	generation uint64,
	damage image.Rectangle,
	texture uint32,
	producerFence uintptr,
	release func(uintptr),
) OpenGLFrame {
	return OpenGLFrame{
		Width: width, Height: height, Generation: generation, Damage: damage,
		Texture: texture, ProducerFence: producerFence, release: release,
	}
}

func (f OpenGLFrame) Release(consumerFence uintptr) {
	if f.release != nil {
		f.release(consumerFence)
	}
}

// Session provides direct access to a running VM's graphical desktop. Key
// accepts Linux input-event key codes. Pointer uses absolute guest coordinates
// and the conventional VNC button mask: left=1, middle=2, right=4, wheel=8/16.
type Session interface {
	Size() (width, height int)
	Snapshot(request image.Rectangle, since uint64, incremental bool) FramebufferUpdate
	Changed() <-chan struct{}
	Resize(width, height int) error
	Key(code uint16, down bool) error
	Pointer(x, y uint32, buttons, previousButtons uint8) error
	SetClipboard(text string)
	GuestClipboard() (text string, generation uint64)
}

// OpenGLFrameSession is an optional zero-copy presentation capability.
type OpenGLFrameSession interface {
	AcquireOpenGLFrame(since uint64) (OpenGLFrame, bool, error)
}

// HighResolutionScroller is implemented by display sessions that accept
// horizontal and vertical wheel movement in v120 units, where 120 is one
// conventional wheel detent.
type HighResolutionScroller interface {
	Scroll(deltaX120, deltaY120 int32) error
}

// CursorProvider is implemented by display sessions that expose the guest's
// hardware cursor independently from the framebuffer.
type CursorProvider interface {
	Cursor() CursorUpdate
}

// RelativePointerSession sends unscaled mouse counts through a relative device.
type RelativePointerSession interface {
	RelativePointer(dx, dy int32, buttons, previousButtons uint8) error
	RelativeScroll(dx120, dy120 int32) error
}
