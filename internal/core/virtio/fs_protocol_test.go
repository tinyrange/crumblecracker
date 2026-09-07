package virtio

import (
	"encoding/binary"
	"testing"
)

type inertFSBackend struct{}

func (inertFSBackend) Init() (uint32, uint32) { return 128 << 10, 0 }
func (inertFSBackend) GetAttr(uint64) (FuseAttr, int32) {
	return FuseAttr{}, -linuxENOENT
}
func (inertFSBackend) Lookup(uint64, string) (uint64, FuseAttr, int32) {
	return 0, FuseAttr{}, -linuxENOENT
}
func (inertFSBackend) Open(uint64, uint32) (uint64, int32) { return 0, -linuxENOENT }
func (inertFSBackend) Release(uint64, uint64)              {}
func (inertFSBackend) Read(uint64, uint64, uint64, uint32) ([]byte, int32) {
	return nil, -linuxENOENT
}
func (inertFSBackend) OpenDir(uint64, uint32) (uint64, int32) { return 0, -linuxENOENT }
func (inertFSBackend) ReadDir(uint64, uint64, uint64, uint32) ([]byte, int32) {
	return nil, -linuxENOENT
}
func (inertFSBackend) ReleaseDir(uint64, uint64) {}
func (inertFSBackend) Readlink(uint64) (string, int32) {
	return "", -linuxENOENT
}
func (inertFSBackend) StatFS(uint64) (uint64, uint64, uint64, uint64, uint64, uint64, uint64, uint64, int32) {
	return 0, 0, 0, 0, 0, 4096, 4096, 255, 0
}

func TestDecodeFUSERequest(t *testing.T) {
	raw := make([]byte, fuseInHeaderSize+7, fuseInHeaderSize+16)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(len(raw)))
	binary.LittleEndian.PutUint32(raw[4:8], fuseWrite)
	binary.LittleEndian.PutUint64(raw[8:16], 0x1020304050607080)
	binary.LittleEndian.PutUint64(raw[16:24], 71)
	binary.LittleEndian.PutUint32(raw[24:28], 1000)
	binary.LittleEndian.PutUint32(raw[28:32], 1001)
	binary.LittleEndian.PutUint32(raw[32:36], 42)
	copy(raw[fuseInHeaderSize:], "payload")

	req, err := decodeFUSERequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.opcode != fuseWrite || req.unique != 0x1020304050607080 || req.nodeID != 71 {
		t.Fatalf("decoded identity = opcode %d unique %#x node %d", req.opcode, req.unique, req.nodeID)
	}
	if req.callerUID != 1000 || req.callerGID != 1001 || req.callerPID != 42 {
		t.Fatalf("decoded caller = uid %d gid %d pid %d", req.callerUID, req.callerGID, req.callerPID)
	}
	if string(req.body) != "payload" {
		t.Fatalf("decoded body = %q", req.body)
	}
	if len(req.raw) != len(raw) {
		t.Fatalf("decoded request length = %d, want %d", len(req.raw), len(raw))
	}
}

func TestDecodeFUSERequestRejectsInvalidLengths(t *testing.T) {
	tests := []struct {
		name     string
		raw      []byte
		declared uint32
	}{
		{name: "short header", raw: make([]byte, fuseInHeaderSize-1)},
		{name: "short declared length", raw: make([]byte, fuseInHeaderSize), declared: fuseInHeaderSize - 1},
		{name: "truncated body", raw: make([]byte, fuseInHeaderSize), declared: fuseInHeaderSize + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.raw) >= 4 {
				binary.LittleEndian.PutUint32(test.raw[:4], test.declared)
			}
			if _, err := decodeFUSERequest(test.raw); err == nil {
				t.Fatal("decode succeeded")
			}
		})
	}
}

func TestDecodeFUSERequestExcludesDescriptorPadding(t *testing.T) {
	raw := make([]byte, fuseInHeaderSize+8)
	binary.LittleEndian.PutUint32(raw[:4], fuseInHeaderSize)
	for index := fuseInHeaderSize; index < len(raw); index++ {
		raw[index] = 0xff
	}
	req, err := decodeFUSERequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.raw) != fuseInHeaderSize || len(req.body) != 0 {
		t.Fatalf("decoded lengths = raw %d body %d", len(req.raw), len(req.body))
	}
}

func TestFUSEDispatcherEnforcesAndReleasesPOSIXLocks(t *testing.T) {
	device := NewFS(0, 0, 0, "locks", inertFSBackend{})
	dispatchLock := func(opcode uint32, owner uint64, lockType uint32) fsReply {
		raw := make([]byte, fuseInHeaderSize+fuseLKInSize)
		binary.LittleEndian.PutUint32(raw[0:4], uint32(len(raw)))
		binary.LittleEndian.PutUint32(raw[4:8], opcode)
		binary.LittleEndian.PutUint64(raw[8:16], uint64(opcode)+owner)
		binary.LittleEndian.PutUint64(raw[16:24], 9)
		binary.LittleEndian.PutUint64(raw[40:48], 1)
		binary.LittleEndian.PutUint64(raw[48:56], owner)
		binary.LittleEndian.PutUint64(raw[64:72], ^uint64(0))
		binary.LittleEndian.PutUint32(raw[72:76], lockType)
		binary.LittleEndian.PutUint32(raw[76:80], uint32(owner))
		result, err := device.dispatcher.Dispatch(raw)
		if err != nil {
			t.Fatalf("dispatch %s: %v", fuseOpcodeName(opcode), err)
		}
		return result.reply
	}

	if reply := dispatchLock(fuseSetLK, 100, linuxFWrLck); reply.errno != 0 {
		t.Fatalf("first SETLK errno = %d", reply.errno)
	}
	get := dispatchLock(fuseGetLK, 200, linuxFRdLck)
	if get.errno != 0 || len(get.extra) != fuseLKOutSize || binary.LittleEndian.Uint32(get.extra[16:20]) != linuxFWrLck || binary.LittleEndian.Uint32(get.extra[20:24]) == 0 {
		t.Fatalf("GETLK reply = errno %d extra %x", get.errno, get.extra)
	}
	if reply := dispatchLock(fuseSetLK, 200, linuxFRdLck); reply.errno != -linuxEAGAIN {
		t.Fatalf("conflicting SETLK errno = %d", reply.errno)
	}

	flush := make([]byte, fuseInHeaderSize+24)
	binary.LittleEndian.PutUint32(flush[0:4], uint32(len(flush)))
	binary.LittleEndian.PutUint32(flush[4:8], fuseFlush)
	binary.LittleEndian.PutUint64(flush[8:16], 77)
	binary.LittleEndian.PutUint64(flush[16:24], 9)
	binary.LittleEndian.PutUint32(flush[48:52], fuseFlushLockOwner)
	binary.LittleEndian.PutUint64(flush[56:64], 100)
	if result, err := device.dispatcher.Dispatch(flush); err != nil || result.reply.errno != 0 {
		t.Fatalf("FLUSH lock owner = reply %+v, err %v", result.reply, err)
	}
	if reply := dispatchLock(fuseSetLK, 200, linuxFRdLck); reply.errno != 0 {
		t.Fatalf("SETLK after FLUSH errno = %d", reply.errno)
	}
	release := make([]byte, fuseInHeaderSize+24)
	binary.LittleEndian.PutUint32(release[0:4], uint32(len(release)))
	binary.LittleEndian.PutUint32(release[4:8], fuseRelease)
	binary.LittleEndian.PutUint64(release[8:16], 78)
	binary.LittleEndian.PutUint64(release[16:24], 9)
	binary.LittleEndian.PutUint32(release[52:56], fuseReleaseFlockUnlock)
	binary.LittleEndian.PutUint64(release[56:64], 200)
	if result, err := device.dispatcher.Dispatch(release); err != nil || result.reply.errno != 0 {
		t.Fatalf("RELEASE flock owner = reply %+v, err %v", result.reply, err)
	}
	if reply := dispatchLock(fuseSetLK, 300, linuxFWrLck); reply.errno != 0 {
		t.Fatalf("SETLK after RELEASE errno = %d", reply.errno)
	}
}

func FuzzDecodeFUSERequest(f *testing.F) {
	valid := make([]byte, fuseInHeaderSize+4)
	binary.LittleEndian.PutUint32(valid[:4], uint32(len(valid)))
	binary.LittleEndian.PutUint32(valid[4:8], fuseGetAttr)
	f.Add(valid)
	f.Add([]byte(nil))
	f.Add(make([]byte, fuseInHeaderSize))

	f.Fuzz(func(t *testing.T, raw []byte) {
		req, err := decodeFUSERequest(raw)
		if err != nil {
			return
		}
		if len(req.raw) < fuseInHeaderSize {
			t.Fatalf("decoded short request: %d", len(req.raw))
		}
		declared := binary.LittleEndian.Uint32(req.raw[:4])
		if int(declared) != len(req.raw) {
			t.Fatalf("decoded length = %d, header = %d", len(req.raw), declared)
		}
		if len(req.body) != len(req.raw)-fuseInHeaderSize {
			t.Fatalf("decoded body length = %d, raw = %d", len(req.body), len(req.raw))
		}
	})
}

func FuzzFUSEDispatcher(f *testing.F) {
	seed := make([]byte, fuseInHeaderSize+128)
	binary.LittleEndian.PutUint32(seed[:4], uint32(len(seed)))
	binary.LittleEndian.PutUint32(seed[4:8], fuseWrite)
	f.Add(seed)

	device := NewFS(0, 0, 0, "fuzz", inertFSBackend{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = device.dispatcher.Dispatch(raw)
	})
}

func BenchmarkDecodeFUSERequest(b *testing.B) {
	raw := make([]byte, fuseInHeaderSize+24)
	binary.LittleEndian.PutUint32(raw[:4], uint32(len(raw)))
	binary.LittleEndian.PutUint32(raw[4:8], fuseRead)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeFUSERequest(raw); err != nil {
			b.Fatal(err)
		}
	}
}
