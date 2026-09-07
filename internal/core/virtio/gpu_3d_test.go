package virtio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"testing"
)

func TestGPU3DTransportCarriesMesaCommandsToRenderer(t *testing.T) {
	mem := make(testGuestMemory, 64<<10)
	framebuffer, err := NewFramebuffer(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	renderer := &recordingGPURenderer{
		capsets: []GPUCapset{{ID: 2, Version: 1, Data: []byte{9, 8, 7, 6}}},
		scanout: []byte{
			1, 2, 3, 0, 4, 5, 6, 0,
			7, 8, 9, 0, 10, 11, 12, 0,
		},
	}
	gpu := NewGPUWithRenderer(0x1000, 0x1000, 9, framebuffer, renderer)
	gpu.Attach(mem, &testIRQ{})

	gpu.deviceFeatureSel = 0
	features, err := gpu.Read(gpu.Base+regDeviceFeatures, 4)
	if err != nil {
		t.Fatal(err)
	}
	if features&1 == 0 {
		t.Fatal("renderer-backed GPU does not advertise VIRTIO_GPU_F_VIRGL")
	}
	config := gpu.configBytesLocked()
	if got := binary.LittleEndian.Uint32(config[12:16]); got != 1 {
		t.Fatalf("num_capsets = %d, want 1", got)
	}

	capsetInfo := gpuTestRequest(gpuCmdGetCapsetInfo, 32)
	response := gpu.dispatchLocked(capsetInfo, gpuQueueControl)
	requireGPUResponse(t, response, gpuRespOKCapsetInfo)
	if id := binary.LittleEndian.Uint32(response[24:28]); id != 2 {
		t.Fatalf("capset id = %d, want 2", id)
	}
	if size := binary.LittleEndian.Uint32(response[32:36]); size != 4 {
		t.Fatalf("capset size = %d, want 4", size)
	}
	getCapset := gpuTestRequest(gpuCmdGetCapset, 32)
	binary.LittleEndian.PutUint32(getCapset[24:28], 2)
	binary.LittleEndian.PutUint32(getCapset[28:32], 1)
	response = gpu.dispatchLocked(getCapset, gpuQueueControl)
	requireGPUResponse(t, response, gpuRespOKCapset)
	if !bytes.Equal(response[24:], renderer.capsets[0].Data) {
		t.Fatalf("capset payload = %v", response[24:])
	}

	createContext := gpuTestRequest(gpuCmdContextCreate, 96)
	binary.LittleEndian.PutUint32(createContext[16:20], 4)
	binary.LittleEndian.PutUint32(createContext[24:28], 4)
	copy(createContext[32:], "cube")
	requireGPUResponse(t, gpu.dispatchLocked(createContext, gpuQueueControl), gpuRespOKNoData)

	createResource := gpuTestRequest(gpuCmdResourceCreate3D, 72)
	binary.LittleEndian.PutUint32(createResource[24:28], 7)
	binary.LittleEndian.PutUint32(createResource[28:32], 2)
	binary.LittleEndian.PutUint32(createResource[32:36], gpuFormatB8G8R8X8)
	binary.LittleEndian.PutUint32(createResource[40:44], 2)
	binary.LittleEndian.PutUint32(createResource[44:48], 2)
	binary.LittleEndian.PutUint32(createResource[48:52], 1)
	binary.LittleEndian.PutUint32(createResource[52:56], 1)
	requireGPUResponse(t, gpu.dispatchLocked(createResource, gpuQueueControl), gpuRespOKNoData)

	attachResource := gpuTestRequest(gpuCmdContextAttachResource, 32)
	binary.LittleEndian.PutUint32(attachResource[16:20], 4)
	binary.LittleEndian.PutUint32(attachResource[24:28], 7)
	requireGPUResponse(t, gpu.dispatchLocked(attachResource, gpuQueueControl), gpuRespOKNoData)

	backingBytes := []byte{20, 21, 22, 23}
	copy(mem[0x4000:], backingBytes[:2])
	copy(mem[0x5000:], backingBytes[2:])
	attachBacking := gpuTestRequest(gpuCmdResourceAttachBacking, 64)
	binary.LittleEndian.PutUint32(attachBacking[24:28], 7)
	binary.LittleEndian.PutUint32(attachBacking[28:32], 2)
	binary.LittleEndian.PutUint64(attachBacking[32:40], 0x4000)
	binary.LittleEndian.PutUint32(attachBacking[40:44], 2)
	binary.LittleEndian.PutUint64(attachBacking[48:56], 0x5000)
	binary.LittleEndian.PutUint32(attachBacking[56:60], 2)
	requireGPUResponse(t, gpu.dispatchLocked(attachBacking, gpuQueueControl), gpuRespOKNoData)

	transfer := gpuTestRequest(gpuCmdTransferToHost3D, 72)
	binary.LittleEndian.PutUint32(transfer[16:20], 4)
	putGPUTestBox(transfer[24:48], 0, 0, 0, 1, 1, 1)
	binary.LittleEndian.PutUint32(transfer[56:60], 7)
	requireGPUResponse(t, gpu.dispatchLocked(transfer, gpuQueueControl), gpuRespOKNoData)

	submit := gpuTestRequest(gpuCmdSubmit3D, 40)
	binary.LittleEndian.PutUint32(submit[4:8], 1)
	binary.LittleEndian.PutUint64(submit[8:16], 99)
	binary.LittleEndian.PutUint32(submit[16:20], 4)
	binary.LittleEndian.PutUint32(submit[24:28], 8)
	copy(submit[32:], []byte{1, 0, 2, 0, 3, 0, 4, 0})
	response = gpu.dispatchLocked(submit, gpuQueueControl)
	requireGPUResponse(t, response, gpuRespOKNoData)
	if flags := binary.LittleEndian.Uint32(response[4:8]); flags&1 == 0 {
		t.Fatalf("fenced response flags = %#x", flags)
	}
	if fence := binary.LittleEndian.Uint64(response[8:16]); fence != 99 {
		t.Fatalf("fence id = %d, want 99", fence)
	}
	if !bytes.Equal(renderer.submitted, submit[32:]) {
		t.Fatalf("submitted commands = %v", renderer.submitted)
	}
	if !bytes.Equal(renderer.transferred, backingBytes) {
		t.Fatalf("transferred bytes = %v", renderer.transferred)
	}

	renderer.fromHost = []byte{31, 32, 33, 34}
	transferFromHost := append([]byte(nil), transfer...)
	binary.LittleEndian.PutUint32(transferFromHost[0:4], gpuCmdTransferFromHost3D)
	requireGPUResponse(t, gpu.dispatchLocked(transferFromHost, gpuQueueControl), gpuRespOKNoData)
	gotFromHost := append([]byte(nil), mem[0x4000:0x4002]...)
	gotFromHost = append(gotFromHost, mem[0x5000:0x5002]...)
	if !bytes.Equal(gotFromHost, renderer.fromHost) {
		t.Fatalf("guest backing after host transfer = %v, want %v", gotFromHost, renderer.fromHost)
	}

	setScanout := gpuTestRequest(gpuCmdSetScanout, 48)
	putGPUTestRect(setScanout[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(setScanout[44:48], 7)
	requireGPUResponse(t, gpu.dispatchLocked(setScanout, gpuQueueControl), gpuRespOKNoData)
	flush := gpuTestRequest(gpuCmdResourceFlush, 48)
	putGPUTestRect(flush[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(flush[40:44], 7)
	requireGPUResponse(t, gpu.dispatchLocked(flush, gpuQueueControl), gpuRespOKNoData)
	update := framebuffer.Snapshot(image.Rect(0, 0, 2, 2), 0, false)
	if !bytes.Equal(update.Pixels, renderer.scanout) {
		t.Fatalf("3D scanout pixels = %v, want %v", update.Pixels, renderer.scanout)
	}
}

func TestGPUNativeScanoutDefersReadbackUntilCPUConsumer(t *testing.T) {
	framebuffer, err := NewFramebuffer(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	pixels := []byte{
		1, 2, 3, 0, 4, 5, 6, 0,
		7, 8, 9, 0, 10, 11, 12, 0,
	}
	base := &recordingGPURenderer{scanout: pixels}
	renderer := &nativeRecordingGPURenderer{recordingGPURenderer: base}
	gpu := NewGPUWithRenderer(0x1000, 0x1000, 9, framebuffer, renderer)

	createResource := gpuTestRequest(gpuCmdResourceCreate3D, 72)
	binary.LittleEndian.PutUint32(createResource[24:28], 7)
	binary.LittleEndian.PutUint32(createResource[28:32], 2)
	binary.LittleEndian.PutUint32(createResource[32:36], gpuFormatB8G8R8X8)
	binary.LittleEndian.PutUint32(createResource[40:44], 2)
	binary.LittleEndian.PutUint32(createResource[44:48], 2)
	binary.LittleEndian.PutUint32(createResource[48:52], 1)
	binary.LittleEndian.PutUint32(createResource[52:56], 1)
	requireGPUResponse(t, gpu.dispatchLocked(createResource, gpuQueueControl), gpuRespOKNoData)

	setScanout := gpuTestRequest(gpuCmdSetScanout, 48)
	putGPUTestRect(setScanout[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(setScanout[44:48], 7)
	requireGPUResponse(t, gpu.dispatchLocked(setScanout, gpuQueueControl), gpuRespOKNoData)
	flush := gpuTestRequest(gpuCmdResourceFlush, 48)
	putGPUTestRect(flush[24:40], 0, 0, 2, 2)
	binary.LittleEndian.PutUint32(flush[40:44], 7)
	requireGPUResponse(t, gpu.dispatchLocked(flush, gpuQueueControl), gpuRespOKNoData)
	// A second guest frame can arrive before the frontend acquires the first.
	// The pending lease must be retired before the renderer's bounded native
	// frame pool is asked to publish its replacement.
	requireGPUResponse(t, gpu.dispatchLocked(flush, gpuQueueControl), gpuRespOKNoData)

	if base.readCalls != 0 {
		t.Fatalf("native flush performed %d CPU readbacks, want 0", base.readCalls)
	}
	if renderer.nativeCalls != 2 {
		t.Fatalf("native frame publications = %d, want 2", renderer.nativeCalls)
	}
	frame, available, err := gpu.AcquireNativeFrame(0)
	if err != nil {
		t.Fatal(err)
	}
	if !available || frame.Texture != 99 || frame.ProducerFence != 77 || frame.Generation == 0 {
		t.Fatalf("native frame = %+v, available=%t", frame, available)
	}
	frame.ReleaseFrame(55)
	if got, want := renderer.releasedFence, uintptr(55); got != want {
		t.Fatalf("released consumer fence = %d, want %d", got, want)
	}

	update, err := gpu.Snapshot(image.Rect(0, 0, 2, 2), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if base.readCalls != 1 {
		t.Fatalf("CPU snapshot readbacks = %d, want 1", base.readCalls)
	}
	if !bytes.Equal(update.Pixels, pixels) {
		t.Fatalf("CPU fallback pixels = %v, want %v", update.Pixels, pixels)
	}
}

func putGPUTestBox(dst []byte, x, y, z, width, height, depth uint32) {
	for index, value := range []uint32{x, y, z, width, height, depth} {
		binary.LittleEndian.PutUint32(dst[index*4:], value)
	}
}

type recordingGPURenderer struct {
	capsets     []GPUCapset
	submitted   []byte
	transferred []byte
	fromHost    []byte
	scanout     []byte
	contexts    map[uint32]struct{}
	resources   map[uint32]GPUResource3D
	readCalls   int
}

func (r *recordingGPURenderer) Capsets() []GPUCapset { return r.capsets }

func (r *recordingGPURenderer) CreateContext(id uint32, _ string, _ uint32) error {
	if r.contexts == nil {
		r.contexts = make(map[uint32]struct{})
	}
	r.contexts[id] = struct{}{}
	return nil
}

func (r *recordingGPURenderer) DestroyContext(id uint32) error {
	delete(r.contexts, id)
	return nil
}

func (r *recordingGPURenderer) CreateResource(resource GPUResource3D) error {
	if r.resources == nil {
		r.resources = make(map[uint32]GPUResource3D)
	}
	r.resources[resource.ID] = resource
	return nil
}

func (r *recordingGPURenderer) UnrefResource(id uint32) error {
	delete(r.resources, id)
	return nil
}

func (r *recordingGPURenderer) AttachResource(contextID, resourceID uint32) error {
	if _, ok := r.contexts[contextID]; !ok {
		return fmt.Errorf("missing context")
	}
	if _, ok := r.resources[resourceID]; !ok {
		return fmt.Errorf("missing resource")
	}
	return nil
}

func (r *recordingGPURenderer) DetachResource(uint32, uint32) error { return nil }

func (r *recordingGPURenderer) TransferToHost(transfer GPUTransfer3D) error {
	r.transferred = make([]byte, transfer.Backing.Size())
	return transfer.Backing.ReadAt(0, r.transferred)
}

func (r *recordingGPURenderer) TransferFromHost(transfer GPUTransfer3D) error {
	if r.fromHost == nil {
		return nil
	}
	return transfer.Backing.WriteAt(transfer.Offset, r.fromHost)
}

func (r *recordingGPURenderer) Submit(_ uint32, commands []byte) error {
	r.submitted = append([]byte(nil), commands...)
	return nil
}

func (r *recordingGPURenderer) ReadScanout(_ uint32, rect image.Rectangle) ([]byte, int, error) {
	r.readCalls++
	return append([]byte(nil), r.scanout...), rect.Dx() * 4, nil
}

func (r *recordingGPURenderer) Reset() {
	r.contexts = make(map[uint32]struct{})
	r.resources = make(map[uint32]GPUResource3D)
}

func (r *recordingGPURenderer) Close() error { return nil }

type nativeRecordingGPURenderer struct {
	*recordingGPURenderer
	releasedFence uintptr
	nativeCalls   int
	outstanding   bool
}

func (r *nativeRecordingGPURenderer) NativeScanout(_ uint32, rect image.Rectangle) (GPUNativeFrame, bool, error) {
	r.nativeCalls++
	if r.outstanding {
		return GPUNativeFrame{}, false, nil
	}
	r.outstanding = true
	return GPUNativeFrame{
		Width:         rect.Dx(),
		Height:        rect.Dy(),
		Texture:       99,
		ProducerFence: 77,
		ReleaseFrame: func(consumerFence uintptr) {
			r.releasedFence = consumerFence
			r.outstanding = false
		},
	}, true, nil
}
