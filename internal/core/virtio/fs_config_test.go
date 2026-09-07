package virtio

import (
	"encoding/binary"
	"testing"
)

type recordingFSDispatcher struct {
	request []byte
}

func (d *recordingFSDispatcher) Dispatch(raw []byte) (fsDispatchResult, error) {
	d.request = append(d.request[:0], raw...)
	return fsDispatchResult{opcode: fuseGetAttr, unique: 99}, nil
}

func dispatchFUSEForTest(device *FS, raw []byte) (fsReply, error) {
	result, err := device.dispatcher.Dispatch(raw)
	return result.reply, err
}

func TestVirtioFSKickPollingRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("CCX3_VIRTIOFS_KICK_POLL", "")
	if resolveVirtioFSKickPoll() {
		t.Fatal("virtio-fs kick polling enabled without explicit opt-in")
	}

	t.Setenv("CCX3_VIRTIOFS_KICK_POLL", "true")
	if !resolveVirtioFSKickPoll() {
		t.Fatal("virtio-fs kick polling not enabled by explicit opt-in")
	}

	t.Setenv("CCX3_VIRTIOFS_KICK_POLL", "invalid")
	if resolveVirtioFSKickPoll() {
		t.Fatal("invalid virtio-fs kick polling setting enabled polling")
	}
}

func TestVirtioFSQueueTreatsRequestsAsOpaque(t *testing.T) {
	dev := NewFS(0, 0, 0, "test", nil)
	dispatcher := &recordingFSDispatcher{}
	dev.dispatcher = dispatcher
	raw := []byte{0xff, 0x00, 0x7f, 0x80, 0x01}

	if err := dev.processWorksInline([]fsWork{{
		qidx:       fsQueueRequest,
		generation: dev.configGeneration,
		req:        append([]byte(nil), raw...),
	}}); err != nil {
		t.Fatal(err)
	}
	if string(dispatcher.request) != string(raw) {
		t.Fatalf("dispatcher received %x, want %x", dispatcher.request, raw)
	}
}

func TestVirtioFSCompletionUsesCurrentInterruptPreference(t *testing.T) {
	mem := make(testGuestMemory, 0x2000)
	dev := NewFS(0, 0x1000, 11, "root", nil)
	dev.Attach(mem, &testIRQ{})
	q := &dev.queues[fsQueueRequest]
	q.size = 8
	q.ready = true
	q.availAddr = 0x1000
	q.usedIdx = 2

	// A request may be harvested while the driver is polling and suppressing
	// interrupts, then complete after the driver has gone to sleep.
	binary.LittleEndian.PutUint16(mem[q.availAddr:], fsAvailNoInterrupt)
	interrupt, err := dev.shouldInterruptCompletionLocked(q, 1)
	if err != nil {
		t.Fatal(err)
	}
	if interrupt {
		t.Fatal("completion ignored the driver's current interrupt suppression")
	}
	binary.LittleEndian.PutUint16(mem[q.availAddr:], 0)
	interrupt, err = dev.shouldInterruptCompletionLocked(q, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !interrupt {
		t.Fatal("completion retained a stale interrupt-suppressed decision")
	}
}

func TestVirtioFSPokeRaisesVringIRQ(t *testing.T) {
	irq := &recordingIRQ{}
	fs := &FS{IRQ: 18, irq: irq}
	if err := fs.Poke(); err != nil {
		t.Fatalf("Poke: %v", err)
	}
	if len(irq.levels) != 1 || !irq.levels[0] {
		t.Fatalf("IRQ levels = %v, want [true]", irq.levels)
	}
	if !fs.irqHigh || fs.interruptStatus&fsInterruptVring == 0 {
		t.Fatalf("vring IRQ was not raised: high=%t status=%#x", fs.irqHigh, fs.interruptStatus)
	}
}

func TestVirtioFSPollReportsUnsupportedWithoutNotifications(t *testing.T) {
	const unique = 42
	req := make([]byte, fuseInHeaderSize+32)
	binary.LittleEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.LittleEndian.PutUint32(req[4:8], fusePoll)
	binary.LittleEndian.PutUint64(req[8:16], unique)

	reply, err := dispatchFUSEForTest(NewFS(0, 0, 0, "test", nil), req)
	if err != nil {
		t.Fatal(err)
	}
	if reply.unique != unique || reply.errno != -linuxENOSYS || len(reply.extra) != 0 {
		t.Fatalf("POLL reply = unique %d errno %d extra %x", reply.unique, reply.errno, reply.extra)
	}
}

func TestVirtioFSReadDirPlusBatchesDirectoryAttributes(t *testing.T) {
	backend := newEmptyImageFS(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		nodeID, fh := createImageFile(t, backend, name, name)
		backend.Release(nodeID, fh)
	}
	fh, errno := backend.OpenDir(1, 0)
	if errno != 0 {
		t.Fatalf("open directory: errno %d", errno)
	}
	defer backend.ReleaseDir(1, fh)

	initReq := make([]byte, fuseInHeaderSize+16)
	binary.LittleEndian.PutUint32(initReq[0:4], uint32(len(initReq)))
	binary.LittleEndian.PutUint32(initReq[4:8], fuseInit)
	initReply, err := dispatchFUSEForTest(NewFS(0, 0, 0, "test", backend), initReq)
	if err != nil {
		t.Fatal(err)
	}
	if flags := binary.LittleEndian.Uint32(initReply.extra[12:16]); flags&fuseCapDoReadDirPlus == 0 {
		t.Fatalf("INIT flags %#x do not advertise READDIRPLUS", flags)
	}

	req := make([]byte, fuseInHeaderSize+24)
	binary.LittleEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.LittleEndian.PutUint32(req[4:8], fuseReadDirPlus)
	binary.LittleEndian.PutUint64(req[8:16], 42)
	binary.LittleEndian.PutUint64(req[16:24], 1)
	binary.LittleEndian.PutUint64(req[40:48], fh)
	binary.LittleEndian.PutUint32(req[56:60], 4096)
	reply, err := dispatchFUSEForTest(NewFS(0, 0, 0, "test", backend), req)
	if err != nil {
		t.Fatal(err)
	}
	if reply.errno != 0 {
		t.Fatalf("READDIRPLUS errno = %d", reply.errno)
	}
	count := 0
	for cursor := 0; cursor < len(reply.extra); {
		if len(reply.extra)-cursor < fuseDirentPlusBaseSize {
			t.Fatalf("short READDIRPLUS record at %d", cursor)
		}
		entryNode := binary.LittleEndian.Uint64(reply.extra[cursor : cursor+8])
		attrNode := binary.LittleEndian.Uint64(reply.extra[cursor+40 : cursor+48])
		dirent := cursor + fuseEntryOutSize
		direntNode := binary.LittleEndian.Uint64(reply.extra[dirent : dirent+8])
		nameBytes := int(binary.LittleEndian.Uint32(reply.extra[dirent+16 : dirent+20]))
		recordBytes := align8(fuseDirentPlusBaseSize + nameBytes)
		if recordBytes > len(reply.extra)-cursor {
			t.Fatalf("READDIRPLUS record exceeds reply at %d", cursor)
		}
		if entryNode == 0 || entryNode != attrNode || entryNode != direntNode {
			t.Fatalf("READDIRPLUS node mismatch: entry=%d attr=%d dirent=%d", entryNode, attrNode, direntNode)
		}
		count++
		cursor += recordBytes
	}
	if count != 5 {
		t.Fatalf("READDIRPLUS entries = %d, want 5", count)
	}
}

func TestVirtioFSMountedExchangeFailsInsteadOfReportingUnsafeSuccess(t *testing.T) {
	backend := newEmptyImageFS(t)
	for _, name := range []string{"left", "right"} {
		nodeID, fh := createImageFile(t, backend, name, name)
		backend.Release(nodeID, fh)
	}
	req := make([]byte, fuseInHeaderSize+16+len("left\x00right\x00"))
	binary.LittleEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.LittleEndian.PutUint32(req[4:8], fuseRename2)
	binary.LittleEndian.PutUint64(req[16:24], 1)
	binary.LittleEndian.PutUint64(req[40:48], 1)
	binary.LittleEndian.PutUint32(req[48:52], linuxRenameExchange)
	copy(req[fuseInHeaderSize+16:], "left\x00right\x00")

	reply, err := dispatchFUSEForTest(NewFS(0, 0, 0, "test", backend), req)
	if err != nil {
		t.Fatal(err)
	}
	if reply.errno != -linuxEOPNOTSUPP {
		t.Fatalf("mounted exchange errno = %d, want %d", reply.errno, -linuxEOPNOTSUPP)
	}
	for _, name := range []string{"left", "right"} {
		nodeID, _, errno := backend.Lookup(1, name)
		if errno != 0 {
			t.Fatalf("lookup %q: errno %d", name, errno)
		}
		fh, errno := backend.Open(nodeID, linuxORDONLY)
		if errno != 0 {
			t.Fatalf("open %q: errno %d", name, errno)
		}
		if got := readImageHandle(t, backend, nodeID, fh); got != name {
			t.Fatalf("%s changed to %q after rejected exchange", name, got)
		}
		backend.Release(nodeID, fh)
	}
}
