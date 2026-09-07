package vm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type snapshotScaleInstance struct {
	id      string
	inst    Instance
	elapsed time.Duration
	err     error
}

type snapshotScaleProbeResult struct {
	elapsed time.Duration
	err     error
}

type snapshotScaleTelemetry struct {
	rssKB           int64
	pssKB           int64
	privateKB       int64
	sharedKB        int64
	availableKB     int64
	swapFreeKB      int64
	pageTablesKB    int64
	secPageTablesKB int64
	kernelStackKB   int64
	slabKB          int64
	threads         int64
	openFDs         int
	goRoutines      int
	heapAllocMiB    uint64
}

type snapshotScaleMappingUsage struct {
	mappings  int
	rssKB     int64
	pssKB     int64
	privateKB int64
}

func TestRuntimeSnapshotCloneScale(t *testing.T) {
	if os.Getenv("CC_TEST_VM_SNAPSHOT_SCALE") == "" {
		t.Skip("set CC_TEST_VM_SNAPSHOT_SCALE=1 to run snapshot clone scaling")
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("KVM startup snapshots are implemented on Linux amd64 and arm64")
	}
	if testing.Short() {
		t.Skip("snapshot clone scaling is not a short test")
	}

	target := soakPositiveInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_COUNT", 250)
	batchSize := soakPositiveInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_BATCH", 25)
	finalCommandRounds := soakPositiveInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_FINAL_COMMANDS", 1)
	finalCommandBatch := soakPositiveInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_FINAL_COMMAND_BATCH", min(500, target))
	finalCommandSettle := snapshotScaleDuration(t, "CC_TEST_VM_SNAPSHOT_SCALE_FINAL_COMMAND_SETTLE", 0)
	memoryMB := uint64(soakPositiveInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_MEMORY_MB", 256))
	balloonMB := uint64(snapshotScaleNonNegativeInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_BALLOON_MB", defaultSnapshotScaleBalloonMB(memoryMB)))
	if balloonMB > memoryMB {
		t.Fatalf("balloon memory %dMiB exceeds guest memory %dMiB", balloonMB, memoryMB)
	}
	minAvailableMB := int64(soakPositiveInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_MIN_AVAILABLE_MB", 4096))
	commandReserveMB := int64(soakPositiveInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_COMMAND_RESERVE_MB", 4096))
	settleEvery := snapshotScaleNonNegativeInt(t, "CC_TEST_VM_SNAPSHOT_SCALE_SETTLE_EVERY", 0)
	checkpointSettle := snapshotScaleDuration(t, "CC_TEST_VM_SNAPSHOT_SCALE_CHECKPOINT_SETTLE", 0)
	env := newRuntimeBootEnv(t)
	snapshotRoot := t.TempDir()

	capture := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID:          "snapshot-scale-capture",
		SnapshotDir: snapshotRoot,
		MemoryMB:    memoryMB,
		BalloonMB:   balloonMB,
		CPUs:        1,
		Network:     &client.NetworkConfig{Enabled: false},
	})
	captureConsole := runtimeConsoleHistory(capture)
	if err := capture.Close(); err != nil {
		t.Fatalf("close snapshot capture instance: %v", err)
	}
	snapshotPath := singleSnapshotPath(t, snapshotRoot, captureConsole)
	runtime.GC()
	baseline := readSnapshotScaleTelemetry()
	t.Logf("snapshot=%s memory=%dMiB balloon=%dMiB target=%d batch=%d baseline=%s", snapshotPath, memoryMB, balloonMB, target, batchSize, baseline)

	instances := make([]snapshotScaleInstance, 0, target)
	t.Cleanup(func() {
		closeSnapshotScaleInstances(instances)
	})
	for len(instances) < target {
		if availableMB := readProcKB("/proc/meminfo", "MemAvailable:") >> 10; availableMB < minAvailableMB {
			t.Logf("safety stop before start: live=%d available=%dMiB threshold=%dMiB", len(instances), availableMB, minAvailableMB)
			break
		}
		count := batchSize
		if remaining := target - len(instances); count > remaining {
			count = remaining
		}
		started := startSnapshotScaleBatch(env, snapshotPath, memoryMB, balloonMB, len(instances), count)
		for _, result := range started {
			if result.err != nil {
				closeSnapshotScaleInstances(started)
				t.Fatalf("snapshot restore %s failed after %s: %v; live=%d telemetry=%s", result.id, result.elapsed, result.err, len(instances), readSnapshotScaleTelemetry())
			}
		}
		instances = append(instances, started...)
		probeSnapshotScaleBatch(t, started, 0)
		runtime.GC()
		telemetry := readSnapshotScaleTelemetry()
		p50, p95, slowest := snapshotScaleStartLatency(started)
		t.Logf("checkpoint live=%d start_latency={p50=%s p95=%s max=%s} telemetry=%s delta_per_vm={pss=%.1fMiB private=%.1fMiB host_available=%.1fMiB}",
			len(instances), p50.Round(time.Millisecond), p95.Round(time.Millisecond), slowest.Round(time.Millisecond), telemetry,
			telemetry.deltaMiB(baseline.pssKB, telemetry.pssKB, len(instances)),
			telemetry.deltaMiB(baseline.privateKB, telemetry.privateKB, len(instances)),
			telemetry.deltaMiB(telemetry.availableKB, baseline.availableKB, len(instances)))
		if telemetry.availableKB>>10 < minAvailableMB {
			t.Logf("safety stop after checkpoint: live=%d available=%dMiB threshold=%dMiB", len(instances), telemetry.availableKB>>10, minAvailableMB)
			break
		}
		if checkpointSettle > 0 && settleEvery > 0 && len(instances) < target && len(instances)%settleEvery == 0 {
			settleSnapshotScaleFor(t, checkpointSettle, "checkpoint")
		}
	}
	if len(instances) != target {
		t.Fatalf("snapshot scale stopped safely at %d VMs before target %d; telemetry=%s", len(instances), target, readSnapshotScaleTelemetry())
	}

	t.Logf("peak live=%d target=%d telemetry=%s", len(instances), target, readSnapshotScaleTelemetry())
	mappingUsage := readSnapshotScaleMappingUsage(filepath.Join(snapshotPath, "memory.bin"))
	t.Logf("peak_mapping_usage snapshot={%s} anonymous={%s} other={%s}", mappingUsage["snapshot"], mappingUsage["anonymous"], mappingUsage["other"])
	settleSnapshotScaleMemory(t)
	if profilePath := strings.TrimSpace(os.Getenv("CC_TEST_VM_SNAPSHOT_SCALE_HEAP_PROFILE")); profilePath != "" {
		writeSnapshotScaleHeapProfile(t, profilePath)
	}
	for round := 0; round < finalCommandRounds; round++ {
		started := time.Now()
		latencies := probeSnapshotScaleFleet(t, instances, round+1, finalCommandBatch, commandReserveMB, finalCommandSettle)
		p50, p95, slowest := snapshotScaleLatencies(latencies)
		t.Logf("full_command_round=%d live=%d wall=%s latency={p50=%s p95=%s max=%s} telemetry=%s", round, len(instances),
			time.Since(started).Round(time.Millisecond), p50.Round(time.Millisecond), p95.Round(time.Millisecond), slowest.Round(time.Millisecond), readSnapshotScaleTelemetry())
	}
	closeSnapshotScaleInstances(instances)
	instances = nil
	parallelWaitForTeardown(baseline.goRoutines, baseline.openFDs, 5*time.Second)
	runtime.GC()
	teardown := readSnapshotScaleTelemetry()
	t.Logf("teardown telemetry=%s", teardown)
	if teardown.threads > baseline.threads+64 {
		t.Fatalf("VM teardown retained %d OS threads above baseline; want at most 64", teardown.threads-baseline.threads)
	}
}

func probeSnapshotScaleFleet(t *testing.T, instances []snapshotScaleInstance, round, batchSize int, reserveMB int64, settle time.Duration) []time.Duration {
	t.Helper()
	latencies := make([]time.Duration, 0, len(instances))
	for start := 0; start < len(instances); start += batchSize {
		telemetry := readSnapshotScaleTelemetry()
		if telemetry.availableKB>>10 < reserveMB {
			t.Fatalf("refusing command round %d at VM %d with %dMiB available below %dMiB reserve; telemetry=%s", round, start, telemetry.availableKB>>10, reserveMB, telemetry)
		}
		end := min(start+batchSize, len(instances))
		latencies = append(latencies, probeSnapshotScaleBatch(t, instances[start:end], round)...)
		if settle > 0 && end < len(instances) {
			settleSnapshotScaleFor(t, settle, fmt.Sprintf("command round=%d completed=%d", round, end))
		}
	}
	return latencies
}

// Large snapshot fleets need enough usable guest memory for the init and
// command agents, while reclaiming the rest before cloning. Sixty-four MiB was
// observed to invoke the guest OOM killer under concurrent commands; eighty
// MiB is the smallest configuration that survived the behavior-level probes.
func defaultSnapshotScaleBalloonMB(memoryMB uint64) int {
	const guestWorkingSetMB = 80
	if memoryMB <= guestWorkingSetMB {
		return 0
	}
	return int(memoryMB - guestWorkingSetMB)
}

func settleSnapshotScaleMemory(t *testing.T) {
	t.Helper()
	duration := snapshotScaleDuration(t, "CC_TEST_VM_SNAPSHOT_SCALE_SETTLE", 0)
	if duration == 0 {
		return
	}
	settleSnapshotScaleFor(t, duration, "settle")
}

func settleSnapshotScaleFor(t *testing.T, duration time.Duration, label string) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		wait := min(5*time.Second, remaining)
		timer := time.NewTimer(wait)
		<-timer.C
		runtime.GC()
		t.Logf("%s remaining=%s telemetry=%s ksm=%s", label, max(time.Duration(0), time.Until(deadline)).Round(time.Second), readSnapshotScaleTelemetry(), readSnapshotScaleKSM())
	}
}

func readSnapshotScaleKSM() string {
	values := make([]string, 0, 6)
	for _, name := range []string{"run", "full_scans", "pages_shared", "pages_sharing", "pages_unshared", "pages_volatile"} {
		value, err := os.ReadFile(filepath.Join("/sys/kernel/mm/ksm", name))
		if err != nil {
			continue
		}
		values = append(values, name+"="+strings.TrimSpace(string(value)))
	}
	if value := readProcTextField("/proc/self/ksm_stat", "ksm_merge_any:"); value != "" {
		values = append(values, "process_merge_any="+value)
	}
	return strings.Join(values, " ")
}

func readProcTextField(path, field string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, field) {
			return strings.TrimSpace(strings.TrimPrefix(line, field))
		}
	}
	return ""
}

func snapshotScaleNonNegativeInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		t.Fatalf("%s=%q is not a non-negative integer", name, value)
	}
	return parsed
}

func snapshotScaleDuration(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		t.Fatalf("%s=%q is not a non-negative duration", name, value)
	}
	return parsed
}

func startSnapshotScaleBatch(env *runtimeBootEnv, snapshotPath string, memoryMB, balloonMB uint64, offset, count int) []snapshotScaleInstance {
	results := make(chan snapshotScaleInstance, count)
	var ready sync.WaitGroup
	ready.Add(count)
	release := make(chan struct{})
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("snapshot-scale-%05d", offset+i)
		go func() {
			ready.Done()
			<-release
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			inst, err := env.backend.StartStream(ctx, client.CreateInstanceRequest{
				ID:              id,
				Image:           env.imageName,
				RestoreSnapshot: snapshotPath,
				MemoryMB:        memoryMB,
				BalloonMB:       balloonMB,
				CPUs:            1,
				Network:         &client.NetworkConfig{Enabled: false},
			}, nil)
			results <- snapshotScaleInstance{id: id, inst: inst, elapsed: time.Since(started), err: err}
		}()
	}
	ready.Wait()
	close(release)
	started := make([]snapshotScaleInstance, 0, count)
	for len(started) < count {
		started = append(started, <-results)
	}
	return started
}

func probeSnapshotScaleBatch(t *testing.T, instances []snapshotScaleInstance, round int) []time.Duration {
	t.Helper()
	results := make(chan snapshotScaleProbeResult, len(instances))
	for _, instance := range instances {
		instance := instance
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			started := time.Now()
			marker := fmt.Sprintf("ready-%s-%03d", instance.id, round)
			resp, err := instance.inst.Exec(ctx, client.ExecRequest{
				Command: []string{"sh", "-lc", "test \"$(uname -s)\" = Linux; printf '%s\\n' \"$1\"", "snapshot-scale", marker},
			})
			if err == nil && (resp.ExitCode != 0 || strings.TrimSpace(resp.Output) != marker) {
				err = fmt.Errorf("exit=%d output=%q, want %q", resp.ExitCode, resp.Output, marker)
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", instance.id, err)
			}
			results <- snapshotScaleProbeResult{elapsed: time.Since(started), err: err}
		}()
	}
	latencies := make([]time.Duration, 0, len(instances))
	for range instances {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		latencies = append(latencies, result.elapsed)
	}
	return latencies
}

func closeSnapshotScaleInstances(instances []snapshotScaleInstance) {
	const closeConcurrency = 32
	sem := make(chan struct{}, closeConcurrency)
	var wg sync.WaitGroup
	for _, instance := range instances {
		if instance.inst == nil {
			continue
		}
		wg.Add(1)
		go func(inst Instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = inst.Close()
		}(instance.inst)
	}
	wg.Wait()
}

func snapshotScaleStartLatency(instances []snapshotScaleInstance) (time.Duration, time.Duration, time.Duration) {
	latencies := make([]time.Duration, 0, len(instances))
	for _, instance := range instances {
		latencies = append(latencies, instance.elapsed)
	}
	return snapshotScaleLatencies(latencies)
}

func snapshotScaleLatencies(latencies []time.Duration) (time.Duration, time.Duration, time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencies[len(latencies)/2], latencies[(len(latencies)-1)*95/100], latencies[len(latencies)-1]
}

func readSnapshotScaleTelemetry() snapshotScaleTelemetry {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	smaps := readProcValues("/proc/self/smaps_rollup", "Rss:", "Pss:", "Private_Dirty:", "Shared_Clean:")
	host := readProcValues("/proc/meminfo", "MemAvailable:", "SwapFree:", "PageTables:", "SecPageTables:", "KernelStack:", "Slab:")
	status := readProcValues("/proc/self/status", "Threads:")
	return snapshotScaleTelemetry{
		rssKB:           smaps["Rss:"],
		pssKB:           smaps["Pss:"],
		privateKB:       smaps["Private_Dirty:"],
		sharedKB:        smaps["Shared_Clean:"],
		availableKB:     host["MemAvailable:"],
		swapFreeKB:      host["SwapFree:"],
		pageTablesKB:    host["PageTables:"],
		secPageTablesKB: host["SecPageTables:"],
		kernelStackKB:   host["KernelStack:"],
		slabKB:          host["Slab:"],
		threads:         status["Threads:"],
		openFDs:         soakOpenFDs(),
		goRoutines:      runtime.NumGoroutine(),
		heapAllocMiB:    mem.HeapAlloc >> 20,
	}
}

func readProcKB(path, prefix string) int64 {
	return readProcValues(path, prefix)[prefix]
}

func readProcValues(path string, prefixes ...string) map[string]int64 {
	values := make(map[string]int64, len(prefixes))
	for _, prefix := range prefixes {
		values[prefix] = -1
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, prefix := range prefixes {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			fields := strings.Fields(strings.TrimPrefix(line, prefix))
			if len(fields) == 0 {
				break
			}
			value, err := strconv.ParseInt(fields[0], 10, 64)
			if err == nil {
				values[prefix] = value
			}
			break
		}
	}
	return values
}

func readSnapshotScaleMappingUsage(snapshotMemoryPath string) map[string]snapshotScaleMappingUsage {
	usage := map[string]snapshotScaleMappingUsage{
		"snapshot":  {},
		"anonymous": {},
		"other":     {},
	}
	file, err := os.Open("/proc/self/smaps")
	if err != nil {
		return usage
	}
	defer file.Close()
	category := "other"
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if fields := strings.Fields(line); len(fields) >= 5 && strings.Contains(fields[0], "-") && len(fields[1]) == 4 {
			path := ""
			if len(fields) >= 6 {
				path = fields[5]
			}
			switch {
			case path == snapshotMemoryPath:
				category = "snapshot"
			case path == "" || strings.HasPrefix(path, "["):
				category = "anonymous"
			default:
				category = "other"
			}
			value := usage[category]
			value.mappings++
			usage[category] = value
			continue
		}
		for _, prefix := range []string{"Rss:", "Pss:", "Private_Dirty:"} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			fields := strings.Fields(strings.TrimPrefix(line, prefix))
			if len(fields) == 0 {
				break
			}
			parsed, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				break
			}
			value := usage[category]
			switch prefix {
			case "Rss:":
				value.rssKB += parsed
			case "Pss:":
				value.pssKB += parsed
			default:
				value.privateKB += parsed
			}
			usage[category] = value
			break
		}
	}
	return usage
}

func writeSnapshotScaleHeapProfile(t *testing.T, path string) {
	t.Helper()
	runtime.GC()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create heap profile: %v", err)
	}
	if err := pprof.WriteHeapProfile(file); err != nil {
		_ = file.Close()
		t.Fatalf("write heap profile: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close heap profile: %v", err)
	}
	t.Logf("heap_profile=%s", path)
}

func (u snapshotScaleMappingUsage) String() string {
	return fmt.Sprintf("mappings=%d rss=%.1fMiB pss=%.1fMiB private_dirty=%.1fMiB", u.mappings,
		float64(u.rssKB)/1024, float64(u.pssKB)/1024, float64(u.privateKB)/1024)
}

func (s snapshotScaleTelemetry) deltaMiB(baseline, current int64, count int) float64 {
	if baseline < 0 || current < 0 || count == 0 {
		return -1
	}
	return float64(current-baseline) / 1024 / float64(count)
}

func (s snapshotScaleTelemetry) String() string {
	return fmt.Sprintf("rss=%.1fMiB pss=%.1fMiB private_dirty=%.1fMiB shared_clean=%.1fMiB heap=%dMiB host_available=%.1fMiB swap_free=%.1fMiB page_tables=%.1fMiB sec_page_tables=%.1fMiB kernel_stack=%.1fMiB slab=%.1fMiB threads=%d goroutines=%d open_fds=%d",
		float64(s.rssKB)/1024, float64(s.pssKB)/1024, float64(s.privateKB)/1024, float64(s.sharedKB)/1024,
		s.heapAllocMiB, float64(s.availableKB)/1024, float64(s.swapFreeKB)/1024,
		float64(s.pageTablesKB)/1024, float64(s.secPageTablesKB)/1024, float64(s.kernelStackKB)/1024, float64(s.slabKB)/1024,
		s.threads, s.goRoutines, s.openFDs)
}
