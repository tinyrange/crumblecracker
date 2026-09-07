package virtio

import (
	"testing"
	"time"
)

func TestFuseLockManagerEnforcesExclusionAndWakesWaiters(t *testing.T) {
	locks := newFuseLockManager()
	writer := fuseFileLock{nodeID: 7, owner: 10, start: 0, end: ^uint64(0), typeID: linuxFWrLck, pid: 100}
	if errno := locks.set(writer, false); errno != 0 {
		t.Fatalf("set writer lock: errno %d", errno)
	}
	reader := fuseFileLock{nodeID: 7, owner: 20, start: 0, end: ^uint64(0), typeID: linuxFRdLck, pid: 200}
	if errno := locks.set(reader, false); errno != -linuxEAGAIN {
		t.Fatalf("conflicting nonblocking lock: errno %d", errno)
	}
	if conflict, ok := locks.get(reader); !ok || conflict.owner != writer.owner || conflict.typeID != linuxFWrLck {
		t.Fatalf("reported conflict = %+v, present=%t", conflict, ok)
	}

	waiting := make(chan int32, 1)
	go func() { waiting <- locks.set(reader, true) }()
	select {
	case errno := <-waiting:
		t.Fatalf("blocking lock completed before unlock: errno %d", errno)
	case <-time.After(25 * time.Millisecond):
	}
	locks.releaseOwner(writer.nodeID, writer.owner)
	select {
	case errno := <-waiting:
		if errno != 0 {
			t.Fatalf("blocking lock after unlock: errno %d", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking lock did not wake after owner release")
	}
}

func TestFuseLockManagerSplitsPartialUnlocks(t *testing.T) {
	locks := newFuseLockManager()
	if errno := locks.set(fuseFileLock{nodeID: 3, owner: 4, start: 0, end: 99, typeID: linuxFWrLck}, false); errno != 0 {
		t.Fatalf("set lock: errno %d", errno)
	}
	if errno := locks.set(fuseFileLock{nodeID: 3, owner: 4, start: 25, end: 74, typeID: linuxFUnlck}, false); errno != 0 {
		t.Fatalf("partial unlock: errno %d", errno)
	}
	for _, test := range []struct {
		start, end uint64
		conflict   bool
	}{
		{0, 24, true},
		{25, 74, false},
		{75, 99, true},
	} {
		_, conflict := locks.get(fuseFileLock{nodeID: 3, owner: 5, start: test.start, end: test.end, typeID: linuxFRdLck})
		if conflict != test.conflict {
			t.Fatalf("range %d-%d conflict=%t, want %t", test.start, test.end, conflict, test.conflict)
		}
	}
}

func TestFuseLockManagerRejectsInvalidRequests(t *testing.T) {
	locks := newFuseLockManager()
	for _, request := range []fuseFileLock{
		{start: 2, end: 1, typeID: linuxFRdLck},
		{start: 0, end: 1, typeID: 99},
	} {
		if errno := locks.set(request, false); errno != -linuxEINVAL {
			t.Fatalf("set invalid lock %+v: errno %d", request, errno)
		}
	}
	if validFuseGetLock(fuseFileLock{start: 2, end: 1, typeID: linuxFRdLck}) {
		t.Fatal("GETLK accepted a reversed range")
	}
	if validFuseGetLock(fuseFileLock{start: 0, end: 1, typeID: linuxFUnlck}) {
		t.Fatal("GETLK accepted an unlock request")
	}
}

func TestFuseLockManagerCloseInterruptsBlockedLocks(t *testing.T) {
	locks := newFuseLockManager()
	if errno := locks.set(fuseFileLock{nodeID: 1, owner: 1, end: ^uint64(0), typeID: linuxFWrLck}, false); errno != 0 {
		t.Fatalf("set lock: errno %d", errno)
	}
	done := make(chan int32, 1)
	go func() {
		done <- locks.set(fuseFileLock{nodeID: 1, owner: 2, end: ^uint64(0), typeID: linuxFWrLck}, true)
	}()
	locks.close()
	select {
	case errno := <-done:
		if errno != -linuxEINTR {
			t.Fatalf("closed lock wait: errno %d", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("closed lock manager did not wake waiter")
	}
}
