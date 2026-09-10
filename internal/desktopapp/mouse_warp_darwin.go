//go:build darwin

package desktopapp

import (
	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
	"github.com/tinyrange/gowin/window"
	"sync"
)

type cursorPoint struct{ X, Y float64 }
type cursorSize struct{ Width, Height float64 }
type cursorRect struct {
	Origin cursorPoint
	Size   cursorSize
}

var cursorWarpOnce sync.Once
var cursorWarp func(cursorPoint) int32

// Input coordinates are backing pixels measured from the content view's top
// left. AppKit uses screen points from the bottom left, while CoreGraphics uses
// the primary display's top left (including negative secondary-display origins).
func warpRelativeHostCursor(w window.Window, x, y float32) {
	if host, ok := w.(interface{ WarpCursor(float32, float32) }); ok {
		host.WarpCursor(x, y)
		return
	}
	cursorWarpOnce.Do(func() {
		library, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil {
			purego.RegisterLibFunc(&cursorWarp, library, "CGWarpMouseCursorPosition")
		}
	})
	if cursorWarp == nil {
		return
	}
	app := objc.ID(objc.GetClass("NSApplication")).Send(objc.RegisterName("sharedApplication"))
	win := app.Send(objc.RegisterName("keyWindow"))
	if win == 0 {
		return
	}
	view := win.Send(objc.RegisterName("contentView"))
	bounds := objc.Send[cursorRect](view, objc.RegisterName("bounds"))
	scale := float64(normalizedDisplayScale(w.Scale()))
	p := cursorPoint{float64(x) / scale, bounds.Size.Height - float64(y)/scale}
	p = objc.Send[cursorPoint](view, objc.RegisterName("convertPoint:toView:"), p, objc.ID(0))
	p = objc.Send[cursorPoint](win, objc.RegisterName("convertPointToScreen:"), p)
	screens := objc.ID(objc.GetClass("NSScreen")).Send(objc.RegisterName("screens"))
	primary := screens.Send(objc.RegisterName("objectAtIndex:"), uint64(0))
	frame := objc.Send[cursorRect](primary, objc.RegisterName("frame"))
	cursorWarp(cursorPoint{p.X, frame.Origin.Y + frame.Size.Height - p.Y})
}
