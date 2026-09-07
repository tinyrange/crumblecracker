package mounts

import (
	"strings"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
)

type recordingSnapshotter struct {
	rootCalls int
	atPaths   []string
}

func (s *recordingSnapshotter) RootSnapshot() (imagefs.Directory, error) {
	s.rootCalls++
	return nil, nil
}

func (s *recordingSnapshotter) RootSnapshotAt(path string) (imagefs.Directory, error) {
	s.atPaths = append(s.atPaths, path)
	return nil, nil
}

func TestSnapshotHelpers(t *testing.T) {
	if _, err := RootSnapshot(nil, ""); err == nil {
		t.Fatalf("nil root snapshot error = %v", err)
	}
	if _, err := ImageSnapshot(nil, "tools", "/mnt/tools"); err == nil {
		t.Fatalf("nil image snapshot error = %v", err)
	}

	snapshotter := &recordingSnapshotter{}
	if _, err := RootSnapshot(snapshotter, ""); err != nil {
		t.Fatalf("root snapshot: %v", err)
	}
	if snapshotter.rootCalls != 1 {
		t.Fatalf("root snapshot calls = %d, want 1", snapshotter.rootCalls)
	}
	if _, err := RootSnapshot(snapshotter, "/rootfs"); err != nil {
		t.Fatalf("root snapshot at: %v", err)
	}
	if _, err := ImageSnapshot(snapshotter, "tools", "/mnt/tools"); err != nil {
		t.Fatalf("image snapshot: %v", err)
	}
	if got := strings.Join(snapshotter.atPaths, ","); got != "/rootfs,/mnt/tools" {
		t.Fatalf("snapshot paths = %q", got)
	}

	if _, err := ImageSnapshot(struct{}{}, "tools", "/mnt/tools"); err == nil {
		t.Fatalf("unsupported image snapshot error = %v", err)
	}
}

func TestSnapshotCapabilityHelpers(t *testing.T) {
	snapshotter := &recordingSnapshotter{}
	caps := managedguest.Capabilities{RootSnapshot: true, ImageSnapshot: true}
	if _, err := RootSnapshotWithCapabilities("TestOS", caps, snapshotter, ""); err != nil {
		t.Fatalf("root snapshot with capability: %v", err)
	}
	if _, err := ImageSnapshotWithCapabilities("TestOS", caps, snapshotter, "tools", "/mnt/tools"); err != nil {
		t.Fatalf("image snapshot with capability: %v", err)
	}

	if _, err := RootSnapshotWithCapabilities("TestOS", managedguest.Capabilities{}, snapshotter, ""); err == nil {
		t.Fatalf("root snapshot disabled error = %v", err)
	}
	if _, err := ImageSnapshotWithCapabilities("TestOS", managedguest.Capabilities{}, snapshotter, "tools", "/mnt/tools"); err == nil {
		t.Fatalf("image snapshot disabled error = %v", err)
	}
}
