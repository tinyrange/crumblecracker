package desktopapp

import (
	"errors"
	"fmt"
	"image"
	"math"

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
	}
	var err error
	if v.mouseCaptured {
		if relative, ok := v.session.(display.RelativePointerSession); ok {
			err = relative.RelativePointer(0, 0, 0, v.sentButtons)
		}
	} else if v.sentButtons != 0 {
		err = v.session.Pointer(v.lastPointerX, v.lastPointerY, 0, v.sentButtons)
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
	x := consumeMouseDelta(dx, &v.mouseRemainderX)
	y := consumeMouseDelta(dy, &v.mouseRemainderY)
	if err := relative.RelativePointer(x, y, v.buttons, v.sentButtons); err != nil {
		return err
	}
	v.sentButtons = v.buttons
	return nil
}

func toolbarActionBounds(insets window.TitleBarInsets, capture bool) (folder, mouse image.Rectangle) {
	left := int(insets.Left) + 8
	folder = image.Rect(left, 3, left+120, int(appChromeHeight)-3)
	if capture {
		mouse = image.Rect(folder.Max.X+6, 3, folder.Max.X+170, int(appChromeHeight)-3)
	}
	return
}

func (v *displayViewer) syncMouseCapture(focused bool) error {
	if v.mouseCaptured && (!focused || !v.desktopVisible) {
		return v.setMouseCaptured(false)
	}
	return nil
}
