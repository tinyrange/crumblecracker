//go:build !windows

package desktopapp

const glassWindowFocusSupported = false

func prepareGlassWindow() (show, release func()) { return func() {}, func() {} }
