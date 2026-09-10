//go:build windows

package desktopapp

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsNativeClipboardRoundTrip(t *testing.T) {
	// CI owns the whole desktop. Local opt-in avoids replacing a user's clipboard.
	if os.Getenv("GITHUB_ACTIONS") != "true" && os.Getenv("CCX3_TEST_WINDOWS_CLIPBOARD") != "1" {
		t.Skip("requires disposable Windows clipboard")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	clipboard, err := newHostClipboard()
	if err != nil {
		t.Fatal(err)
	}
	defer clipboard.Close()
	for _, text := range []string{"host → guest\r\nUnicode: 日本語 🧠", "", strings.Repeat("λ", (1<<20)+1)} {
		if err := clipboard.SetText(text); err != nil {
			t.Fatal(err)
		}
		got, err := clipboard.ReadText()
		if err != nil || got != text {
			t.Fatalf("clipboard round-trip: length %d want %d, err %v", len(got), len(text), err)
		}
	}
	held := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		other, err := newHostClipboard()
		if err != nil {
			held <- err
			return
		}
		defer other.Close()
		if err := other.(*platformClipboard).open(); err != nil {
			held <- err
			return
		}
		defer clipboardClose.Call()
		held <- nil
		<-release
	}()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if _, err := clipboard.ReadText(); err == nil {
		t.Error("clipboard contention reported as empty text")
	}
	if err := clipboard.SetText("retry me"); err == nil {
		t.Error("contended clipboard write reported success")
	}
	close(release)
	<-done
	if err := clipboard.SetText("retry me"); err != nil {
		t.Fatal(err)
	}
	if text, err := clipboard.ReadText(); err != nil || text != "retry me" {
		t.Fatalf("clipboard retry %q %v", text, err)
	}
}
