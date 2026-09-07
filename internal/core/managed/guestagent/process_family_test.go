//go:build linux

package guestagent

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProcessFamilyFollowsSetsidDescendant(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	cmd := exec.Command("/bin/sh", "-c", "setsid sleep 30 & child=$!; echo $child >"+shellQuoteForTest(pidFile)+"; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	family := NewProcessFamily(cmd.Process.Pid)
	defer family.Close()
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	var childPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("setsid descendant did not start")
	}
	// Give the watcher an opportunity to observe the child while its ancestry
	// is intact, then verify an exact-PID signal reaches it outside the group.
	time.Sleep(20 * time.Millisecond)
	if err := family.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("setsid descendant %d survived command-family cancellation", childPID)
}

type kernelTrackedFamily struct{ snapshots atomic.Int64 }

func (t *kernelTrackedFamily) Snapshot() map[int]struct{} {
	t.snapshots.Add(1)
	return nil
}
func (*kernelTrackedFamily) Close()                   {}
func (*kernelTrackedFamily) kernelTracksDescendants() {}

func TestKernelTrackedProcessFamilyDoesNotPollPerCommand(t *testing.T) {
	tracker := &kernelTrackedFamily{}
	family := newProcessFamily(os.Getpid()+100000, tracker)
	defer family.Close()
	time.Sleep(5 * processFamilyPollInterval)
	if got := tracker.snapshots.Load(); got != 0 {
		t.Fatalf("idle kernel-tracked command performed %d process scans", got)
	}
	_ = family.Signal(syscall.SIGTERM)
	if got := tracker.snapshots.Load(); got != 1 {
		t.Fatalf("cancellation performed %d process scans, want one", got)
	}
}

func TestNativeTrackerWithoutKernelDescendantCapabilityIsPolled(t *testing.T) {
	tracker := &kernelTrackedFamily{}
	family := newProcessFamily(os.Getpid()+100000, &pollRequiredFamily{tracker: tracker})
	defer family.Close()
	deadline := time.Now().Add(100 * time.Millisecond)
	for tracker.snapshots.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := tracker.snapshots.Load(); got == 0 {
		t.Fatal("platform tracker was never refreshed; a tracker-owned command start gate could remain closed")
	}
}

type pollRequiredFamily struct{ tracker *kernelTrackedFamily }

// Deliberately hide kernelTracksDescendants while preserving the tracker
// methods, matching the BSD native tracker contract.
func (t *pollRequiredFamily) Snapshot() map[int]struct{} { return t.tracker.Snapshot() }
func (t *pollRequiredFamily) Close()                     { t.tracker.Close() }

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
