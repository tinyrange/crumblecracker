package vm

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type parallelGuest struct {
	id       string
	name     string
	image    string
	guestOS  string
	memoryMB uint64
	network  *client.NetworkConfig
}

type parallelOperationResult struct {
	id      string
	elapsed time.Duration
	err     error
}

func TestRuntimeParallelCapacity(t *testing.T) {
	if os.Getenv("CC_TEST_VM_PARALLEL") == "" {
		t.Skip("set CC_TEST_VM_PARALLEL=1 to run parallel VM capacity testing")
	}
	if testing.Short() {
		t.Skip("parallel VM capacity testing is not a short test")
	}
	env := newRuntimeBootEnv(t)
	count := soakPositiveInt(t, "CC_TEST_VM_PARALLEL_COUNT", 4)
	commands := soakPositiveInt(t, "CC_TEST_VM_PARALLEL_COMMANDS", 5)
	commandUser := strings.TrimSpace(os.Getenv("CC_TEST_VM_PARALLEL_USER"))
	guests := parallelGuestPlan(t, env, count, os.Getenv("CC_TEST_VM_PARALLEL_GUESTS"))
	manager := NewManagerWithBackend(env.backend)
	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs := soakOpenFDs()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = manager.ShutdownAll(ctx)
	})

	t.Logf("starting count=%d guests=%s commands_per_guest=%d user=%q telemetry=%s", count, parallelGuestSummary(guests), commands, commandUser, parallelTelemetry())
	startResults := parallelStartGuests(manager, guests)
	logParallelResults(t, "start", startResults)
	if err := parallelResultError(startResults); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		shutdownErr := manager.ShutdownAll(ctx)
		t.Fatalf("parallel start failed: %v; shutdown: %v; telemetry=%s", err, shutdownErr, parallelTelemetry())
	}

	for _, guest := range guests {
		if state := manager.StatusOf(guest.id); state.Status != "running" {
			t.Fatalf("%s state after parallel start = %+v", guest.id, state)
		}
	}
	t.Logf("all running telemetry=%s", parallelTelemetry())

	for round := 0; round < commands; round++ {
		results := parallelRunCommandRound(manager, guests, round, commandUser)
		logParallelResults(t, fmt.Sprintf("command_round_%d", round), results)
		if err := parallelResultError(results); err != nil {
			t.Fatalf("parallel command round %d failed: %v; telemetry=%s", round, err, parallelTelemetry())
		}
	}

	shutdownResults := parallelShutdownGuests(manager, guests)
	logParallelResults(t, "shutdown", shutdownResults)
	if err := parallelResultError(shutdownResults); err != nil {
		t.Fatalf("parallel shutdown failed: %v; telemetry=%s", err, parallelTelemetry())
	}
	for _, guest := range guests {
		if state := manager.StatusOf(guest.id); state.Status != "stopped" || state.ExitReason != "clean shutdown" {
			t.Fatalf("%s state after parallel shutdown = %+v", guest.id, state)
		}
	}
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Minute)
	if err := manager.ShutdownAll(closeCtx); err != nil {
		cancelClose()
		t.Fatalf("close manager after parallel shutdown: %v", err)
	}
	cancelClose()
	parallelWaitForTeardown(baselineGoroutines, baselineFDs, 2*time.Second)
	t.Logf("completed count=%d commands=%d telemetry=%s", count, count*commands, parallelTelemetry())
}

func parallelWaitForTeardown(baselineGoroutines, baselineFDs int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= baselineGoroutines && (baselineFDs < 0 || soakOpenFDs() <= baselineFDs) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func parallelGuestPlan(t *testing.T, env *runtimeBootEnv, count int, raw string) []parallelGuest {
	t.Helper()
	names := []string{"linux"}
	if fields := strings.Split(strings.ToLower(raw), ","); strings.TrimSpace(raw) != "" {
		names = names[:0]
		for _, field := range fields {
			if name := strings.TrimSpace(field); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("CC_TEST_VM_PARALLEL_GUESTS must select at least one guest")
	}
	guests := make([]parallelGuest, 0, count)
	for i := 0; i < count; i++ {
		name := names[i%len(names)]
		guest := parallelGuest{id: fmt.Sprintf("parallel-%s-%03d", name, i), name: name}
		switch name {
		case "linux":
			guest.image, guest.guestOS, guest.memoryMB = env.imageName, "Linux", env.memoryMB
			guest.network = &client.NetworkConfig{Enabled: false}
		case "netbsd":
			guest.image, guest.guestOS, guest.memoryMB = "@netbsd", "NetBSD", 1024
		case "freebsd":
			guest.image, guest.guestOS, guest.memoryMB = "@freebsd", "FreeBSD", 1024
		case "openbsd":
			guest.image, guest.guestOS, guest.memoryMB = "@openbsd", "OpenBSD", 768
		default:
			t.Fatalf("unknown parallel guest %q", name)
		}
		guests = append(guests, guest)
	}
	return guests
}

func parallelStartGuests(manager *Manager, guests []parallelGuest) []parallelOperationResult {
	results := make(chan parallelOperationResult, len(guests))
	var ready sync.WaitGroup
	ready.Add(len(guests))
	release := make(chan struct{})
	for _, guest := range guests {
		guest := guest
		go func() {
			ready.Done()
			<-release
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_, err := manager.StartStream(ctx, client.CreateInstanceRequest{
				ID:       guest.id,
				Image:    guest.image,
				MemoryMB: guest.memoryMB,
				CPUs:     1,
				Network:  guest.network,
			}, nil)
			results <- parallelOperationResult{id: guest.id, elapsed: time.Since(started), err: err}
		}()
	}
	ready.Wait()
	close(release)
	return collectParallelResults(results, len(guests))
}

func parallelRunCommandRound(manager *Manager, guests []parallelGuest, round int, user string) []parallelOperationResult {
	results := make(chan parallelOperationResult, len(guests))
	for _, guest := range guests {
		guest := guest
		go func() {
			marker := fmt.Sprintf("%s-round-%03d", guest.id, round)
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := manager.RunIn(ctx, guest.id, client.RunRequest{
				Command: []string{"sh", "-c", "test \"$(uname -s)\" = \"$1\"; printf '%s\\n' \"$2\"", "parallel", guest.guestOS, marker},
				User:    user,
			})
			if err == nil && (resp.ExitCode != 0 || strings.TrimSpace(resp.Output) != marker) {
				err = fmt.Errorf("exit=%d output=%q, want %q", resp.ExitCode, resp.Output, marker)
			}
			results <- parallelOperationResult{id: guest.id, elapsed: time.Since(started), err: err}
		}()
	}
	return collectParallelResults(results, len(guests))
}

func parallelShutdownGuests(manager *Manager, guests []parallelGuest) []parallelOperationResult {
	results := make(chan parallelOperationResult, len(guests))
	for _, guest := range guests {
		guest := guest
		go func() {
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			err := manager.ShutdownInstance(ctx, guest.id)
			results <- parallelOperationResult{id: guest.id, elapsed: time.Since(started), err: err}
		}()
	}
	return collectParallelResults(results, len(guests))
}

func collectParallelResults(results <-chan parallelOperationResult, count int) []parallelOperationResult {
	collected := make([]parallelOperationResult, 0, count)
	for len(collected) < count {
		collected = append(collected, <-results)
	}
	return collected
}

func parallelResultError(results []parallelOperationResult) error {
	var failures []string
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.id+": "+result.err.Error())
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(failures, "; "))
}

func logParallelResults(t *testing.T, operation string, results []parallelOperationResult) {
	t.Helper()
	var slowest time.Duration
	failures := 0
	for _, result := range results {
		if result.elapsed > slowest {
			slowest = result.elapsed
		}
		if result.err != nil {
			failures++
		}
	}
	t.Logf("%s completed=%d failures=%d slowest=%s telemetry=%s", operation, len(results), failures, slowest.Round(time.Millisecond), parallelTelemetry())
}

func parallelGuestSummary(guests []parallelGuest) string {
	counts := make(map[string]int)
	for _, guest := range guests {
		counts[guest.name]++
	}
	parts := make([]string, 0, 4)
	for _, name := range []string{"linux", "netbsd", "freebsd", "openbsd"} {
		if counts[name] != 0 {
			parts = append(parts, name+"="+strconv.Itoa(counts[name]))
		}
	}
	return strings.Join(parts, ",")
}

func parallelTelemetry() string {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return fmt.Sprintf("goroutines=%d heap=%dMiB rss=%s mem_available=%s swap_free=%s open_fds=%d",
		runtime.NumGoroutine(), mem.HeapAlloc>>20, procStatusValue("/proc/self/status", "VmRSS:"),
		procStatusValue("/proc/meminfo", "MemAvailable:"), procStatusValue("/proc/meminfo", "SwapFree:"), soakOpenFDs())
}

func procStatusValue(path, prefix string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.Join(strings.Fields(strings.TrimPrefix(line, prefix)), "")
		}
	}
	return "unknown"
}
