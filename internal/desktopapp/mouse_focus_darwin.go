//go:build darwin

package desktopapp

import "github.com/ebitengine/purego/objc"

func mouseCaptureWindowFocused() bool {
	app := objc.ID(objc.GetClass("NSApplication")).Send(objc.RegisterName("sharedApplication"))
	return app != 0 && objc.Send[bool](app, objc.RegisterName("isActive")) && app.Send(objc.RegisterName("keyWindow")) != 0
}
