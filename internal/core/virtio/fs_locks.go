package virtio

import (
	"sort"
	"sync"
)

const (
	linuxFRdLck = 0
	linuxFWrLck = 1
)

type fuseFileLock struct {
	nodeID uint64
	owner  uint64
	start  uint64
	end    uint64
	typeID uint32
	pid    uint32
}

type fuseLockManager struct {
	mu     sync.Mutex
	cond   *sync.Cond
	locks  []fuseFileLock
	closed bool
}

func newFuseLockManager() *fuseLockManager {
	m := &fuseLockManager{}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *fuseLockManager) get(request fuseFileLock) (fuseFileLock, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conflictLocked(request)
}

func validFuseLockRange(request fuseFileLock) bool {
	return request.start <= request.end
}

func validFuseGetLock(request fuseFileLock) bool {
	return validFuseLockRange(request) && (request.typeID == linuxFRdLck || request.typeID == linuxFWrLck)
}

func (m *fuseLockManager) set(request fuseFileLock, wait bool) int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validFuseLockRange(request) || (request.typeID != linuxFRdLck && request.typeID != linuxFWrLck && request.typeID != linuxFUnlck) {
		return -linuxEINVAL
	}
	for request.typeID != linuxFUnlck {
		if _, conflict := m.conflictLocked(request); !conflict {
			break
		}
		if !wait {
			return -linuxEAGAIN
		}
		if m.closed {
			return -linuxEINTR
		}
		m.cond.Wait()
	}
	if m.closed {
		return -linuxEINTR
	}
	m.replaceOwnerRangeLocked(request)
	m.cond.Broadcast()
	return 0
}

func (m *fuseLockManager) releaseOwner(nodeID, owner uint64) {
	m.mu.Lock()
	kept := m.locks[:0]
	for _, lock := range m.locks {
		if lock.nodeID != nodeID || lock.owner != owner {
			kept = append(kept, lock)
		}
	}
	m.locks = kept
	m.cond.Broadcast()
	m.mu.Unlock()
}

func (m *fuseLockManager) close() {
	m.mu.Lock()
	m.closed = true
	m.locks = nil
	m.cond.Broadcast()
	m.mu.Unlock()
}

func (m *fuseLockManager) conflictLocked(request fuseFileLock) (fuseFileLock, bool) {
	for _, held := range m.locks {
		if held.nodeID != request.nodeID || held.owner == request.owner || !lockRangesOverlap(held, request) {
			continue
		}
		if held.typeID == linuxFWrLck || request.typeID == linuxFWrLck {
			return held, true
		}
	}
	return fuseFileLock{}, false
}

func (m *fuseLockManager) replaceOwnerRangeLocked(request fuseFileLock) {
	rebuilt := make([]fuseFileLock, 0, len(m.locks)+1)
	for _, held := range m.locks {
		if held.nodeID != request.nodeID || held.owner != request.owner || !lockRangesOverlap(held, request) {
			rebuilt = append(rebuilt, held)
			continue
		}
		if held.start < request.start {
			left := held
			left.end = request.start - 1
			rebuilt = append(rebuilt, left)
		}
		if request.end != ^uint64(0) && held.end > request.end {
			right := held
			right.start = request.end + 1
			rebuilt = append(rebuilt, right)
		}
	}
	if request.typeID != linuxFUnlck {
		rebuilt = append(rebuilt, request)
	}
	sort.Slice(rebuilt, func(i, j int) bool {
		if rebuilt[i].nodeID != rebuilt[j].nodeID {
			return rebuilt[i].nodeID < rebuilt[j].nodeID
		}
		if rebuilt[i].owner != rebuilt[j].owner {
			return rebuilt[i].owner < rebuilt[j].owner
		}
		return rebuilt[i].start < rebuilt[j].start
	})
	merged := rebuilt[:0]
	for _, lock := range rebuilt {
		if len(merged) != 0 {
			previous := &merged[len(merged)-1]
			adjacent := previous.end == ^uint64(0) || lock.start <= previous.end+1
			if previous.nodeID == lock.nodeID && previous.owner == lock.owner && previous.typeID == lock.typeID && adjacent {
				if lock.end > previous.end {
					previous.end = lock.end
				}
				continue
			}
		}
		merged = append(merged, lock)
	}
	m.locks = merged
}

func lockRangesOverlap(left, right fuseFileLock) bool {
	return left.start <= right.end && right.start <= left.end
}
