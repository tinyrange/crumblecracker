package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	intcvmfs "github.com/tinyrange/crumblecracker/internal/core/cvmfs"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestCVMFSMonitorReportsLogicalActiveTransfers(t *testing.T) {
	monitor := &cvmfsMonitor{active: make(map[uint64]client.CVMFSTransferState)}
	started := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	monitor.Record(intcvmfs.TransferEvent{
		Time: started, ID: 7, State: "progress", Repo: "neurodesk.ardc.edu.au",
		Path: "/containers/tool.sif", Mirror: "http://mirror.example/cvmfs", Bytes: 25, TotalBytes: 100,
	})
	status := monitor.Status()
	if status.State != "downloading" || status.Progress != 0.25 || len(status.ActiveTransfers) != 1 {
		t.Fatalf("status = %+v", status)
	}
	transfer := status.ActiveTransfers[0]
	if transfer.Path != "/cvmfs/neurodesk.ardc.edu.au/containers/tool.sif" || transfer.Mirror != "http://mirror.example/cvmfs" {
		t.Fatalf("logical transfer = %+v", transfer)
	}
	monitor.Record(intcvmfs.TransferEvent{Time: started.Add(time.Second), ID: 7, State: "completed", Mirror: transfer.Mirror})
	if status := monitor.Status(); status.State != "idle" || len(status.ActiveTransfers) != 0 {
		t.Fatalf("completed status = %+v", status)
	}
}

func TestCVMFSCacheLimitEvictsOldestDownloadData(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "objects", "old")
	newPath := filepath.Join(root, "files", "new")
	statePath := filepath.Join(root, "state", "repo", "manifest")
	for _, path := range []string{oldPath, newPath, statePath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 64), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	monitor := &cvmfsMonitor{cacheRoot: root, cacheLimit: 128, active: make(map[uint64]client.CVMFSTransferState)}
	monitor.enforceCacheLimit()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old cache object was not evicted: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new cache file was evicted: %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("repository state was evicted: %v", err)
	}
}
