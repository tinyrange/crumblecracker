package snapshotstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureIsVisibleOnlyAfterCompletePublication(t *testing.T) {
	root := t.TempDir()
	capture, err := Begin(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = capture.Abort() })

	if matches, err := filepath.Glob(filepath.Join(root, "snapshot-*")); err != nil || len(matches) != 0 {
		t.Fatalf("incomplete capture discovery = %v, err = %v", matches, err)
	}
	if err := os.WriteFile(filepath.Join(capture.Dir(), "memory.bin"), []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Publish("memory.bin", "manifest.json"); err == nil {
		t.Fatal("published capture without required manifest")
	}
	if matches, err := filepath.Glob(filepath.Join(root, "snapshot-*")); err != nil || len(matches) != 0 {
		t.Fatalf("failed capture discovery = %v, err = %v", matches, err)
	}
	if err := os.WriteFile(filepath.Join(capture.Dir(), "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	final, err := capture.Publish("memory.bin", "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if matches, err := filepath.Glob(filepath.Join(root, "snapshot-*")); err != nil || len(matches) != 1 || matches[0] != final {
		t.Fatalf("published capture discovery = %v, err = %v", matches, err)
	}
}

func TestBeginRemovesOnlyStaleStagingDirectories(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, stagingPrefix+"stale")
	active := filepath.Join(root, stagingPrefix+"active")
	for _, path := range []string{stale, active} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-staleAfter - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	capture, err := Begin(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = capture.Abort() })
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging stat error = %v", err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active staging was removed: %v", err)
	}
}
