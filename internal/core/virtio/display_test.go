package virtio

import (
	"bytes"
	"encoding/binary"
	"image"
	"reflect"
	"testing"
)

func TestGPU2DScanoutCopiesGuestBacking(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	framebuffer, err := NewFramebuffer(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	gpu := NewGPU(0x1000, 0x1000, 9, framebuffer)
	gpu.Attach(mem, &testIRQ{})

	create := gpuTestRequest(gpuCmdResourceCreate2D, 40)
	binary.LittleEndian.PutUint32(create[24:28], 7)
	binary.LittleEndian.PutUint32(create[28:32], gpuFormatB8G8R8X8)
	binary.LittleEndian.PutUint32(create[32:36], 2)
	binary.LittleEndian.PutUint32(create[36:40], 2)
	requireGPUResponse(t, gpu.dispatchLocked(create, gpuQueueControl), gpuRespOKNoData)

	want := []byte{
		1, 2, 3, 0, 4, 5, 6, 0,
		7, 8, 9, 0, 10, 11, 12, 0,
	}
	copy(mem[0x4000:], want)
	attach := gpuTestRequest(gpuCmdResourceAttachBacking, 48)
	binary.LittleEndian.PutUint32(attach[24:28], 7)
	binary.LittleEndian.PutUint32(attach[28:32], 1)
	binary.LittleEndian.PutUint64(attach[32:40], 0x4000)
	binary.LittleEndian.PutUint32(attach[40:44], uint32(len(want)))
	requireGPUResponse(t, gpu.dispatchLocked(attach, gpuQueueControl), gpuRespOKNoData)

	transfer := gpuTestRequest(gpuCmdTransferToHost2D, 56)
	putGPUTestRect(transfer[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(transfer[48:52], 7)
	requireGPUResponse(t, gpu.dispatchLocked(transfer, gpuQueueControl), gpuRespOKNoData)

	scanout := gpuTestRequest(gpuCmdSetScanout, 48)
	putGPUTestRect(scanout[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(scanout[44:48], 7)
	requireGPUResponse(t, gpu.dispatchLocked(scanout, gpuQueueControl), gpuRespOKNoData)

	flush := gpuTestRequest(gpuCmdResourceFlush, 48)
	putGPUTestRect(flush[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(flush[40:44], 7)
	requireGPUResponse(t, gpu.dispatchLocked(flush, gpuQueueControl), gpuRespOKNoData)

	update := framebuffer.Snapshot(image.Rect(0, 0, 2, 2), 0, false)
	if !bytes.Equal(update.Pixels, want) {
		t.Fatalf("scanout pixels = %v, want %v", update.Pixels, want)
	}
}

func TestGPUScanoutUsesGuestModeAfterHostWindowResize(t *testing.T) {
	framebuffer, err := NewFramebuffer(1440, 900)
	if err != nil {
		t.Fatal(err)
	}
	gpu := NewGPU(0x1000, 0x1000, 9, framebuffer)

	create := gpuTestRequest(gpuCmdResourceCreate2D, 40)
	binary.LittleEndian.PutUint32(create[24:28], 7)
	binary.LittleEndian.PutUint32(create[28:32], gpuFormatB8G8R8X8)
	binary.LittleEndian.PutUint32(create[32:36], 1440)
	binary.LittleEndian.PutUint32(create[36:40], 900)
	requireGPUResponse(t, gpu.dispatchLocked(create, gpuQueueControl), gpuRespOKNoData)

	if err := gpu.Resize(1440, 891); err != nil {
		t.Fatal(err)
	}
	scanout := gpuTestRequest(gpuCmdSetScanout, 48)
	putGPUTestRect(scanout[24:40], 0, 0, 1440, 900)
	binary.LittleEndian.PutUint32(scanout[44:48], 7)
	requireGPUResponse(t, gpu.dispatchLocked(scanout, gpuQueueControl), gpuRespOKNoData)
	if width, height := framebuffer.Size(); width != 1440 || height != 900 {
		t.Fatalf("guest scanout framebuffer = %dx%d, want 1440x900", width, height)
	}
}

func TestGPURejectsResourceLargerThanConfiguredDisplay(t *testing.T) {
	framebuffer, err := NewFramebuffer(800, 600)
	if err != nil {
		t.Fatal(err)
	}
	gpu := NewGPU(0x1000, 0x1000, 9, framebuffer)
	create := gpuTestRequest(gpuCmdResourceCreate2D, 40)
	binary.LittleEndian.PutUint32(create[24:28], 1)
	binary.LittleEndian.PutUint32(create[28:32], gpuFormatB8G8R8X8)
	binary.LittleEndian.PutUint32(create[32:36], 801)
	binary.LittleEndian.PutUint32(create[36:40], 600)
	requireGPUResponse(t, gpu.dispatchLocked(create, gpuQueueControl), gpuRespErrInvalidParameter)
}

func TestGPUResizePublishesDisplayEvent(t *testing.T) {
	framebuffer, err := NewFramebuffer(800, 600)
	if err != nil {
		t.Fatal(err)
	}
	irq := &testIRQ{}
	gpu := NewGPU(0x1000, 0x1000, 9, framebuffer)
	gpu.Attach(make(testGuestMemory, 4096), irq)
	if err := gpu.Resize(1024, 768); err != nil {
		t.Fatal(err)
	}
	if width, height := framebuffer.Size(); width != 1024 || height != 768 {
		t.Fatalf("resized framebuffer = %dx%d", width, height)
	}
	if gpu.eventsRead&1 == 0 {
		t.Fatal("GPU display-change event was not published")
	}
	if gpu.interruptStatus&intConfig == 0 || !irq.level {
		t.Fatalf("GPU resize interrupt = %#x level=%v", gpu.interruptStatus, irq.level)
	}
	display := gpuTestRequest(gpuCmdGetDisplayInfo, gpuControlHeaderSize)
	response := gpu.dispatchLocked(display, gpuQueueControl)
	requireGPUResponse(t, response, gpuRespOKDisplayInfo)
	if width := binary.LittleEndian.Uint32(response[gpuControlHeaderSize+8:]); width != 1024 {
		t.Fatalf("display width = %d", width)
	}
	if height := binary.LittleEndian.Uint32(response[gpuControlHeaderSize+12:]); height != 768 {
		t.Fatalf("display height = %d", height)
	}
}

func TestGPUPartialTransferUsesBackingOffsetForRectangleStart(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	framebuffer, err := NewFramebuffer(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	gpu := NewGPU(0x1000, 0x1000, 9, framebuffer)
	gpu.Attach(mem, &testIRQ{})

	create := gpuTestRequest(gpuCmdResourceCreate2D, 40)
	binary.LittleEndian.PutUint32(create[24:28], 7)
	binary.LittleEndian.PutUint32(create[28:32], gpuFormatB8G8R8X8)
	binary.LittleEndian.PutUint32(create[32:36], 3)
	binary.LittleEndian.PutUint32(create[36:40], 2)
	requireGPUResponse(t, gpu.dispatchLocked(create, gpuQueueControl), gpuRespOKNoData)

	backing := []byte{
		1, 2, 3, 0, 4, 5, 6, 0, 7, 8, 9, 0,
		10, 11, 12, 0, 13, 14, 15, 0, 16, 17, 18, 0,
	}
	copy(mem[0x4000:], backing)
	attach := gpuTestRequest(gpuCmdResourceAttachBacking, 48)
	binary.LittleEndian.PutUint32(attach[24:28], 7)
	binary.LittleEndian.PutUint32(attach[28:32], 1)
	binary.LittleEndian.PutUint64(attach[32:40], 0x4000)
	binary.LittleEndian.PutUint32(attach[40:44], uint32(len(backing)))
	requireGPUResponse(t, gpu.dispatchLocked(attach, gpuQueueControl), gpuRespOKNoData)

	transfer := gpuTestRequest(gpuCmdTransferToHost2D, 56)
	putGPUTestRect(transfer[24:40], 1, 1, 2, 1)
	binary.LittleEndian.PutUint64(transfer[40:48], 16)
	binary.LittleEndian.PutUint32(transfer[48:52], 7)
	requireGPUResponse(t, gpu.dispatchLocked(transfer, gpuQueueControl), gpuRespOKNoData)

	got := gpu.resources[7].pixels[16:24]
	want := backing[16:24]
	if !bytes.Equal(got, want) {
		t.Fatalf("partial transfer pixels = %v, want %v", got, want)
	}
}

func TestGPUCursorQueueDoesNotRequireResponseBuffer(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	framebuffer, err := NewFramebuffer(800, 600)
	if err != nil {
		t.Fatal(err)
	}
	gpu := NewGPU(0x1000, 0x1000, 9, framebuffer)
	gpu.Attach(mem, &testIRQ{})

	q := &gpu.queues[gpuQueueCursor]
	q.size = 1
	q.ready = true
	q.descAddr = 0x2000
	q.availAddr = 0x3000
	q.usedAddr = 0x3800
	request := gpuTestRequest(gpuCmdMoveCursor, 56)
	binary.LittleEndian.PutUint32(request[28:32], 321)
	binary.LittleEndian.PutUint32(request[32:36], 123)
	copy(mem[0x4000:], request)
	writeDesc(mem, q.descAddr, 0x4000, uint32(len(request)), 0, 0)
	binary.LittleEndian.PutUint16(mem[q.availAddr+2:], 1)

	if err := gpu.processQueueLocked(gpuQueueCursor); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(mem[q.usedAddr+2:]); got != 1 {
		t.Fatalf("used index = %d, want 1", got)
	}
	cursor := gpu.Cursor().Snapshot()
	if cursor.X != 321 || cursor.Y != 123 || cursor.PositionGeneration == 0 || cursor.Generation != 0 {
		t.Fatalf("cursor movement: %+v", cursor)
	}
	if got := binary.LittleEndian.Uint32(mem[q.usedAddr+8:]); got != 0 {
		t.Fatalf("used length = %d, want 0", got)
	}
}

func TestGPUCursorUpdatePublishesGuestShape(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	framebuffer, err := NewFramebuffer(800, 600)
	if err != nil {
		t.Fatal(err)
	}
	gpu := NewGPU(0x1000, 0x1000, 9, framebuffer)
	gpu.Attach(mem, &testIRQ{})

	create := gpuTestRequest(gpuCmdResourceCreate2D, 40)
	binary.LittleEndian.PutUint32(create[24:28], 7)
	binary.LittleEndian.PutUint32(create[28:32], gpuFormatB8G8R8A8)
	binary.LittleEndian.PutUint32(create[32:36], 2)
	binary.LittleEndian.PutUint32(create[36:40], 2)
	requireGPUResponse(t, gpu.dispatchLocked(create, gpuQueueControl), gpuRespOKNoData)

	pixels := []byte{
		0, 0, 0, 0, 10, 20, 30, 255,
		40, 50, 60, 128, 70, 80, 90, 255,
	}
	copy(mem[0x4000:], pixels)
	attach := gpuTestRequest(gpuCmdResourceAttachBacking, 48)
	binary.LittleEndian.PutUint32(attach[24:28], 7)
	binary.LittleEndian.PutUint32(attach[28:32], 1)
	binary.LittleEndian.PutUint64(attach[32:40], 0x4000)
	binary.LittleEndian.PutUint32(attach[40:44], uint32(len(pixels)))
	requireGPUResponse(t, gpu.dispatchLocked(attach, gpuQueueControl), gpuRespOKNoData)

	transfer := gpuTestRequest(gpuCmdTransferToHost2D, 56)
	putGPUTestRect(transfer[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(transfer[48:52], 7)
	requireGPUResponse(t, gpu.dispatchLocked(transfer, gpuQueueControl), gpuRespOKNoData)

	update := gpuTestRequest(gpuCmdUpdateCursor, 56)
	binary.LittleEndian.PutUint32(update[40:44], 7)
	binary.LittleEndian.PutUint32(update[44:48], 1)
	requireGPUResponse(t, gpu.dispatchLocked(update, gpuQueueCursor), gpuRespOKNoData)

	cursor := gpu.Cursor().Snapshot()
	if !cursor.Visible || cursor.Width != 2 || cursor.Height != 2 || cursor.HotX != 1 || cursor.HotY != 0 {
		t.Fatalf("cursor metadata = %+v", cursor)
	}
	if !bytes.Equal(cursor.Pixels, pixels) {
		t.Fatalf("cursor pixels = %v, want %v", cursor.Pixels, pixels)
	}

	hide := gpuTestRequest(gpuCmdUpdateCursor, 56)
	requireGPUResponse(t, gpu.dispatchLocked(hide, gpuQueueCursor), gpuRespOKNoData)
	if cursor := gpu.Cursor().Snapshot(); cursor.Visible {
		t.Fatal("zero-resource cursor update did not hide the cursor")
	}
}

func TestInputDeliversOrderedEventsToPostedBuffers(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	irq := &testIRQ{}
	input := NewKeyboardInput(0x1000, 0x1000, 11)
	input.Attach(mem, irq)

	q := &input.queues[inputQueueEvent]
	q.size = 2
	q.ready = true
	q.descAddr = 0x2000
	q.availAddr = 0x3000
	q.usedAddr = 0x3800
	writeDesc(mem, q.descAddr, 0x4000, 8, descFWrite, 0)
	writeDesc(mem, q.descAddr+16, 0x4010, 8, descFWrite, 0)
	binary.LittleEndian.PutUint16(mem[q.availAddr+2:], 2)
	binary.LittleEndian.PutUint16(mem[q.availAddr+4:], 0)
	binary.LittleEndian.PutUint16(mem[q.availAddr+6:], 1)

	if err := input.Key(30, true); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(mem[0x4000:]); got != inputEventKey {
		t.Fatalf("first event type = %d", got)
	}
	if got := binary.LittleEndian.Uint16(mem[0x4002:]); got != 30 {
		t.Fatalf("first event code = %d", got)
	}
	if got := binary.LittleEndian.Uint32(mem[0x4004:]); got != 1 {
		t.Fatalf("first event value = %d", got)
	}
	if got := binary.LittleEndian.Uint16(mem[0x4010:]); got != inputEventSyn {
		t.Fatalf("second event type = %d", got)
	}
	if got := binary.LittleEndian.Uint16(mem[q.usedAddr+2:]); got != 2 {
		t.Fatalf("used index = %d", got)
	}
	if !irq.level || irq.line != 11 {
		t.Fatalf("IRQ = line %d level %v", irq.line, irq.level)
	}
}

func TestAbsolutePointerReportsMouseWheel(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	input := NewAbsolutePointerInput(0x1000, 0x1000, 11, 800, 600)
	input.Attach(mem, &testIRQ{})

	eventTypes := input.eventBitmapLocked(0)
	if eventTypes[inputEventRel/8]&(1<<(inputEventRel%8)) == 0 {
		t.Fatal("pointer does not advertise relative events")
	}
	relativeEvents := input.eventBitmapLocked(inputEventRel)
	if relativeEvents[inputRelWheel/8]&(1<<(inputRelWheel%8)) == 0 {
		t.Fatal("pointer does not advertise a vertical wheel")
	}

	q := &input.queues[inputQueueEvent]
	q.size = 16
	q.ready = true
	q.descAddr = 0x2000
	q.availAddr = 0x3000
	q.usedAddr = 0x3800
	for index := uint16(0); index < q.size; index++ {
		writeDesc(mem, q.descAddr+uint64(index)*16, 0x4000+uint64(index)*8, 8, descFWrite, 0)
		binary.LittleEndian.PutUint16(mem[q.availAddr+4+uint64(index)*2:], index)
	}
	binary.LittleEndian.PutUint16(mem[q.availAddr+2:], q.size)

	// RFB button four is one wheel-up step. The matching release must not
	// generate a second step.
	if err := input.PointerEvent(400, 300, 8, 0); err != nil {
		t.Fatal(err)
	}
	if err := input.PointerEvent(400, 300, 0, 8); err != nil {
		t.Fatal(err)
	}
	if err := input.PointerEvent(400, 300, 16, 0); err != nil {
		t.Fatal(err)
	}
	if err := input.PointerEvent(400, 300, 0, 16); err != nil {
		t.Fatal(err)
	}

	var wheelEvents []int32
	for index := uint16(0); index < q.usedIdx; index++ {
		event := mem[0x4000+uint64(index)*8:]
		if binary.LittleEndian.Uint16(event) == inputEventRel &&
			binary.LittleEndian.Uint16(event[2:]) == inputRelWheel {
			wheelEvents = append(wheelEvents, int32(binary.LittleEndian.Uint32(event[4:])))
		}
	}
	if len(wheelEvents) != 2 || wheelEvents[0] != 1 || wheelEvents[1] != -1 {
		t.Fatalf("wheel events = %v, want [1 -1]", wheelEvents)
	}
}

func TestAbsolutePointerReportsHighResolutionScroll(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	input := NewAbsolutePointerInput(0x1000, 0x1000, 11, 800, 600)
	input.Attach(mem, &testIRQ{})

	relativeEvents := input.eventBitmapLocked(inputEventRel)
	for _, code := range []uint16{inputRelHWheel, inputRelWheel, inputRelWheelHiRes, inputRelHWheelHiRes} {
		if relativeEvents[code/8]&(1<<(code%8)) == 0 {
			t.Fatalf("pointer does not advertise relative event %d", code)
		}
	}

	q := &input.queues[inputQueueEvent]
	q.size = 16
	q.ready = true
	q.descAddr = 0x2000
	q.availAddr = 0x3000
	q.usedAddr = 0x3800
	for index := uint16(0); index < q.size; index++ {
		writeDesc(mem, q.descAddr+uint64(index)*16, 0x4000+uint64(index)*8, 8, descFWrite, 0)
		binary.LittleEndian.PutUint16(mem[q.availAddr+4+uint64(index)*2:], index)
	}
	binary.LittleEndian.PutUint16(mem[q.availAddr+2:], q.size)

	if err := input.ScrollEvent(30, 40); err != nil {
		t.Fatal(err)
	}
	if err := input.ScrollEvent(90, 80); err != nil {
		t.Fatal(err)
	}

	var events []InputEvent
	for index := uint16(0); index < q.usedIdx; index++ {
		raw := mem[0x4000+uint64(index)*8:]
		events = append(events, InputEvent{
			Type:  binary.LittleEndian.Uint16(raw),
			Code:  binary.LittleEndian.Uint16(raw[2:]),
			Value: int32(binary.LittleEndian.Uint32(raw[4:])),
		})
	}
	want := []InputEvent{
		{Type: inputEventRel, Code: inputRelHWheelHiRes, Value: 30},
		{Type: inputEventRel, Code: inputRelWheelHiRes, Value: 40},
		{Type: inputEventSyn, Code: inputSynReport},
		{Type: inputEventRel, Code: inputRelHWheelHiRes, Value: 90},
		{Type: inputEventRel, Code: inputRelHWheel, Value: 1},
		{Type: inputEventRel, Code: inputRelWheelHiRes, Value: 80},
		{Type: inputEventRel, Code: inputRelWheel, Value: 1},
		{Type: inputEventSyn, Code: inputSynReport},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestFramebufferIncrementalSnapshot(t *testing.T) {
	framebuffer, err := NewFramebuffer(4, 3)
	if err != nil {
		t.Fatal(err)
	}
	initial := framebuffer.Snapshot(image.Rect(0, 0, 4, 3), 0, false)
	pixel := []byte{1, 2, 3, 0}
	if err := framebuffer.Update(image.Rect(2, 1, 3, 2), pixel, 4); err != nil {
		t.Fatal(err)
	}
	update := framebuffer.Snapshot(image.Rect(0, 0, 4, 3), initial.Generation, true)
	if update.Rect != image.Rect(2, 1, 3, 2) {
		t.Fatalf("dirty rect = %v", update.Rect)
	}
	if !bytes.Equal(update.Pixels, pixel) {
		t.Fatalf("dirty pixels = %v", update.Pixels)
	}
	current := framebuffer.Snapshot(image.Rect(0, 0, 4, 3), update.Generation, true)
	if !current.Rect.Empty() || len(current.Pixels) != 0 {
		t.Fatalf("current incremental snapshot = %#v", current)
	}
}

func TestFramebufferPartialRequestRetainsUnsentDirtyRegion(t *testing.T) {
	framebuffer, err := NewFramebuffer(4, 1)
	if err != nil {
		t.Fatal(err)
	}
	initial := framebuffer.Snapshot(image.Rect(0, 0, 4, 1), 0, false)
	if err := framebuffer.Update(image.Rect(0, 0, 4, 1), make([]byte, 16), 16); err != nil {
		t.Fatal(err)
	}
	left := framebuffer.Snapshot(image.Rect(0, 0, 2, 1), initial.Generation, true)
	if left.Rect != image.Rect(0, 0, 2, 1) {
		t.Fatalf("left update = %v", left.Rect)
	}
	remaining := framebuffer.Snapshot(image.Rect(0, 0, 4, 1), left.Generation, true)
	if remaining.Rect != image.Rect(0, 0, 4, 1) {
		t.Fatalf("remaining update = %v, want retained dirty scanout", remaining.Rect)
	}
}

func gpuTestRequest(command uint32, size int) []byte {
	request := make([]byte, size)
	binary.LittleEndian.PutUint32(request[0:4], command)
	return request
}

func putGPUTestRect(dst []byte, x, y, width, height uint32) {
	binary.LittleEndian.PutUint32(dst[0:4], x)
	binary.LittleEndian.PutUint32(dst[4:8], y)
	binary.LittleEndian.PutUint32(dst[8:12], width)
	binary.LittleEndian.PutUint32(dst[12:16], height)
}

func requireGPUResponse(t *testing.T, response []byte, want uint32) {
	t.Helper()
	if len(response) < 4 {
		t.Fatalf("short GPU response: %d", len(response))
	}
	if got := binary.LittleEndian.Uint32(response[0:4]); got != want {
		t.Fatalf("GPU response = %#x, want %#x", got, want)
	}
}
