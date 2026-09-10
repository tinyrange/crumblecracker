//go:build !windows

package desktopapp

import "github.com/tinyrange/gowin/window"

type platformClipboard struct{ window.Clipboard }

func newHostClipboard() (hostClipboard, error)        { return platformClipboard{window.GetClipboard()}, nil }
func (c platformClipboard) ReadText() (string, error) { return c.GetText(), nil }
func (c platformClipboard) Close()                    {}
