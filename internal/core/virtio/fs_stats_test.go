package virtio

import (
	"testing"
	"time"
)

func TestFSStatsConcurrentTimingSnapshots(t *testing.T) {
	const iterations = 10_000
	const opcode = fuseLookup
	const opDuration = 7 * time.Nanosecond
	const stageDuration = 11 * time.Nanosecond

	fs := &FS{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range iterations {
			fs.fuseRequests.Add(1)
			recordTimingStat(&fs.fuseOpStats[opcode].timingStat, opDuration)
			recordTimingStat(&fs.stageStats[fsStageQueueHarvest], stageDuration)
		}
	}()

	var previousOp, previousStage TimingStats
	for {
		stats := fs.Stats()
		if op, ok := findFUSEOpStats(stats.FUSEOps, opcode); ok {
			assertTimingStatsDoNotRegress(t, previousOp, TimingStats{
				Count:      op.Count,
				TotalNanos: op.TotalNanos,
				MaxNanos:   op.MaxNanos,
			})
			previousOp = TimingStats{Count: op.Count, TotalNanos: op.TotalNanos, MaxNanos: op.MaxNanos}
		}
		if stage, ok := findTimingStats(stats.Stages, fsStageName(fsStageQueueHarvest)); ok {
			assertTimingStatsDoNotRegress(t, previousStage, stage)
			previousStage = stage
		}

		select {
		case <-done:
			final := fs.Stats()
			op, ok := findFUSEOpStats(final.FUSEOps, opcode)
			if !ok {
				t.Fatal("FUSE operation statistics are missing")
			}
			if op.Count != iterations || op.TotalNanos != int64(iterations*opDuration) || op.MaxNanos != int64(opDuration) {
				t.Fatalf("FUSE operation statistics = %+v", op)
			}
			stage, ok := findTimingStats(final.Stages, fsStageName(fsStageQueueHarvest))
			if !ok {
				t.Fatal("stage statistics are missing")
			}
			if stage.Count != iterations || stage.TotalNanos != int64(iterations*stageDuration) || stage.MaxNanos != int64(stageDuration) {
				t.Fatalf("stage statistics = %+v", stage)
			}
			return
		default:
		}
	}
}

func TestFSStatsRemainReadableAfterGuestMemoryDetaches(t *testing.T) {
	fs := &FS{queues: make([]queue, 1)}
	fs.queues[0].ready = true
	fs.queues[0].size = 8
	fs.queues[0].descAddr = 0x1000
	fs.Attach(nil, nil)
	stats := fs.Stats()
	if len(stats.QueueAvailIdx) != 1 || stats.QueueAvailIdx[0] != 0 {
		t.Fatalf("detached queue availability = %v", stats.QueueAvailIdx)
	}
}

func assertTimingStatsDoNotRegress(t *testing.T, previous, current TimingStats) {
	t.Helper()
	if current.Count < previous.Count || current.TotalNanos < previous.TotalNanos || current.MaxNanos < previous.MaxNanos {
		t.Fatalf("timing statistics regressed from %+v to %+v", previous, current)
	}
}

func findFUSEOpStats(stats []FUSEOpStats, opcode uint32) (FUSEOpStats, bool) {
	for _, stat := range stats {
		if stat.Opcode == opcode {
			return stat, true
		}
	}
	return FUSEOpStats{}, false
}

func findTimingStats(stats []TimingStats, name string) (TimingStats, bool) {
	for _, stat := range stats {
		if stat.Name == name {
			return stat, true
		}
	}
	return TimingStats{}, false
}
