package runtime

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	intcvmfs "github.com/tinyrange/crumblecracker/internal/core/cvmfs"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type cvmfsMonitor struct {
	mu            sync.Mutex
	cacheRoot     string
	cacheLimit    int64
	cacheBytes    int64
	active        map[uint64]client.CVMFSTransferState
	selected      string
	lastError     string
	lastErrorTime time.Time
	janitorActive bool
}

func (m *cvmfsMonitor) SetSelectedMirror(mirror string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.selected = mirror
	m.mu.Unlock()
}

func newCVMFSMonitor(cacheRoot string, cacheLimit int64) *cvmfsMonitor {
	m := &cvmfsMonitor{
		cacheRoot: strings.TrimSpace(cacheRoot), cacheLimit: cacheLimit,
		active: make(map[uint64]client.CVMFSTransferState), janitorActive: true,
	}
	go m.enforceCacheLimit()
	return m
}

func (m *cvmfsMonitor) Record(event intcvmfs.TransferEvent) {
	if m == nil || event.ID == 0 {
		return
	}
	m.mu.Lock()
	switch event.State {
	case "started", "progress":
		started := event.Time
		if previous, ok := m.active[event.ID]; ok && previous.StartedAt != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, previous.StartedAt); err == nil {
				started = parsed
			}
		}
		state := client.CVMFSTransferState{
			ID: event.ID, Path: "/cvmfs/" + event.Repo + "/" + strings.TrimPrefix(event.Path, "/"),
			Mirror: event.Mirror, Bytes: event.Bytes, TotalBytes: event.TotalBytes,
			StartedAt: started.UTC().Format(time.RFC3339Nano),
		}
		if state.TotalBytes > 0 {
			state.Progress = min(1, float64(state.Bytes)/float64(state.TotalBytes))
		}
		m.active[event.ID] = state
		m.selected = event.Mirror
	case "completed":
		delete(m.active, event.ID)
		m.selected = event.Mirror
		m.triggerJanitorLocked()
	case "failed":
		delete(m.active, event.ID)
		m.selected = event.Mirror
		m.lastError = event.Error
		m.lastErrorTime = event.Time
		m.triggerJanitorLocked()
	}
	m.mu.Unlock()
}

func (m *cvmfsMonitor) Status() client.CVMFSStatusResponse {
	if m == nil {
		return client.CVMFSStatusResponse{State: "idle", ActiveTransfers: []client.CVMFSTransferState{}}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status := client.CVMFSStatusResponse{
		State: "idle", SelectedMirror: m.selected, CacheBytes: m.cacheBytes,
		CacheLimitBytes: m.cacheLimit, ActiveTransfers: make([]client.CVMFSTransferState, 0, len(m.active)),
	}
	totalsKnown := true
	for _, transfer := range m.active {
		status.ActiveTransfers = append(status.ActiveTransfers, transfer)
		status.Bytes += transfer.Bytes
		status.TotalBytes += transfer.TotalBytes
		if transfer.TotalBytes <= 0 {
			totalsKnown = false
		}
	}
	sort.Slice(status.ActiveTransfers, func(i, j int) bool {
		if status.ActiveTransfers[i].StartedAt == status.ActiveTransfers[j].StartedAt {
			return status.ActiveTransfers[i].Path < status.ActiveTransfers[j].Path
		}
		return status.ActiveTransfers[i].StartedAt < status.ActiveTransfers[j].StartedAt
	})
	if len(status.ActiveTransfers) != 0 {
		status.State = "downloading"
		if totalsKnown && status.TotalBytes > 0 {
			status.Progress = min(1, float64(status.Bytes)/float64(status.TotalBytes))
		} else {
			status.TotalBytes = 0
		}
	} else if m.lastError != "" && time.Since(m.lastErrorTime) < 30*time.Second {
		status.State = "error"
		status.LastError = m.lastError
		status.LastErrorUnix = m.lastErrorTime.Unix()
	}
	return status
}

func (m *cvmfsMonitor) triggerJanitorLocked() {
	if m.cacheRoot == "" || m.janitorActive {
		return
	}
	m.janitorActive = true
	go m.enforceCacheLimit()
}

type cvmfsCacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

func (m *cvmfsMonitor) enforceCacheLimit() {
	if m == nil || m.cacheRoot == "" {
		return
	}
	var total int64
	var entries []cvmfsCacheEntry
	_ = filepath.WalkDir(m.cacheRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		rel, err := filepath.Rel(m.cacheRoot, path)
		if err != nil || strings.HasPrefix(rel, "state"+string(filepath.Separator)) || rel == "requests.log" || strings.HasPrefix(entry.Name(), ".cvmfs-cache-") {
			return nil
		}
		entries = append(entries, cvmfsCacheEntry{path: path, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	if m.cacheLimit > 0 && total > m.cacheLimit {
		sort.Slice(entries, func(i, j int) bool { return entries[i].modTime.Before(entries[j].modTime) })
		for _, entry := range entries {
			if total <= m.cacheLimit {
				break
			}
			if err := os.Remove(entry.path); err == nil {
				total -= entry.size
			}
		}
	}
	m.mu.Lock()
	m.cacheBytes = max(0, total)
	m.janitorActive = false
	m.mu.Unlock()
}
