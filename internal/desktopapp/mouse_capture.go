package desktopapp

import (
	"errors"
	"fmt"
	"image"
	"math"
	"time"

	"github.com/tinyrange/crumblecracker/internal/display"
	"github.com/tinyrange/gowin/window"
)

func (v *displayViewer) mouseCaptureAvailable() bool {
	_, host := v.window.(window.CursorCaptureSupport)
	_, guest := v.session.(display.RelativePointerSession)
	return appConfig.Kind == "squadvm" && host && guest && v.desktopVisible && v.mouseCaptureReady
}

func (v *displayViewer) setMouseCaptured(captured bool) error {
	if v.mouseCaptured == captured {
		return nil
	}
	host, ok := v.window.(window.CursorCaptureSupport)
	if !ok {
		return fmt.Errorf("mouse capture is unavailable")
	}
	if captured && !v.mouseCaptureAvailable() {
		return fmt.Errorf("relative mouse input is unavailable")
	}
	// Release the host first, even if the guest has already disconnected.
	if !captured {
		host.SetCursorCaptured(false)
		v.mouseLocked = false
		v.mouseReentryBlocked = true
		v.mouseEdgeReleasedAt = time.Time{}
	}
	var err error
	if v.mouseCaptured {
		if relative, ok := v.session.(display.RelativePointerSession); ok {
			err = relative.RelativePointer(0, 0, 0, v.sentButtons)
		}
	} else if v.sentButtons != 0 {
		if v.automation != nil {
			err = v.automation.pointer(v.lastPointerX, v.lastPointerY, 0)
		} else {
			err = v.session.Pointer(v.lastPointerX, v.lastPointerY, 0, v.sentButtons)
		}
	}
	if captured && err != nil {
		return err
	}
	v.buttons, v.sentButtons = 0, 0
	v.mouseRemainderX, v.mouseRemainderY = 0, 0
	v.scrollX120Remainder, v.scrollY120Remainder = 0, 0
	v.mouseCaptured = captured
	if captured {
		host.SetCursorCaptured(true)
	} else {
		for key, down := range v.keysDown {
			if code, ok := linuxKeycode(key); down && ok && v.session != nil {
				err = errors.Join(err, v.session.Key(code, false))
			}
			v.keysDown[key] = false
		}
	}
	return err
}

func consumeMouseDelta(delta float32, remainder *float64) int32 {
	whole, fraction := math.Modf(float64(delta) + *remainder)
	*remainder = fraction
	return int32(max(float64(math.MinInt32), min(float64(math.MaxInt32), whole)))
}

func (v *displayViewer) sendRelativePointer(dx, dy float32) error {
	relative, ok := v.session.(display.RelativePointerSession)
	if !ok {
		return fmt.Errorf("relative pointer unavailable")
	}
	// Native movement counts are not framebuffer pixels: never multiply by DPI
	// or scale them through the guest desktop's absolute-coordinate range.
	if v.relativeDesktop {
		v.reconcileRelativeCursor()
	}
	x := consumeMouseDelta(dx, &v.mouseRemainderX)
	y := consumeMouseDelta(dy, &v.mouseRemainderY)
	if err := relative.RelativePointer(x, y, v.buttons, v.sentButtons); err != nil {
		return err
	}
	v.sentButtons = v.buttons
	if v.relativeDesktop {
		v.virtualX += float64(x)
		v.virtualY += float64(y)
		if x != 0 || y != 0 {
			v.lastRelativeMotion = time.Now()
		}
		width, height := v.session.Size()
		if !v.mouseLocked && (v.virtualX < 0 || v.virtualY < 0 || v.virtualX >= float64(width) || v.virtualY >= float64(height)) {
			// Limit the overshoot to a small host-side handoff, even for fast motion.
			v.virtualX = max(-2, min(float64(width)+1, v.virtualX))
			v.virtualY = max(-2, min(float64(height)+1, v.virtualY))
			err := v.releaseRelativeCursor()
			v.mouseEdgeReleasedAt = time.Now()
			return err
		}
		v.virtualX = max(0, min(float64(width-1), v.virtualX))
		v.virtualY = max(0, min(float64(height-1), v.virtualY))
	}
	return nil
}

func toolbarActionBounds(width float32, insets window.TitleBarInsets, capture, cvmfs bool) (folder, mouse image.Rectangle) {
	buttonWidth := 164
	right := int(width-insets.Right) - 8
	if cvmfs {
		right = cvmfsChromeStatusBounds(width, insets).Min.X - 8
	}
	if capture {
		// Keep room for a centered title when the window is narrow.
		buttonWidth = min(buttonWidth, max(100, (int(width/2)-80-int(insets.Right)-16)/2))
		mouse = image.Rect(right-buttonWidth, 3, right, int(appChromeHeight)-3)
		right = mouse.Min.X - 8
	}
	folder = image.Rect(right-buttonWidth, 3, right, int(appChromeHeight)-3)
	return
}

func (v *displayViewer) syncMouseCapture(focused bool) error {
	v.mouseCaptureFocused = focused
	if v.relativeDesktop {
		v.reconcileRelativeCursor()
	}
	if v.mouseCaptured && (!focused || !v.desktopVisible) {
		return v.setMouseCaptured(false)
	}
	return nil
}
