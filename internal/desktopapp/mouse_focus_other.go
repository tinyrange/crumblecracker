//go:build !darwin

package desktopapp

// The current native window backend only implements cursor capture on macOS.
func mouseCaptureWindowFocused() bool { return false }
