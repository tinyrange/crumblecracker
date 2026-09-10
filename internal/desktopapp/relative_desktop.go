package desktopapp

import (
	"time"
	"unsafe"

	"github.com/tinyrange/crumblecracker/internal/display"
	"github.com/tinyrange/gowin/gl"
	"github.com/tinyrange/gowin/window"
)

type relativeCursorPresentation struct {
	captured     bool
	x, y         float64
	shape        uint64
	hostX, hostY float32
}

func (v *displayViewer) relativeCursorPresentation() relativeCursorPresentation {
	if !v.relativeDesktop {
		return relativeCursorPresentation{}
	}
	state := relativeCursorPresentation{captured: v.mouseCaptured}
	if v.mouseCaptured {
		state.x, state.y = v.virtualX, v.virtualY
		if provider, ok := v.session.(display.CursorProvider); ok {
			state.shape = provider.Cursor().Generation
		}
	} else {
		state.hostX, state.hostY = v.window.Cursor()
	}
	return state
}

// Relative desktops never send absolute device events. The virtual cursor is
// expressed in guest pixels; native relative counts remain unscaled.
func (v *displayViewer) reconcileRelativeCursor() {
	provider, ok := v.session.(display.CursorProvider)
	if !ok {
		return
	}
	cursor := provider.Cursor()
	if cursor.PositionGeneration == 0 || cursor.PositionGeneration == v.cursorPositionGeneration {
		return
	}
	// Do not replace predicted movement with feedback still in flight. Application
	// warps and desktop clamping settle back to the guest position when input stops.
	if time.Since(v.lastRelativeMotion) < 80*time.Millisecond {
		return
	}
	v.virtualX, v.virtualY = float64(cursor.X), float64(cursor.Y)
	v.cursorPositionGeneration = cursor.PositionGeneration
}

func (v *displayViewer) guestPointerGeometry() (width, height, top float32) {
	w, h := v.window.BackingSize()
	if v.chromeEnabled {
		top = appChromeHeight * normalizedDisplayScale(v.window.Scale())
	}
	return float32(w), float32(h) - top, top
}

func (v *displayViewer) handleRelativeDesktopEntry(event window.InputEvent) (bool, error) {
	if event.Type != window.InputEventMouseMove && event.Type != window.InputEventMouseDown && event.Type != window.InputEventMouseUp {
		return false, nil
	}
	width, height, top := v.guestPointerGeometry()
	inside := width > 0 && height > 0 && event.MouseX >= 0 && event.MouseX < width && event.MouseY >= top && event.MouseY < top+height
	if !inside {
		v.mouseReentryBlocked = false
		return false, nil
	}
	if event.Type == window.InputEventMouseUp {
		return true, nil
	}
	if !v.mouseEdgeReleasedAt.IsZero() && time.Since(v.mouseEdgeReleasedAt) >= 100*time.Millisecond && event.MouseX >= 4 && event.MouseX < width-4 && event.MouseY >= top+4 && event.MouseY < top+height-4 {
		v.mouseReentryBlocked = false
	}
	if !v.mouseCaptureFocused || !v.mouseCaptureAvailable() || (v.mouseReentryBlocked && event.Type != window.InputEventMouseDown) {
		return true, nil
	}
	v.reconcileRelativeCursor()
	// Cursor feedback is required to align entry without an absolute device.
	provider, ok := v.session.(display.CursorProvider)
	if !ok || provider.Cursor().PositionGeneration == 0 {
		return true, nil
	}
	gw, gh := v.session.Size()
	targetX := float64(event.MouseX / width * float32(gw))
	targetY := float64((event.MouseY - top) / height * float32(gh))
	if err := v.setMouseCaptured(true); err != nil {
		return true, err
	}
	v.mouseLocked = false
	dx, dy := int32(targetX-v.virtualX), int32(targetY-v.virtualY)
	if err := v.session.(display.RelativePointerSession).RelativePointer(dx, dy, 0, 0); err != nil {
		return true, err
	}
	v.virtualX += float64(dx)
	v.virtualY += float64(dy)
	v.lastRelativeMotion = time.Now()
	// The click belongs to the guest; the entry movement has already been sent.
	return event.Type == window.InputEventMouseMove, nil
}

func (v *displayViewer) releaseRelativeCursor() error {
	width, height, top := v.guestPointerGeometry()
	gw, gh := v.session.Size()
	x, y := v.virtualX, v.virtualY
	err := v.setMouseCaptured(false)
	if gw > 0 && gh > 0 {
		warpRelativeHostCursor(v.window, float32(x)*width/float32(gw), top+float32(y)*height/float32(gh))
	}
	return err
}

func (v *displayViewer) drawRelativeDesktopCursor(backingWidth, backingHeight int) {
	if !v.relativeDesktop || !v.mouseCaptured {
		return
	}
	provider, ok := v.session.(display.CursorProvider)
	if !ok {
		return
	}
	cursor := provider.Cursor()
	if !cursor.Visible || cursor.Width <= 0 || cursor.Height <= 0 || cursor.Width > 256 || cursor.Height > 256 || len(cursor.Pixels) < cursor.Width*cursor.Height*4 {
		return
	}
	if v.cursorTexture == 0 {
		v.gl.GenTextures(1, &v.cursorTexture)
	}
	if cursor.Generation != v.cursorTextureGeneration {
		v.gl.BindTexture(gl.Texture2D, v.cursorTexture)
		v.gl.TexParameteri(gl.Texture2D, gl.TextureMinFilter, gl.Nearest)
		v.gl.TexParameteri(gl.Texture2D, gl.TextureMagFilter, gl.Nearest)
		v.gl.TexParameteri(gl.Texture2D, gl.TextureWrapS, gl.ClampToEdge)
		v.gl.TexParameteri(gl.Texture2D, gl.TextureWrapT, gl.ClampToEdge)
		v.gl.TexImage2D(gl.Texture2D, 0, int32(gl.RGBA), int32(cursor.Width), int32(cursor.Height), 0, glBGRA, gl.UnsignedByte, unsafe.Pointer(&cursor.Pixels[0]))
		v.cursorTextureGeneration = cursor.Generation
	}
	width, height, top := v.guestPointerGeometry()
	gw, gh := v.session.Size()
	if gw <= 0 || gh <= 0 {
		return
	}
	sx, sy := width/float32(gw), height/float32(gh)
	v.gl.Enable(gl.ScissorTest)
	v.gl.Scissor(0, 0, int32(backingWidth), int32(height))
	v.drawTexture(v.cursorTexture, backingWidth, backingHeight, 1, (float32(v.virtualX)-float32(cursor.HotX))*sx, top+(float32(v.virtualY)-float32(cursor.HotY))*sy, float32(cursor.Width)*sx, float32(cursor.Height)*sy)
	v.gl.Disable(gl.ScissorTest)
}
