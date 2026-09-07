//go:build linux && arm64

package vm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const arm64StartupBenchmarkEnv = "CC_KVM_ARM64_STARTUP_BENCHMARK"

type arm64StartupSample struct {
	ReadyNanos        int64                    `json:"ready_nanos"`
	FirstCommandNanos int64                    `json:"first_command_nanos"`
	Phases            map[string]time.Duration `json:"phases"`
}

type arm64StartupReport struct {
	GOARCH                  string               `json:"goarch"`
	Mode                    string               `json:"mode"`
	MemoryMB                uint64               `json:"memory_mb"`
	Samples                 []arm64StartupSample `json:"samples"`
	MedianReadyNanos        int64                `json:"median_ready_nanos"`
	MedianFirstCommandNanos int64                `json:"median_first_command_nanos"`
}

func TestKVMARM64StartupBenchmark(t *testing.T) {
	if os.Getenv(arm64StartupBenchmarkEnv) != "1" {
		t.Skipf("set %s=1 on a Linux/arm64 KVM host", arm64StartupBenchmarkEnv)
	}
	if err := Supports(); err != nil {
		t.Fatalf("KVM unavailable: %v", err)
	}

	prepareCtx, prepareCancel := context.WithTimeout(context.Background(), runtimePrepareTimeout())
	defer prepareCancel()
	cacheRoot := runtimeBootCacheRoot(t)
	kernelManager := alpine.NewManager(filepath.Join(cacheRoot, "kernel"))
	if err := kernelManager.EnsureWithProgress(prepareCtx, nil); err != nil {
		t.Fatalf("prepare kernel: %v", err)
	}
	backend := NewRuntimeBackend(kernelManager, oci.NewStore(filepath.Join(t.TempDir(), "images")), filepath.Join(cacheRoot, "guestinit"))
	snapshotRoot := t.TempDir()
	captureCtx, captureCancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	capture, err := backend.StartBlankStream(captureCtx, client.StartInstanceRequest{MemoryMB: 768, CPUs: 1, SnapshotDir: snapshotRoot}, nil)
	if err != nil {
		captureCancel()
		t.Fatalf("capture startup snapshot: %v", err)
	}
	if err := capture.Close(); err != nil {
		captureCancel()
		t.Fatalf("close snapshot capture: %v", err)
	}
	captureCancel()
	snapshotPath := singleSnapshotPath(t, snapshotRoot, "")

	const samples = 5
	report := arm64StartupReport{GOARCH: "arm64", Mode: "snapshot_restore", MemoryMB: 768, Samples: make([]arm64StartupSample, 0, samples)}
	readyValues := make([]int64, 0, samples)
	commandValues := make([]int64, 0, samples)
	for i := 0; i < samples; i++ {
		recorder := timing.NewRecorder()
		ctx, cancel := context.WithTimeout(timing.WithRecorder(context.Background(), recorder), runtimeBootTimeout())
		readyStart := time.Now()
		inst, err := backend.StartBlankStream(ctx, client.StartInstanceRequest{MemoryMB: report.MemoryMB, CPUs: 1, RestoreSnapshot: snapshotPath}, nil)
		readyDuration := time.Since(readyStart)
		if err != nil {
			cancel()
			t.Fatalf("sample %d boot: %v", i, err)
		}
		commandStart := time.Now()
		linuxInst, ok := inst.(*linuxInstance)
		if !ok || linuxInst.managedInstance == nil || linuxInst.managedInstance.session == nil {
			_ = inst.Close()
			cancel()
			t.Fatalf("sample %d did not return a managed Linux session", i)
		}
		commandErr := linuxInst.managedInstance.session.Flush(ctx)
		commandDuration := time.Since(commandStart)
		closeErr := inst.Close()
		cancel()
		if commandErr != nil {
			t.Fatalf("sample %d first command: %v", i, commandErr)
		}
		if closeErr != nil {
			t.Fatalf("sample %d close: %v", i, closeErr)
		}
		phases := make(map[string]time.Duration)
		for _, snapshot := range recorder.Snapshots() {
			phases[snapshot.Name] = snapshot.Duration
		}
		report.Samples = append(report.Samples, arm64StartupSample{ReadyNanos: readyDuration.Nanoseconds(), FirstCommandNanos: commandDuration.Nanoseconds(), Phases: phases})
		readyValues = append(readyValues, readyDuration.Nanoseconds())
		commandValues = append(commandValues, commandDuration.Nanoseconds())
	}
	sort.Slice(readyValues, func(i, j int) bool { return readyValues[i] < readyValues[j] })
	sort.Slice(commandValues, func(i, j int) bool { return commandValues[i] < commandValues[j] })
	report.MedianReadyNanos = readyValues[len(readyValues)/2]
	report.MedianFirstCommandNanos = commandValues[len(commandValues)/2]
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
}
