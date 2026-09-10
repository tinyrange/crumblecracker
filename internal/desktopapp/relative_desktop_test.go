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
	if w.captured || !v.mouseLocked || len(w.warped) != 1 {
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

func TestForceLockSurvivesReleaseAndRecapture(t *testing.T) {
	for _, release := range []string{"shortcut", "focus loss"} {
		t.Run(release, func(t *testing.T) {
			v, w, s := relativeViewer(t)
			v.reconcileRelativeCursor()
			if err := v.setMouseForceLocked(true); err != nil {
				t.Fatal(err)
			}
			if release == "focus loss" {
				if err := v.syncMouseCapture(false); err != nil {
					t.Fatal(err)
				}
			} else {
				w.events = []window.InputEvent{{Type: window.InputEventFlagsChanged, Mods: window.ModCtrl | window.ModAlt}}
				if err := v.handleInput(); err != nil {
					t.Fatal(err)
				}
			}
			if w.captured || !v.mouseLocked {
				t.Fatal("temporary release lost force-lock mode")
			}
			if err := v.syncMouseCapture(true); err != nil {
				t.Fatal(err)
			}
			if w.captured {
				t.Fatal("focus gain captured without user input")
			}
			s.relative = nil
			// Re-entry at a distant host coordinate must not move the game camera.
			w.events = []window.InputEvent{{Type: window.InputEventMouseDown, Button: window.ButtonLeft, MouseX: 1500, MouseY: 1200}}
			if err := v.handleInput(); err != nil {
				t.Fatal(err)
			}
			if !w.captured || !v.mouseLocked || len(s.relative) != 1 || s.relative[0].x != 0 || s.relative[0].y != 0 || s.relative[0].buttons != 1 {
				t.Fatalf("recapture=%v mode=%v input=%v", w.captured, v.mouseLocked, s.relative)
			}
			// Fast movement repeatedly crosses every edge without releasing capture.
			for _, delta := range []image.Point{image.Pt(100000, 100000), image.Pt(-200000, -200000), image.Pt(200000, -200000)} {
				if err := v.sendRelativePointer(float32(delta.X), float32(delta.Y)); err != nil {
					t.Fatal(err)
				}
				if !w.captured {
					t.Fatal("force lock escaped at an edge")
				}
				last := s.relative[len(s.relative)-1]
				if last.x != int32(delta.X) || last.y != int32(delta.Y) {
					t.Fatalf("motion changed: %+v", last)
				}
			}
		})
	}
}

func TestForceLockToolbarCanRestoreAutomaticEdges(t *testing.T) {
	v, w, _ := relativeViewer(t)
	width, _ := w.BackingSize()
	_, button := toolbarActionBounds(float32(width)/w.Scale(), v.chromeInsets, true, false)
	click := window.InputEvent{Type: window.InputEventMouseDown, Button: window.ButtonLeft, MouseX: float32(button.Min.X+2) * w.Scale(), MouseY: float32(button.Min.Y+2) * w.Scale()}
	if !v.handleChromeInput(click) || !v.mouseLocked || !w.captured {
		t.Fatal("toolbar did not force lock")
	}
	if err := v.releaseRelativeCursor(); err != nil {
		t.Fatal(err)
	}
	if !v.handleChromeInput(click) || v.mouseLocked || w.captured {
		t.Fatal("toolbar did not turn force lock off")
	}
	w.events = []window.InputEvent{{Type: window.InputEventMouseMove, MouseX: 500, MouseY: 500}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if !w.captured {
		t.Fatal("desktop entry did not resume")
	}
	if err := v.sendRelativePointer(-100000, 0); err != nil {
		t.Fatal(err)
	}
	if w.captured {
		t.Fatal("automatic edge release was not restored")
	}
}
