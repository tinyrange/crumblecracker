//go:build !windows

package desktopapp

import "github.com/tinyrange/crumblecracker/internal/display"

type noopGuestCursorHost struct{}

func newGuestCursorHost() guestCursorHost { return noopGuestCursorHost{} }

func (noopGuestCursorHost) Apply(display.CursorUpdate, bool) error { return nil }
func (noopGuestCursorHost) Close()                                 {}
