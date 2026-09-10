package virtio

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestRelativePointerWireEvents(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	input := NewRelativePointerInput(0x1000, 0x1000, 11)
	input.Attach(mem, &testIRQ{})
	if input.eventBitmapLocked(inputEventAbs) != nil {
		t.Fatal("relative mouse advertises absolute axes")
	}
	rel := input.eventBitmapLocked(inputEventRel)
	if rel[0]&3 != 3 {
		t.Fatal("relative mouse lacks X/Y axes")
	}
	// Queue motion before descriptors are available: each delta must survive.
	input.RelativePointerEvent(1, -2, 1, 0)
	input.RelativePointerEvent(3, 4, 0, 1)
	q := &input.queues[inputQueueEvent]
	q.size = 16
	q.ready = true
	q.descAddr = 0x2000
	q.availAddr = 0x3000
	q.usedAddr = 0x3800
	for idx := uint16(0); idx < q.size; idx++ {
		writeDesc(mem, q.descAddr+uint64(idx)*16, 0x4000+uint64(idx)*8, 8, descFWrite, 0)
		binary.LittleEndian.PutUint16(mem[q.availAddr+4+uint64(idx)*2:], idx)
	}
	binary.LittleEndian.PutUint16(mem[q.availAddr+2:], q.size)
	if err := input.Send(); err != nil {
		t.Fatal(err)
	}
	var events []InputEvent
	for idx := uint16(0); idx < q.usedIdx; idx++ {
		raw := mem[0x4000+uint64(idx)*8:]
		events = append(events, InputEvent{binary.LittleEndian.Uint16(raw), binary.LittleEndian.Uint16(raw[2:]), int32(binary.LittleEndian.Uint32(raw[4:]))})
	}
	want := []InputEvent{{inputEventRel, inputRelX, 1}, {inputEventRel, inputRelY, -2}, {inputEventKey, inputBtnLeft, 1}, {inputEventSyn, inputSynReport, 0}, {inputEventRel, inputRelX, 3}, {inputEventRel, inputRelY, 4}, {inputEventKey, inputBtnLeft, 0}, {inputEventSyn, inputSynReport, 0}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("wire events=%v, want %v", events, want)
	}
}

func TestCoordinateAndNativeInputShareRelativeDevice(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	relative := NewRelativePointerInput(0x1000, 0x1000, 11)
	relative.Attach(mem, &testIRQ{})
	absolute := NewAbsolutePointerInput(0x5000, 0x1000, 12, 800, 600)
	framebuffer, err := NewFramebuffer(800, 600)
	if err != nil {
		t.Fatal(err)
	}
	desktop := &Desktop{Framebuffer: framebuffer, Pointer: absolute, RelativePointer: relative, GPU: NewGPU(0x6000, 0x1000, 13, framebuffer)}
	desktop.GPU.Cursor().Move(100, 200)
	desktop.SetRelativePointerMode(true)
	if err := desktop.SendPointer(110, 190, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := desktop.SendRelativePointer(1, 2, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := desktop.SendPointer(110, 190, 0, 1); err != nil {
		t.Fatal(err)
	}
	q := &relative.queues[inputQueueEvent]
	q.size = 16
	q.ready = true
	q.descAddr = 0x2000
	q.availAddr = 0x3000
	q.usedAddr = 0x3800
	for idx := uint16(0); idx < q.size; idx++ {
		writeDesc(mem, q.descAddr+uint64(idx)*16, 0x4000+uint64(idx)*8, 8, descFWrite, 0)
		binary.LittleEndian.PutUint16(mem[q.availAddr+4+uint64(idx)*2:], idx)
	}
	binary.LittleEndian.PutUint16(mem[q.availAddr+2:], q.size)
	if err := relative.Send(); err != nil {
		t.Fatal(err)
	}
	var events []InputEvent
	for idx := uint16(0); idx < q.usedIdx; idx++ {
		raw := mem[0x4000+uint64(idx)*8:]
		events = append(events, InputEvent{binary.LittleEndian.Uint16(raw), binary.LittleEndian.Uint16(raw[2:]), int32(binary.LittleEndian.Uint32(raw[4:]))})
	}
	want := []InputEvent{{inputEventRel, inputRelX, 10}, {inputEventRel, inputRelY, -10}, {inputEventKey, inputBtnLeft, 1}, {inputEventSyn, inputSynReport, 0}, {inputEventRel, inputRelX, 1}, {inputEventRel, inputRelY, 2}, {inputEventSyn, inputSynReport, 0}, {inputEventRel, inputRelX, -1}, {inputEventRel, inputRelY, -2}, {inputEventKey, inputBtnLeft, 0}, {inputEventSyn, inputSynReport, 0}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("wire input=%v, want %v", events, want)
	}
}
