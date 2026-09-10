package desktopapp

import (
	"fmt"
	"testing"
)

func TestClipboardReconciliationPreservesNewestSide(t *testing.T) {
	tests := []struct {
		name      string
		cached    string
		cachedGen uint64
		host      string
		guest     string
		guestGen  uint64
		want      clipboardDecision
	}{
		{
			name:   "host pasteboard update",
			cached: "old", cachedGen: 2, host: "copied on host", guest: "old", guestGen: 2,
			want: clipboardDecision{text: "copied on host", guestGeneration: 2, sendToGuest: true},
		},
		{
			name:   "guest clipboard update",
			cached: "old", cachedGen: 2, host: "old", guest: "copied in guest", guestGen: 3,
			want: clipboardDecision{text: "copied in guest", guestGeneration: 3, writeToHost: true},
		},
		{
			name:   "simultaneous updates keep pasteboard",
			cached: "old", cachedGen: 2, host: "new pasteboard", guest: "stale guest", guestGen: 3,
			want: clipboardDecision{text: "new pasteboard", guestGeneration: 3, sendToGuest: true},
		},
		{
			name:   "unchanged",
			cached: "same", cachedGen: 4, host: "same", guest: "same", guestGen: 4,
			want: clipboardDecision{text: "same", guestGeneration: 4},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := reconcileClipboard(test.cached, test.cachedGen, test.host, test.guest, test.guestGen)
			if got != test.want {
				t.Fatalf("clipboard decision = %+v, want %+v", got, test.want)
			}
		})
	}
}

type clipboardTestHost struct {
	text              string
	readErr, writeErr error
}

func (c *clipboardTestHost) ReadText() (string, error) { return c.text, c.readErr }
func (c *clipboardTestHost) SetText(text string) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	c.text = text
	return nil
}
func (*clipboardTestHost) Close() {}

type clipboardTestSession struct {
	resizeTestSession
	text       string
	generation uint64
	sent       []string
}

func (s *clipboardTestSession) GuestClipboard() (string, uint64) { return s.text, s.generation }
func (s *clipboardTestSession) SetClipboard(text string)         { s.sent = append(s.sent, text) }

func TestClipboardFailuresRetryWithoutLosingGuestUpdate(t *testing.T) {
	session := &clipboardTestSession{text: "guest text", generation: 1}
	host := &clipboardTestHost{text: "old", writeErr: fmt.Errorf("clipboard busy")}
	viewer := &displayViewer{session: session, hostClipboard: "old"}
	if err := viewer.syncClipboard(host); err == nil {
		t.Fatal("failed write acknowledged")
	}
	if viewer.guestClipboardGen != 0 || viewer.hostClipboard != "old" {
		t.Fatal("failed write consumed pending guest text")
	}
	host.writeErr = nil
	if err := viewer.syncClipboard(host); err != nil {
		t.Fatal(err)
	}
	if host.text != "guest text" || viewer.guestClipboardGen != 1 {
		t.Fatal("guest clipboard was not retried")
	}
	host.readErr = fmt.Errorf("clipboard busy")
	host.text = ""
	if err := viewer.syncClipboard(host); err == nil {
		t.Fatal("failed read accepted")
	}
	if len(session.sent) != 0 {
		t.Fatal("read failure erased guest clipboard")
	}
	host.readErr = nil
	host.text = "host text"
	if err := viewer.syncClipboard(host); err != nil {
		t.Fatal(err)
	}
	if len(session.sent) != 1 || session.sent[0] != "host text" {
		t.Fatal("host update was not delivered")
	}
}
