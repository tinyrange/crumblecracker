package desktopapp

import (
	"errors"
	"image"
	"testing"

	"github.com/tinyrange/gowin/window"
)

type captureTestWindow struct {
	resizeTestWindow
	events   []window.InputEvent
	captured bool
}

func (w *captureTestWindow) SetCursorCaptured(v bool) { w.captured = v }
func (w *captureTestWindow) DrainInputEvents() []window.InputEvent {
	events := w.events
	w.events = nil
	return events
}

type relativeTestEvent struct {
	x, y              int32
	buttons, previous uint8
}
type captureTestSession struct {
	*automationTestSession
	relative       []relativeTestEvent
	relativeScroll []image.Point
	failure        error
}

func (s *captureTestSession) RelativePointer(x, y int32, b, p uint8) error {
	s.relative = append(s.relative, relativeTestEvent{x, y, b, p})
	return s.failure
}
func (s *captureTestSession) RelativeScroll(x, y int32) error {
	s.relativeScroll = append(s.relativeScroll, image.Pt(int(x), int(y)))
	return nil
}

func captureViewer(t *testing.T) (*displayViewer, *captureTestWindow, *captureTestSession) {
	t.Helper()
	previous := appConfig
	appConfig.Kind = "squadvm"
	t.Cleanup(func() { appConfig = previous })
	w := &captureTestWindow{resizeTestWindow: resizeTestWindow{width: 2048, height: 1536, scale: 2}}
	s := &captureTestSession{automationTestSession: newAutomationTestSession()}
	v := &displayViewer{window: w, session: s, desktopVisible: true, mouseCaptureReady: true, chromeEnabled: true, keysDown: map[window.Key]bool{}}
	return v, w, s
}
func TestCapturedMouseRoutesUnscaledDeltasAndButtons(t *testing.T) {
	v, w, s := captureViewer(t)
	if err := v.setMouseCaptured(true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		w.events = append(w.events, window.InputEvent{Type: window.InputEventMouseMove, MouseX: 10, MouseY: 2, MouseDeltaX: .25, MouseDeltaY: -.5})
	}
	w.events = append(w.events, window.InputEvent{Type: window.InputEventMouseDown, Button: window.ButtonLeft}, window.InputEvent{Type: window.InputEventScroll, ScrollY: 1})
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	var x, y int32
	for _, e := range s.relative {
		x += e.x
		y += e.y
	}
	if x != 1 || y != -2 || len(s.pointers) != 0 {
		t.Fatalf("movement=(%d,%d), absolute=%v", x, y, s.pointers)
	}
	if len(s.relativeScroll) != 1 || s.relativeScroll[0] != image.Pt(0, 120) {
		t.Fatalf("scroll=%v", s.relativeScroll)
	}
	if err := v.setMouseCaptured(false); err != nil {
		t.Fatal(err)
	}
	last := s.relative[len(s.relative)-1]
	if w.captured || last.buttons != 0 || last.previous != 1 {
		t.Fatalf("capture=%v release=%+v", w.captured, last)
	}
	w.events = []window.InputEvent{{Type: window.InputEventMouseMove, MouseX: 1000, MouseY: 1000}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if len(s.pointers) != 1 {
		t.Fatalf("absolute mode not restored: %v", s.pointers)
	}
}
func TestCaptureReleaseChordReleasesGuestKeys(t *testing.T) {
	v, w, s := captureViewer(t)
	if err := v.setMouseCaptured(true); err != nil {
		t.Fatal(err)
	}
	v.keysDown[window.KeyLeftControl] = true
	w.events = []window.InputEvent{{Type: window.InputEventFlagsChanged, Key: window.KeyLeftAlt, Mods: window.ModCtrl | window.ModAlt}}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	if w.captured || v.mouseCaptured {
		t.Fatal("release chord left cursor captured")
	}
	if len(s.keys) != 1 || s.keys[0].down {
		t.Fatalf("guest key release=%v", s.keys)
	}
}
func TestCaptureReleasesHostEvenWhenGuestFails(t *testing.T) {
	v, w, s := captureViewer(t)
	if err := v.setMouseCaptured(true); err != nil {
		t.Fatal(err)
	}
	s.failure = errors.New("guest disconnected")
	if err := v.setMouseCaptured(false); err == nil {
		t.Fatal("missing guest error")
	}
	if w.captured || v.mouseCaptured {
		t.Fatal("failed guest trapped host cursor")
	}
}
func TestToolbarOpensConfiguredSharedFolder(t *testing.T) {
	previousOpen := openSharedFolder
	t.Cleanup(func() { openSharedFolder = previousOpen })
	for _, kind := range []string{"squadvm", "ndappx"} {
		t.Run(kind, func(t *testing.T) {
			v, _, s := captureViewer(t)
			appConfig.Kind = kind
			v.settings.SharedFolder = t.TempDir()
			var opened string
			openSharedFolder = func(path string) error { opened = path; return nil }
			folder, _ := toolbarActionBounds(v.chromeInsets, v.mouseCaptureAvailable())
			event := window.InputEvent{Type: window.InputEventMouseDown, Button: window.ButtonLeft, MouseX: float32(folder.Min.X+2) * 2, MouseY: float32(folder.Min.Y+2) * 2}
			if !v.handleChromeInput(event) || opened != v.settings.SharedFolder || len(s.pointers) != 0 {
				t.Fatalf("folder routing: %q", opened)
			}
			v.settings.SharedFolder = t.TempDir()
			v.handleChromeInput(event)
			if opened != v.settings.SharedFolder {
				t.Fatal("opened stale shared folder")
			}
		})
	}
}

func TestCaptureRequiresGuestDeviceReadiness(t *testing.T) {
	v, w, _ := captureViewer(t)
	v.mouseCaptureReady = false
	if v.mouseCaptureAvailable() {
		t.Fatal("capture advertised for an unsupported image")
	}
	if err := v.setMouseCaptured(true); err == nil || w.captured {
		t.Fatal("captured without guest device readiness")
	}
}
func TestFocusLossReleasesCaptureWithoutAutomaticRecapture(t *testing.T) {
	v, w, s := captureViewer(t)
	if err := v.setMouseCaptured(true); err != nil {
		t.Fatal(err)
	}
	v.buttons = 1
	if err := v.sendRelativePointer(0, 0); err != nil {
		t.Fatal(err)
	}
	v.keysDown[window.KeyW] = true
	if err := v.syncMouseCapture(false); err != nil {
		t.Fatal(err)
	}
	if w.captured || v.mouseCaptured || s.relative[len(s.relative)-1].buttons != 0 {
		t.Fatal("focus loss did not release mouse")
	}
	if len(s.keys) != 1 || s.keys[0].down {
		t.Fatalf("keys on focus loss: %v", s.keys)
	}
	if err := v.syncMouseCapture(true); err != nil {
		t.Fatal(err)
	}
	if w.captured {
		t.Fatal("focus gain recaptured mouse without user action")
	}
}

func TestGuestDragReleasesOverToolbar(t *testing.T) {
	v, w, s := captureViewer(t)
	previous := openSharedFolder
	t.Cleanup(func() { openSharedFolder = previous })
	openSharedFolder = func(string) error { t.Fatal("guest drag activated toolbar"); return nil }
	w.events = []window.InputEvent{
		{Type: window.InputEventMouseDown, Button: window.ButtonLeft, MouseX: 500, MouseY: 500},
		{Type: window.InputEventMouseMove, MouseX: 50, MouseY: 10},
		{Type: window.InputEventMouseUp, Button: window.ButtonLeft, MouseX: 50, MouseY: 10},
	}
	if err := v.handleInput(); err != nil {
		t.Fatal(err)
	}
	last := s.pointers[len(s.pointers)-1]
	if last.buttons != 0 || last.previous != 1 || v.buttons != 0 {
		t.Fatalf("drag release: %+v", last)
	}
}
func TestCaptureKeepsAutomationButtonStateConsistent(t *testing.T) {
	v, _, s := captureViewer(t)
	a := &desktopAutomation{}
	a.setSession(s)
	v.automation = a
	if err := v.sendPointer(500, 500, 1); err != nil {
		t.Fatal(err)
	}
	if err := v.setMouseCaptured(true); err != nil {
		t.Fatal(err)
	}
	if a.pointerButtons != 0 {
		t.Fatal("capture retained absolute automation button")
	}
	if err := v.setMouseCaptured(false); err != nil {
		t.Fatal(err)
	}
	if err := v.sendPointer(500, 500, 1); err != nil {
		t.Fatal(err)
	}
	last := s.pointers[len(s.pointers)-1]
	if last.previous != 0 || last.buttons != 1 {
		t.Fatalf("first click after capture: %+v", last)
	}
}
