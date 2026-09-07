//go:build linux

package guestagent

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestUsageFromProcessStateWithoutState(t *testing.T) {
	usage := UsageFromProcessState(nil, 1500*time.Millisecond)
	if usage == nil {
		t.Fatalf("UsageFromProcessState returned nil")
	}
	if usage.WallSeconds != 1.5 {
		t.Fatalf("wall seconds = %v, want 1.5", usage.WallSeconds)
	}
	if usage.CPUSeconds != 0 || usage.MaxRSSBytes != 0 {
		t.Fatalf("empty process state usage = %+v", usage)
	}
}

func TestEncodeExecUsage(t *testing.T) {
	got := EncodeExecUsage(&ExecUsage{WallSeconds: 1.5, CPUSeconds: 0.25})
	const want = "eyJ3YWxsX3NlY29uZHMiOjEuNSwiY3B1X3NlY29uZHMiOjAuMjV9"
	if got != want {
		t.Fatalf("EncodeExecUsage = %q, want %q", got, want)
	}
}

func TestProcessExitCodeUsesSignalStatus(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if err == nil {
		t.Fatalf("signaled command unexpectedly succeeded")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("command error = %T %v, want ExitError", err, err)
	}
	got := ProcessExitCode(cmd.ProcessState, exitErr.ExitCode())
	want := 128 + int(syscall.SIGTERM)
	if got != want {
		t.Fatalf("ProcessExitCode = %d, want %d", got, want)
	}
}
