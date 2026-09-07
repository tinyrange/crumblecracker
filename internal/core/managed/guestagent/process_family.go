//go:build !windows

package guestagent

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ProcessFamily tracks descendants while their parentage is still observable.
// Unlike a process group, the retained PID set follows children across setsid
// and daemon reparenting so command cancellation can contain the whole workload.
type ProcessFamily struct {
	root    int
	tracker processFamilyTracker
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	pids    map[int]struct{}
}

type processFamilyTracker interface {
	Snapshot() map[int]struct{}
	Close()
}

type processFamilyKiller interface {
	Kill() error
}

// processFamilyKernelTracker marks trackers whose kernel primitive follows the
// complete descendant set without process-table polling. Other platform
// trackers may still require refresh both to open a command start gate and to
// discover descendants not represented by their native notification stream.
type processFamilyKernelTracker interface {
	processFamilyTracker
	kernelTracksDescendants()
}

type processFamilyPreparation interface {
	Start(root int) (processFamilyTracker, error)
	Abort()
}

type PreparedProcessFamily struct {
	platform processFamilyPreparation
}

func PrepareProcessFamily(cmd *exec.Cmd, id string) (*PreparedProcessFamily, error) {
	platform, err := prepareProcessFamily(cmd, id)
	if err != nil {
		return nil, err
	}
	return &PreparedProcessFamily{platform: platform}, nil
}

func (p *PreparedProcessFamily) Start(root int) *ProcessFamily {
	var tracker processFamilyTracker
	if p != nil && p.platform != nil {
		tracker, _ = p.platform.Start(root)
		p.platform = nil
	}
	return newProcessFamily(root, tracker)
}

func (p *PreparedProcessFamily) Abort() {
	if p != nil && p.platform != nil {
		p.platform.Abort()
		p.platform = nil
	}
}

func NewProcessFamily(root int) *ProcessFamily {
	return newProcessFamily(root, nil)
}

func newProcessFamily(root int, tracker processFamilyTracker) *ProcessFamily {
	if root <= 1 || root == os.Getpid() {
		if tracker != nil {
			tracker.Close()
		}
		return &ProcessFamily{done: make(chan struct{}), pids: make(map[int]struct{})}
	}
	f := &ProcessFamily{root: root, tracker: tracker, done: make(chan struct{}), pids: map[int]struct{}{root: {}}}
	// Linux cgroups already track forks, setsid, and daemon reparenting in the
	// kernel. Per-command process-table polling is both redundant there and
	// catastrophically expensive for many persistent contexts. BSD's native
	// tracker deliberately is not marked: its first snapshot opens the process
	// start gate and its event stream still benefits from ancestry observation.
	if _, kernelTracked := tracker.(processFamilyKernelTracker); !kernelTracked {
		go f.watch()
	}
	return f
}

func (f *ProcessFamily) watch() {
	// Monitoring must never hold command startup hostage. Platform snapshots can
	// involve virtual filesystems that are temporarily slow after VM restore;
	// the root PID and platform tracker are already armed before this starts.
	f.refresh()
	ticker := time.NewTicker(processFamilyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			f.refresh()
		case <-f.done:
			return
		}
	}
}

func (f *ProcessFamily) refresh() {
	tracked := map[int]struct{}{}
	if f.tracker != nil {
		// Platform trackers are armed before the command's start gate opens.
		// Snapshot first so the gate cannot remain closed merely because a
		// process-table facility is temporarily unavailable.
		tracked = f.tracker.Snapshot()
	}
	table, _ := processSnapshot("")
	if len(table) == 0 && len(tracked) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for pid := range f.pids {
		if _, exists := table[pid]; !exists {
			delete(f.pids, pid)
		}
	}
	for pid := range tracked {
		if pid > 1 && pid != os.Getpid() {
			f.pids[pid] = struct{}{}
		}
	}
	changed := true
	for changed {
		changed = false
		for pid, ppid := range table {
			if _, known := f.pids[pid]; known {
				continue
			}
			if _, parentKnown := f.pids[ppid]; parentKnown {
				f.pids[pid] = struct{}{}
				changed = true
			}
		}
	}
}

func (f *ProcessFamily) Signal(sig syscall.Signal) error {
	f.refresh()
	f.mu.Lock()
	pids := make([]int, 0, len(f.pids))
	for pid := range f.pids {
		pids = append(pids, pid)
	}
	f.mu.Unlock()
	var firstErr error
	for _, pid := range pids {
		// The guest agent is PID 1. It is the containment boundary, never part
		// of a command family, even if a transient process-table observation is
		// malformed or a PID is recycled.
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f *ProcessFamily) Terminate() {
	_ = f.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !f.alive() {
			f.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = f.Signal(syscall.SIGKILL)
	if killer, ok := f.tracker.(processFamilyKiller); ok {
		_ = killer.Kill()
	}
	deadline = time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		f.reapExited()
		if !f.alive() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.Close()
}

func (f *ProcessFamily) reapExited() {
	if os.Getpid() != 1 {
		return
	}
	f.mu.Lock()
	pids := make([]int, 0, len(f.pids))
	for pid := range f.pids {
		if pid > 1 {
			pids = append(pids, pid)
		}
	}
	f.mu.Unlock()
	for _, pid := range pids {
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	}
}

func (f *ProcessFamily) alive() bool {
	f.refresh()
	f.mu.Lock()
	defer f.mu.Unlock()
	for pid := range f.pids {
		if pid == f.root {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
			return true
		}
	}
	return false
}

func (f *ProcessFamily) Close() {
	f.once.Do(func() {
		close(f.done)
		if f.tracker != nil {
			f.tracker.Close()
		}
	})
}
