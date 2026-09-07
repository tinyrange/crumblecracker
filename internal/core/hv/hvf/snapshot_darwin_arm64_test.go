//go:build darwin && arm64

package hvf

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

func TestWriteSparseSnapshotMemoryPreservesDataAndHoles(t *testing.T) {
	pageSize := os.Getpagesize()
	memory, err := syscall.Mmap(-1, 0, 16<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap guest memory: %v", err)
	}
	defer syscall.Munmap(memory)
	copy(memory[pageSize:pageSize+4], "left")
	copy(memory[12<<20:(12<<20)+5], "right")

	path := filepath.Join(t.TempDir(), "memory.bin")
	if err := writeSparseSnapshotMemory(path, memory, 0o600); err != nil {
		t.Fatalf("write sparse snapshot memory: %v", err)
	}
	assertSparseSnapshotMemory(t, path, memory)
}

func TestWriteSparseSnapshotMemoryFallsBackWhenPageQueryFails(t *testing.T) {
	memory := make([]byte, 8<<20)
	copy(memory[2<<20:], "first")
	copy(memory[6<<20:], "second")
	queries := 0
	query := func(uintptr) (int32, error) {
		queries++
		return 0, fmt.Errorf("page query unavailable")
	}

	path := filepath.Join(t.TempDir(), "memory.bin")
	if err := writeSparseSnapshotMemoryWithQuery(path, memory, 0o600, query); err != nil {
		t.Fatalf("write fallback sparse snapshot memory: %v", err)
	}
	if queries != 1 {
		t.Fatalf("page queries = %d, want 1 before fallback", queries)
	}
	assertSparseSnapshotMemory(t, path, memory)
}

func assertSparseSnapshotMemory(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot memory: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored memory differs from captured memory")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot memory: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("snapshot stat has type %T", info.Sys())
	}
	if allocated := stat.Blocks * 512; allocated >= info.Size() {
		if !filesystemReportsSparseAllocation(t, filepath.Dir(path), info.Size()) {
			t.Logf("filesystem reports dense allocation for sparse probe; skipping physical allocation assertion")
			return
		}
		t.Fatalf("allocated snapshot bytes = %d, logical bytes = %d", allocated, info.Size())
	}
}

func filesystemReportsSparseAllocation(t *testing.T, dir string, size int64) bool {
	t.Helper()
	path := filepath.Join(dir, "sparse-allocation-probe.bin")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("create sparse allocation probe: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatalf("truncate sparse allocation probe: %v", err)
	}
	if _, err := file.WriteAt([]byte{1}, 0); err != nil {
		_ = file.Close()
		t.Fatalf("write sparse allocation probe: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close sparse allocation probe: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat sparse allocation probe: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("sparse allocation probe stat has type %T", info.Sys())
	}
	return stat.Blocks*512 < info.Size()
}

func BenchmarkWriteSparseSnapshotMemory(b *testing.B) {
	for _, size := range []int{512 << 20, 12 << 30} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			memory, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
			if err != nil {
				b.Fatalf("mmap guest memory: %v", err)
			}
			defer syscall.Munmap(memory)
			for offset := 0; offset < 64<<20; offset += os.Getpagesize() {
				memory[offset] = 1
			}
			path := filepath.Join(b.TempDir(), "memory.bin")
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				if err := writeSparseSnapshotMemory(path, memory, 0o600); err != nil {
					b.Fatalf("write sparse snapshot memory: %v", err)
				}
			}
			b.StopTimer()
			info, err := os.Stat(path)
			if err != nil {
				b.Fatalf("stat snapshot memory: %v", err)
			}
			stat := info.Sys().(*syscall.Stat_t)
			b.ReportMetric(float64(stat.Blocks*512)/(1<<20), "allocated-MiB")
		})
	}
}

func TestValidateSnapshotRequestRequiresMatchingMemory(t *testing.T) {
	manifest := snapshotManifest{
		MemorySize: arm64vm.MemorySizeBytes(4096),
		Devices: map[string]virtio.MMIOState{
			"balloon": {NumPages: balloonTargetPages(1024)},
		},
	}
	req := vmruntime.RunRequest{MemoryMB: 4096, BalloonMB: 1024}
	if err := validateSnapshotRequest(manifest, req); err != nil {
		t.Fatalf("validate matching snapshot: %v", err)
	}
	if err := validateSnapshotRequest(manifest, vmruntime.RunRequest{MemoryMB: 2048, BalloonMB: 1024}); err == nil {
		t.Fatalf("validate mismatched memory succeeded")
	}
	if err := validateSnapshotRequest(manifest, vmruntime.RunRequest{MemoryMB: 4096, BalloonMB: 512}); err != nil {
		t.Fatalf("validate snapshot with a new balloon target: %v", err)
	}
}

func TestRestoreDeviceStatesAppliesCurrentBalloonTarget(t *testing.T) {
	balloon := virtio.NewBalloon(arm64vm.BalloonBase, arm64vm.BalloonSize, arm64vm.BalloonIRQ)
	state := balloon.SnapshotState()
	state.Status = 4
	state.NumPages = balloonTargetPages(1024)

	want := balloonTargetPages(512)
	if err := restoreDeviceStates(map[string]virtio.MMIOState{"balloon": state}, nil, nil, balloon, want, nil, nil, nil); err != nil {
		t.Fatalf("restore balloon state: %v", err)
	}
	if got := balloon.SnapshotState().NumPages; got != want {
		t.Fatalf("restored balloon target pages = %d, want %d", got, want)
	}
}

func TestSnapshotTriggerWaitsForBalloonTarget(t *testing.T) {
	balloon := virtio.NewBalloon(arm64vm.BalloonBase, arm64vm.BalloonSize, arm64vm.BalloonIRQ)
	state := balloon.SnapshotState()
	state.Status = 4
	state.NumPages = balloonTargetPages(160)
	state.ActualPages = balloonTargetPages(128)
	if err := balloon.RestoreState(state); err != nil {
		t.Fatalf("restore balloon state: %v", err)
	}
	trigger := &snapshotTrigger{devices: snapshotDevices{balloon: balloon}}
	if got := trigger.readyValue(); got != 0 {
		t.Fatalf("snapshot trigger ready value = %#x before balloon target, want 0", got)
	}

	state.ActualPages = state.NumPages
	if err := balloon.RestoreState(state); err != nil {
		t.Fatalf("restore balloon target state: %v", err)
	}
	if got := trigger.readyValue(); got != snapshotTriggerMagic {
		t.Fatalf("snapshot trigger ready value = %#x at balloon target, want %#x", got, snapshotTriggerMagic)
	}
}
