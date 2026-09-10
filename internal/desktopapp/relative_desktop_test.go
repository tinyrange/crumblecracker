package desktopapp

import (
	"image"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/display"
	"github.com/tinyrange/gowin/window"
)

type relativeDesktopTestSession struct {
	*captureTestSession
	cursor display.CursorUpdate
}

func (s *relativeDesktopTestSession) Cursor() display.CursorUpdate { return s.cursor }

type relativeDesktopTestWindow struct {
	*captureTestWindow
	warped []image.Point
}

func (w *relativeDesktopTestWindow) WarpCursor(x, y float32) {
	w.warped = append(w.warped, image.Pt(int(x), int(y)))
}

func relativeViewer(t *testing.T) (*displayViewer, *relativeDesktopTestWindow, *relativeDesktopTestSession) {
	v, base, session := captureViewer(t)
	session.width, session.height = 1024, 768
	s := &relativeDesktopTestSession{captureTestSession: session, cursor: display.CursorUpdate{X: 512, Y: 384, PositionGeneration: 1}}
	w := &relativeDesktopTestWindow{captureTestWindow: base}
	v.window, v.session = w, s
	v.relativeDesktop = true
	v.mouseCaptureFocused = true
	return v, w, s
}

func TestRelativeDesktopEntryAndEdgeHandoff(t *testing.T) {
	v, w, s := relativeViewer(t)
	_, _, top := v.guestPointerGeometry()
	// Enter at guest (256,192), then drag past its left edge.
	w.events = []window.InputEvent{{Type: window.InputEventMouseDown, Button: window.ButtonLeft, MouseX: 512, MouseY: top + (1536-top)/4}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if !w.captured || v.virtualX != 256 || v.virtualY != 192 {
		t.Fatalf("entry capture=%v xy=%v,%v", w.captured, v.virtualX, v.virtualY)
	}
	if s.relative[0].x != -256 || s.relative[0].y != -192 || len(s.pointers) != 0 {
		t.Fatalf("entry input=%v absolute=%v", s.relative, s.pointers)
	}
	w.events = []window.InputEvent{{Type: window.InputEventMouseMove, MouseDeltaX: -300}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	last := s.relative[len(s.relative)-1]
	if w.captured || last.buttons != 0 || last.previous != 1 || v.buttons != 0 {
		t.Fatalf("edge release capture=%v last=%+v", w.captured, last)
	}
	if len(w.warped) != 1 || w.warped[0].X >= 0 {
		t.Fatalf("host edge=%v", w.warped)
	}
	// Queued pre-release input cannot immediately trap the host again.
	w.events = []window.InputEvent{{Type: window.InputEventMouseMove, MouseX: 512, MouseY: 500}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if w.captured {
		t.Fatal("immediate recapture")
	}
	v.mouseEdgeReleasedAt = time.Now().Add(-time.Second)
	s.cursor = display.CursorUpdate{X: 0, Y: 192, PositionGeneration: 2}
	v.lastRelativeMotion = time.Time{}
	w.events = []window.InputEvent{{Type: window.InputEventMouseMove, MouseX: 12, MouseY: 500}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if !w.captured || len(s.pointers) != 0 {
		t.Fatal("reentry failed or used absolute device")
	}
}

func TestRelativeDesktopGameLockIgnoresEdges(t *testing.T) {
	v, w, s := relativeViewer(t)
	v.reconcileRelativeCursor()
	v.mouseLocked = true
	if err := v.setMouseCaptured(true); err != nil {
		t.Fatal(err)
	}
	w.events = []window.InputEvent{{Type: window.InputEventMouseMove, MouseDeltaX: 2000, MouseDeltaY: -900}, {Type: window.InputEventScroll, ScrollY: 1}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if !w.captured || s.relative[0].x != 2000 || s.relative[0].y != -900 || len(w.warped) != 0 || len(s.relativeScroll) != 1 {
		t.Fatalf("locked input=%v warp=%v", s.relative, w.warped)
	}
	w.events = []window.InputEvent{{Type: window.InputEventFlagsChanged, Mods: window.ModCtrl | window.ModAlt}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if w.captured || v.mouseLocked || len(w.warped) != 1 {
		t.Fatal("manual release failed")
	}
}

func TestRelativeDesktopReconcilesGuestWarpWithoutReplayingStaleFeedback(t *testing.T) {
	v, _, s := relativeViewer(t)
	v.reconcileRelativeCursor()
	v.virtualX = 550
	v.lastRelativeMotion = time.Now()
	s.cursor = display.CursorUpdate{X: 520, Y: 384, PositionGeneration: 2}
	v.reconcileRelativeCursor()
	if v.virtualX != 550 {
		t.Fatal("in-flight feedback undid motion")
	}
	s.cursor = display.CursorUpdate{X: 200, Y: 100, PositionGeneration: 3}
	v.lastRelativeMotion = time.Time{}
	v.reconcileRelativeCursor()
	if v.virtualX != 200 || v.virtualY != 100 {
		t.Fatal("guest warp not reflected")
	}
}

func TestRelativeDesktopDoesNotCaptureUnfocusedWindow(t *testing.T) {
	v, w, s := relativeViewer(t)
	v.mouseCaptureFocused = false
	w.events = []window.InputEvent{{Type: window.InputEventMouseMove, MouseX: 512, MouseY: 500}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if w.captured || len(s.relative) != 0 || len(s.pointers) != 0 {
		t.Fatal("unfocused window consumed guest pointer input")
	}
}
