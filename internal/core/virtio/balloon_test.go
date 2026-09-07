package virtio

import (
	"encoding/binary"
	"reflect"
	"testing"
)

type testBalloonMemory struct {
	testGuestMemory
	reclaimed []uint64
	reused    []uint64
}

type testRangeBalloonMemory struct {
	testBalloonMemory
	reclaimedRanges [][2]uint64
	reusedRanges    [][2]uint64
}

func (m *testRangeBalloonMemory) ReclaimGuestPages(ipa, size uint64) error {
	m.reclaimedRanges = append(m.reclaimedRanges, [2]uint64{ipa, size})
	return nil
}

func (m *testRangeBalloonMemory) ReuseGuestPages(ipa, size uint64) error {
	m.reusedRanges = append(m.reusedRanges, [2]uint64{ipa, size})
	return nil
}

func (m *testBalloonMemory) ReclaimGuestPage(ipa uint64) error {
	m.reclaimed = append(m.reclaimed, ipa)
	return nil
}

func (m *testBalloonMemory) ReuseGuestPage(ipa uint64) error {
	m.reused = append(m.reused, ipa)
	return nil
}

func TestBalloonTargetRaisesConfigInterrupt(t *testing.T) {
	irq := &testIRQ{}
	dev := NewBalloon(0, 0x1000, 12)
	dev.Attach(make(testGuestMemory, 0x10000), irq)
	if err := dev.Write(regStatus, 4, balloonDriverOK); err != nil {
		t.Fatalf("mark driver ready: %v", err)
	}

	if err := dev.SetTargetPages(16); err != nil {
		t.Fatalf("SetTargetPages: %v", err)
	}
	if got, err := dev.Read(regConfig, 4); err != nil || got != 16 {
		t.Fatalf("target pages = %d, %v; want 16, nil", got, err)
	}
	if got, err := dev.Read(regInterruptStatus, 4); err != nil || got != intConfig {
		t.Fatalf("interrupt status = %#x, %v; want config", got, err)
	}
	if !irq.level {
		t.Fatalf("config update did not assert IRQ")
	}
	if err := dev.Write(regInterruptAck, 4, intConfig); err != nil {
		t.Fatalf("ack config interrupt: %v", err)
	}
	if irq.level {
		t.Fatalf("config ack left IRQ asserted")
	}
}

func TestBalloonTargetBeforeDriverReadyDefersConfigInterrupt(t *testing.T) {
	irq := &testIRQ{}
	dev := NewBalloon(0, 0x1000, 12)
	dev.Attach(make(testGuestMemory, 0x10000), irq)

	if err := dev.SetTargetPages(16); err != nil {
		t.Fatalf("set initial target: %v", err)
	}
	if irq.level {
		t.Fatal("initial target asserted IRQ before the guest driver was ready")
	}
	if got, err := dev.Read(regInterruptStatus, 4); err != nil || got != 0 {
		t.Fatalf("interrupt status = %#x, %v; want 0 before driver readiness", got, err)
	}
	if got, err := dev.Read(regConfig, 4); err != nil || got != 16 {
		t.Fatalf("target pages = %d, %v; want 16, nil", got, err)
	}

	if err := dev.Write(regStatus, 4, balloonDriverOK); err != nil {
		t.Fatalf("mark driver ready: %v", err)
	}
	if !irq.level {
		t.Fatal("driver readiness did not publish the pending balloon target")
	}
}

func TestBalloonInflateAndDeflateReclaimPages(t *testing.T) {
	mem := &testBalloonMemory{testGuestMemory: make(testGuestMemory, 0x20000)}
	irq := &testIRQ{}
	dev := NewBalloon(0, 0x1000, 12)
	dev.Attach(mem, irq)
	configureBalloonQueue(t, dev, balloonQueueInflate, 8, 0x1000, 0x2000, 0x3000)
	configureBalloonQueue(t, dev, balloonQueueDeflate, 8, 0x4000, 0x5000, 0x6000)

	writeBalloonPFNChain(mem.testGuestMemory, 0x1000, 0x2000, 0x8000, 0, []uint32{3, 5})
	if err := dev.Write(regQueueNotify, 4, balloonQueueInflate); err != nil {
		t.Fatalf("notify inflate: %v", err)
	}
	if got, want := mem.reclaimed, []uint64{3 * balloonPageSize, 5 * balloonPageSize}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("reclaimed pages = %v, want %v", got, want)
	}
	if got := binary.LittleEndian.Uint16(mem.testGuestMemory[0x3000+2:]); got != 1 {
		t.Fatalf("inflate used idx = %d, want 1", got)
	}
	if !irq.level {
		t.Fatalf("inflate completion did not assert IRQ")
	}
	if err := dev.Write(regInterruptAck, 4, intVring); err != nil {
		t.Fatalf("ack inflate interrupt: %v", err)
	}

	writeBalloonPFNChain(mem.testGuestMemory, 0x4000, 0x5000, 0x9000, 0, []uint32{5})
	if err := dev.Write(regQueueNotify, 4, balloonQueueDeflate); err != nil {
		t.Fatalf("notify deflate: %v", err)
	}
	if got, want := mem.reused, []uint64{5 * balloonPageSize}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("reused pages = %v, want %v", got, want)
	}
}

func TestBalloonEmptyQueueNotifyDoesNotInterrupt(t *testing.T) {
	mem := make(testGuestMemory, 0x10000)
	irq := &testIRQ{}
	dev := NewBalloon(0, 0x1000, 12)
	dev.Attach(mem, irq)
	configureBalloonQueue(t, dev, balloonQueueInflate, 8, 0x1000, 0x2000, 0x3000)

	if err := dev.Write(regQueueNotify, 4, balloonQueueInflate); err != nil {
		t.Fatalf("notify empty queue: %v", err)
	}
	if irq.level {
		t.Fatalf("empty notify asserted IRQ")
	}
	if got, err := dev.Read(regInterruptStatus, 4); err != nil || got != 0 {
		t.Fatalf("interrupt status = %#x, %v; want 0, nil", got, err)
	}
}

func TestBalloonCoalescesContiguousPageReclaims(t *testing.T) {
	mem := &testRangeBalloonMemory{testBalloonMemory: testBalloonMemory{testGuestMemory: make(testGuestMemory, 0x20000)}}
	dev := NewBalloon(0, 0x1000, 12)
	dev.Attach(mem, &testIRQ{})
	configureBalloonQueue(t, dev, balloonQueueInflate, 8, 0x1000, 0x2000, 0x3000)

	writeBalloonPFNChain(mem.testGuestMemory, 0x1000, 0x2000, 0x8000, 0, []uint32{3, 4, 5, 9, 10})
	if err := dev.Write(regQueueNotify, 4, balloonQueueInflate); err != nil {
		t.Fatalf("notify inflate: %v", err)
	}
	want := [][2]uint64{{3 * balloonPageSize, 3 * balloonPageSize}, {9 * balloonPageSize, 2 * balloonPageSize}}
	if len(mem.reclaimedRanges) != len(want) {
		t.Fatalf("reclaimed ranges = %v, want %v", mem.reclaimedRanges, want)
	}
	for i := range want {
		if mem.reclaimedRanges[i] != want[i] {
			t.Fatalf("reclaimed ranges = %v, want %v", mem.reclaimedRanges, want)
		}
	}
}

func TestBalloonAtTargetTracksGuestAcknowledgement(t *testing.T) {
	dev := NewBalloon(0, 0x1000, 12)
	if !dev.AtTarget() {
		t.Fatal("zero-sized balloon should start at its target")
	}
	if err := dev.SetTargetPages(100); err != nil {
		t.Fatalf("set target: %v", err)
	}
	if dev.AtTarget() {
		t.Fatal("balloon unexpectedly at an unacknowledged target")
	}
	dev.mu.Lock()
	dev.actualPages = 100
	dev.mu.Unlock()
	if !dev.AtTarget() {
		t.Fatal("balloon should be at its acknowledged target")
	}
	if err := dev.SetTargetPages(50); err != nil {
		t.Fatalf("lower target: %v", err)
	}
	if dev.AtTarget() {
		t.Fatal("balloon incorrectly reported a deflation request as acknowledged")
	}
}

func TestBalloonSnapshotRoundTripsCompactInflatedPages(t *testing.T) {
	mem := &testBalloonMemory{testGuestMemory: make(testGuestMemory, 0x20000)}
	dev := NewBalloon(0, 0x1000, 12)
	dev.Attach(mem, &testIRQ{})
	configureBalloonQueue(t, dev, balloonQueueInflate, 8, 0x1000, 0x2000, 0x3000)
	writeBalloonPFNChain(mem.testGuestMemory, 0x1000, 0x2000, 0x8000, 0, []uint32{3, 5, 65})
	if err := dev.Write(regQueueNotify, 4, balloonQueueInflate); err != nil {
		t.Fatalf("notify inflate: %v", err)
	}

	state := dev.SnapshotState()
	if len(state.InflatedPages) != 0 || len(state.InflatedPageWords) != 2 {
		t.Fatalf("inflated state uses pages=%v words=%v, want two compact words", state.InflatedPages, state.InflatedPageWords)
	}
	restored := NewBalloon(0, 0x1000, 12)
	restored.Attach(make(testGuestMemory, 0x20000), &testIRQ{})
	if err := restored.RestoreState(state); err != nil {
		t.Fatalf("restore state: %v", err)
	}
	if got := restored.SnapshotState().InflatedPageWords; !reflect.DeepEqual(got, state.InflatedPageWords) {
		t.Fatalf("restored inflated words = %v, want %v", got, state.InflatedPageWords)
	}
}

func configureBalloonQueue(t *testing.T, dev *Balloon, qidx uint32, size uint64, desc, avail, used uint64) {
	t.Helper()
	writes := []struct {
		reg   uint64
		value uint64
	}{
		{regQueueSel, uint64(qidx)},
		{regQueueNum, size},
		{regQueueDescLow, desc},
		{regQueueAvailLow, avail},
		{regQueueUsedLow, used},
		{regQueueReady, 1},
	}
	for _, write := range writes {
		if err := dev.Write(write.reg, 4, write.value); err != nil {
			t.Fatalf("write queue reg %#x: %v", write.reg, err)
		}
	}
}

func writeBalloonPFNChain(mem testGuestMemory, desc, avail, data uint64, slot uint16, pfns []uint32) {
	for i, pfn := range pfns {
		binary.LittleEndian.PutUint32(mem[data+uint64(i*4):], pfn)
	}
	writeDesc(mem, desc, data, uint32(len(pfns)*4), 0, 0)
	binary.LittleEndian.PutUint16(mem[avail+2:], slot+1)
	binary.LittleEndian.PutUint16(mem[avail+4+uint64(slot)*2:], 0)
}
