package vmruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSnapshotMemoryPath(t *testing.T) {
	snapshotDir := t.TempDir()
	manifestPath := filepath.Join(snapshotDir, "manifest.json")
	memoryPath := filepath.Join(snapshotDir, "memory.bin")
	if err := os.WriteFile(memoryPath, []byte("memory"), 0o600); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	resolved, err := ResolveSnapshotMemoryPath(manifestPath, "memory.bin")
	if err != nil {
		t.Fatalf("resolve memory: %v", err)
	}
	want, err := filepath.EvalSymlinks(memoryPath)
	if err != nil {
		t.Fatalf("resolve expected memory: %v", err)
	}
	if resolved != want {
		t.Fatalf("resolved path = %q, want %q", resolved, want)
	}
}

func TestResolveSnapshotMemoryPathRejectsUnsafePaths(t *testing.T) {
	parent := t.TempDir()
	snapshotDir := filepath.Join(parent, "snapshot")
	if err := os.Mkdir(snapshotDir, 0o700); err != nil {
		t.Fatalf("make snapshot dir: %v", err)
	}
	manifestPath := filepath.Join(snapshotDir, "manifest.json")
	outside := filepath.Join(parent, "outside.bin")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(snapshotDir, "linked-memory.bin")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("make escaping symlink: %v", err)
	}

	for _, memoryFile := range []string{
		outside,
		"../outside.bin",
		"sub/../memory.bin",
		"linked-memory.bin",
	} {
		t.Run(memoryFile, func(t *testing.T) {
			_, err := ResolveSnapshotMemoryPath(manifestPath, memoryFile)
			var pathErr *SnapshotMemoryPathError
			if !errors.As(err, &pathErr) {
				t.Fatalf("path error = %v, want SnapshotMemoryPathError", err)
			}
			if pathErr.Path != memoryFile {
				t.Fatalf("rejected path = %q, want %q", pathErr.Path, memoryFile)
			}
		})
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatalf("outside file = %q, %v", got, err)
	}
}
