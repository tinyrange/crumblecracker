package desktopapp

import "github.com/tinyrange/crumblecracker/internal/display"

type guestCursorHost interface {
	Apply(update display.CursorUpdate, desktopVisible bool) error
	Close()
}
