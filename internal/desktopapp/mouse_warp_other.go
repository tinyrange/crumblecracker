//go:build !darwin

package desktopapp

import "github.com/tinyrange/gowin/window"

func warpRelativeHostCursor(w window.Window, x, y float32) {
	if host, ok := w.(interface{ WarpCursor(float32, float32) }); ok {
		host.WarpCursor(x, y)
	}
}
