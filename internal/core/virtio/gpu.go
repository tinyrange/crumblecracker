package virtio

import (
	"encoding/binary"
	"fmt"
	"image"
	"log"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	mmioDeviceIDGPU = 16

	gpuQueueControl = 0
	gpuQueueCursor  = 1

	gpuCmdGetDisplayInfo        = 0x0100
	gpuCmdResourceCreate2D      = 0x0101
	gpuCmdResourceUnref         = 0x0102
	gpuCmdSetScanout            = 0x0103
	gpuCmdResourceFlush         = 0x0104
	gpuCmdTransferToHost2D      = 0x0105
	gpuCmdResourceAttachBacking = 0x0106
	gpuCmdResourceDetachBacking = 0x0107
	gpuCmdGetCapsetInfo         = 0x0108
	gpuCmdGetCapset             = 0x0109
	gpuCmdContextCreate         = 0x0200
	gpuCmdContextDestroy        = 0x0201
	gpuCmdContextAttachResource = 0x0202
	gpuCmdContextDetachResource = 0x0203
	gpuCmdResourceCreate3D      = 0x0204
	gpuCmdTransferToHost3D      = 0x0205
	gpuCmdTransferFromHost3D    = 0x0206
	gpuCmdSubmit3D              = 0x0207
	gpuCmdUpdateCursor          = 0x0300
	gpuCmdMoveCursor            = 0x0301

	gpuRespOKNoData             = 0x1100
	gpuRespOKDisplayInfo        = 0x1101
	gpuRespOKCapsetInfo         = 0x1102
	gpuRespOKCapset             = 0x1103
	gpuRespErrUnspecified       = 0x1200
	gpuRespErrOutOfMemory       = 0x1201
	gpuRespErrInvalidScanoutID  = 0x1202
	gpuRespErrInvalidResourceID = 0x1203
	gpuRespErrInvalidContextID  = 0x1204
	gpuRespErrInvalidParameter  = 0x1205

	gpuFormatB8G8R8A8 = 1
	gpuFormatB8G8R8X8 = 2
	gpuFormatA8R8G8B8 = 3
	gpuFormatX8R8G8B8 = 4

	gpuControlHeaderSize = 24
	gpuMaxScanouts       = 16
)

type gpuMemoryEntry struct {
	addr   uint64
	length uint32
}

type gpuResource struct {
	id         uint32
	format     uint32
	width      uint32
	height     uint32
	pixels     []byte
	backing    []gpuMemoryEntry
	resource3D *GPUResource3D
}

type gpuContext struct {
	id        uint32
	resources map[uint32]struct{}
}

type GPU struct {
	Base uint64
	Size uint64
	IRQ  uint32

	mu               sync.Mutex
	mem              GuestMemory
	irq              IRQController
	deviceFeatureSel uint32
	driverFeatureSel uint32
	driverFeatures   uint64
	queueSel         uint32
	sharedMemorySel  uint32
	status           uint32
	interruptStatus  uint32
	irqHigh          bool
	configGeneration uint32
	eventsRead       uint32
	queues           [2]queue
	resources        map[uint32]*gpuResource
	contexts         map[uint32]*gpuContext
	scanoutResource  uint32
	scanoutRect      image.Rectangle
	framebuffer      *Framebuffer
	cursor           *Cursor
	cursorResource   uint32
	renderer         GPURenderer
	rendererErrors   map[string]struct{}
	nativeFrame      *GPUNativeFrame
	nativeGeneration uint64
	cpuGeneration    uint64
}

func NewGPU(base, size uint64, irq uint32, framebuffer *Framebuffer) *GPU {
	return NewGPUWithRenderer(base, size, irq, framebuffer, nil)
}

func NewGPUWithRenderer(base, size uint64, irq uint32, framebuffer *Framebuffer, renderer GPURenderer) *GPU {
	g := &GPU{
		Base:           base,
		Size:           size,
		IRQ:            irq,
		framebuffer:    framebuffer,
		cursor:         &Cursor{},
		renderer:       renderer,
		rendererErrors: make(map[string]struct{}),
	}
	g.resetLocked()
	return g
}

func (g *GPU) Framebuffer() *Framebuffer {
	return g.framebuffer
}

func (g *GPU) Cursor() *Cursor {
	return g.cursor
}

func (g *GPU) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseNativeFrameLocked(0)
	if g.renderer == nil {
		return nil
	}
	return g.renderer.Close()
}

func (g *GPU) Resize(width, height int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.framebuffer == nil {
		return fmt.Errorf("GPU framebuffer is unavailable")
	}
	currentWidth, currentHeight := g.framebuffer.Size()
	if width == currentWidth && height == currentHeight {
		return nil
	}
	if err := g.framebuffer.Resize(width, height); err != nil {
		return err
	}
	g.releaseNativeFrameLocked(0)
	g.scanoutResource = 0
	g.scanoutRect = image.Rectangle{}
	g.eventsRead |= 1 // VIRTIO_GPU_EVENT_DISPLAY
	g.configGeneration++
	g.interruptStatus |= intConfig
	return g.updateIRQLocked()
}

func (g *GPU) Attach(mem GuestMemory, irq IRQController) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mem = mem
	g.irq = irq
}

func (g *GPU) IRQAsserted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.irqHigh
}

func (g *GPU) Contains(addr uint64, size int) bool {
	if size <= 0 || addr < g.Base {
		return false
	}
	end := addr + uint64(size)
	return end >= addr && end <= g.Base+g.Size
}

func (g *GPU) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("virtio@%x", g.Base),
		Properties: map[string]fdt.Property{
			"compatible":   {Strings: []string{"virtio,mmio"}},
			"dma-coherent": {Flag: true},
			"reg":          {U64: []uint64{g.Base, g.Size}},
			"interrupts":   {U32: []uint32{0, g.IRQ, 4}},
			"status":       {Strings: []string{"okay"}},
		},
	}
}

func (g *GPU) Read(addr uint64, size int) (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	offset := addr - g.Base
	switch offset {
	case regMagicValue:
		return truncateValue(mmioMagicValue, size), nil
	case regVersion:
		return truncateValue(mmioVersion, size), nil
	case regDeviceID:
		return truncateValue(mmioDeviceIDGPU, size), nil
	case regVendorID:
		return truncateValue(mmioVendorID, size), nil
	case regDeviceFeatures:
		if g.deviceFeatureSel == 1 {
			return truncateValue(1, size), nil
		}
		if g.renderer != nil {
			return truncateValue(1, size), nil // VIRTIO_GPU_F_VIRGL
		}
		return 0, nil
	case regQueueNumMax:
		if g.selectedQueueLocked() != nil {
			return truncateValue(256, size), nil
		}
		return 0, nil
	case regQueueNum:
		if q := g.selectedQueueLocked(); q != nil {
			return truncateValue(uint64(q.size), size), nil
		}
		return 0, nil
	case regQueueReady:
		if q := g.selectedQueueLocked(); q != nil && q.ready {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regInterruptStatus:
		return truncateValue(uint64(g.interruptStatus), size), nil
	case regStatus:
		return truncateValue(uint64(g.status), size), nil
	case regConfigGen:
		return truncateValue(uint64(g.configGeneration), size), nil
	case regSharedMemoryLenLow, regSharedMemoryLenHigh:
		// A length of all ones reports that the selected shared-memory region
		// does not exist. Returning zero makes Linux try to reserve GPA zero.
		return truncateValue(uint64(^uint32(0)), size), nil
	case regSharedMemoryBaseLow, regSharedMemoryBaseHigh:
		return truncateValue(uint64(^uint32(0)), size), nil
	default:
		if offset >= regConfig && offset+uint64(size) <= regConfig+16 {
			cfg := g.configBytesLocked()
			return truncateValue(readConfigValue(cfg[offset-regConfig:], size), size), nil
		}
		return 0, nil
	}
}

func (g *GPU) Write(addr uint64, size int, value uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	offset := addr - g.Base
	switch offset {
	case regDeviceFeatSel:
		g.deviceFeatureSel = uint32(value)
	case regDriverFeatSel:
		g.driverFeatureSel = uint32(value)
	case regDriverFeatures:
		if g.driverFeatureSel == 0 {
			g.driverFeatures = (g.driverFeatures &^ 0xffffffff) | uint64(uint32(value))
		} else if g.driverFeatureSel == 1 {
			g.driverFeatures = (g.driverFeatures & 0xffffffff) | uint64(uint32(value))<<32
		}
	case regQueueSel:
		g.queueSel = uint32(value)
	case regSharedMemorySel:
		g.sharedMemorySel = uint32(value)
	case regQueueNum:
		if q := g.selectedQueueLocked(); q != nil {
			if value <= 256 {
				q.size = uint16(value)
			} else {
				q.size = 0
			}
		}
	case regQueueReady:
		if q := g.selectedQueueLocked(); q != nil {
			q.ready = value != 0
			if value == 0 {
				q.lastAvailIdx = 0
				q.usedIdx = 0
			}
		}
	case regQueueDescLow:
		if q := g.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrDesc, true)
		}
	case regQueueDescHigh:
		if q := g.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrDesc, false)
		}
	case regQueueAvailLow:
		if q := g.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrAvail, true)
		}
	case regQueueAvailHigh:
		if q := g.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrAvail, false)
		}
	case regQueueUsedLow:
		if q := g.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrUsed, true)
		}
	case regQueueUsedHigh:
		if q := g.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrUsed, false)
		}
	case regInterruptAck:
		g.interruptStatus &^= uint32(value)
		return g.updateIRQLocked()
	case regStatus:
		g.status = uint32(value)
		if g.status == 0 {
			g.resetLocked()
		}
	case regQueueNotify:
		if int(value) == gpuQueueControl || int(value) == gpuQueueCursor {
			return g.processQueueLocked(int(value))
		}
	default:
		if offset >= regConfig+4 && offset+uint64(size) <= regConfig+8 {
			clear := writeConfigUint32(0, offset-(regConfig+4), size, value)
			g.eventsRead &^= clear
		}
	}
	return nil
}

func (g *GPU) processQueueLocked(queueIndex int) error {
	q := &g.queues[queueIndex]
	if !q.ready || q.size == 0 || g.mem == nil {
		return nil
	}
	availIdx, err := readAvailableIndex(g.mem, q)
	if err != nil {
		return err
	}
	processed := false
	for q.lastAvailIdx != availIdx {
		head, err := readAvailableHead(g.mem, q, q.lastAvailIdx)
		if err != nil {
			return err
		}
		buffers, err := readQueueChain(g.mem, q, head)
		if err != nil {
			return err
		}
		request, err := readQueueRequest(g.mem, buffers)
		if err != nil {
			return err
		}
		response := g.dispatchLocked(request, queueIndex)
		var written uint32
		if queueIndex == gpuQueueControl {
			written, err = writeQueueResponse(g.mem, buffers, response)
			if err != nil {
				return err
			}
		}
		if err := writeQueueUsed(g.mem, q, head, written); err != nil {
			return err
		}
		q.lastAvailIdx++
		processed = true
	}
	if processed {
		g.interruptStatus |= intVring
		return g.updateIRQLocked()
	}
	return nil
}

func (g *GPU) dispatchLocked(request []byte, queueIndex int) []byte {
	if len(request) < gpuControlHeaderSize {
		return gpuResponse(request, gpuRespErrInvalidParameter, nil)
	}
	command := binary.LittleEndian.Uint32(request[0:4])
	if queueIndex == gpuQueueCursor && command != gpuCmdUpdateCursor && command != gpuCmdMoveCursor {
		return gpuResponse(request, gpuRespErrInvalidParameter, nil)
	}

	switch command {
	case gpuCmdGetDisplayInfo:
		width, height := g.framebuffer.Size()
		modes := make([]byte, gpuMaxScanouts*24)
		binary.LittleEndian.PutUint32(modes[8:12], uint32(width))
		binary.LittleEndian.PutUint32(modes[12:16], uint32(height))
		binary.LittleEndian.PutUint32(modes[16:20], 1)
		return gpuResponse(request, gpuRespOKDisplayInfo, modes)

	case gpuCmdResourceCreate2D:
		if len(request) < 40 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		id := binary.LittleEndian.Uint32(request[24:28])
		format := binary.LittleEndian.Uint32(request[28:32])
		width := binary.LittleEndian.Uint32(request[32:36])
		height := binary.LittleEndian.Uint32(request[36:40])
		if id == 0 || !supportedGPUFormat(format) || width == 0 || height == 0 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		if g.resources[id] != nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		fbWidth, fbHeight := g.framebuffer.Size()
		if width > uint32(fbWidth) || height > uint32(fbHeight) {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		size, ok := checkedPixelBytes(int(width), int(height))
		if !ok {
			return gpuResponse(request, gpuRespErrOutOfMemory, nil)
		}
		g.resources[id] = &gpuResource{
			id:     id,
			format: format,
			width:  width,
			height: height,
			pixels: make([]byte, size),
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdResourceUnref:
		if len(request) < 32 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		id := binary.LittleEndian.Uint32(request[24:28])
		if g.resources[id] == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		resource := g.resources[id]
		if resource.resource3D != nil {
			if err := g.renderer.UnrefResource(id); err != nil {
				return gpuResponse(request, gpuRespErrUnspecified, nil)
			}
			for _, context := range g.contexts {
				delete(context.resources, id)
			}
		}
		delete(g.resources, id)
		if g.cursorResource == id {
			g.cursorResource = 0
			g.cursor.Hide()
		}
		if g.scanoutResource == id {
			g.releaseNativeFrameLocked(0)
			g.scanoutResource = 0
			g.scanoutRect = image.Rectangle{}
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdResourceAttachBacking:
		if len(request) < 32 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		id := binary.LittleEndian.Uint32(request[24:28])
		count := binary.LittleEndian.Uint32(request[28:32])
		resource := g.resources[id]
		if resource == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		if uint64(count)*16 > uint64(len(request)-32) {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		entries := make([]gpuMemoryEntry, count)
		var total uint64
		for index := range entries {
			offset := 32 + index*16
			entries[index] = gpuMemoryEntry{
				addr:   binary.LittleEndian.Uint64(request[offset : offset+8]),
				length: binary.LittleEndian.Uint32(request[offset+8 : offset+12]),
			}
			total += uint64(entries[index].length)
			if total < uint64(entries[index].length) {
				return gpuResponse(request, gpuRespErrInvalidParameter, nil)
			}
		}
		resource.backing = entries
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdResourceDetachBacking:
		if len(request) < 32 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		id := binary.LittleEndian.Uint32(request[24:28])
		resource := g.resources[id]
		if resource == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		resource.backing = nil
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdGetCapsetInfo:
		if g.renderer == nil || len(request) < 32 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		index := binary.LittleEndian.Uint32(request[24:28])
		capsets := g.renderer.Capsets()
		if index >= uint32(len(capsets)) {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		capset := capsets[index]
		info := make([]byte, 16)
		binary.LittleEndian.PutUint32(info[0:4], capset.ID)
		binary.LittleEndian.PutUint32(info[4:8], capset.Version)
		binary.LittleEndian.PutUint32(info[8:12], uint32(len(capset.Data)))
		return gpuResponse(request, gpuRespOKCapsetInfo, info)

	case gpuCmdGetCapset:
		if g.renderer == nil || len(request) < 32 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		id := binary.LittleEndian.Uint32(request[24:28])
		version := binary.LittleEndian.Uint32(request[28:32])
		for _, capset := range g.renderer.Capsets() {
			if capset.ID == id && version <= capset.Version {
				return gpuResponse(request, gpuRespOKCapset, append([]byte(nil), capset.Data...))
			}
		}
		return gpuResponse(request, gpuRespErrInvalidParameter, nil)

	case gpuCmdContextCreate:
		if g.renderer == nil || len(request) < 96 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		id := binary.LittleEndian.Uint32(request[16:20])
		nameLength := binary.LittleEndian.Uint32(request[24:28])
		contextInit := binary.LittleEndian.Uint32(request[28:32])
		if id == 0 || nameLength > 64 || g.contexts[id] != nil {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		if err := g.renderer.CreateContext(id, string(request[32:32+nameLength]), contextInit); err != nil {
			return gpuResponse(request, gpuRespErrUnspecified, nil)
		}
		g.contexts[id] = &gpuContext{id: id, resources: make(map[uint32]struct{})}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdContextDestroy:
		if g.renderer == nil {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		id := binary.LittleEndian.Uint32(request[16:20])
		if g.contexts[id] == nil {
			return gpuResponse(request, gpuRespErrInvalidContextID, nil)
		}
		if err := g.renderer.DestroyContext(id); err != nil {
			return gpuResponse(request, gpuRespErrUnspecified, nil)
		}
		delete(g.contexts, id)
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdContextAttachResource, gpuCmdContextDetachResource:
		if g.renderer == nil || len(request) < 32 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		contextID := binary.LittleEndian.Uint32(request[16:20])
		resourceID := binary.LittleEndian.Uint32(request[24:28])
		context := g.contexts[contextID]
		resource := g.resources[resourceID]
		if context == nil {
			return gpuResponse(request, gpuRespErrInvalidContextID, nil)
		}
		if resource == nil || resource.resource3D == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		if command == gpuCmdContextAttachResource {
			if err := g.renderer.AttachResource(contextID, resourceID); err != nil {
				return gpuResponse(request, gpuRespErrUnspecified, nil)
			}
			context.resources[resourceID] = struct{}{}
		} else {
			if _, ok := context.resources[resourceID]; !ok {
				return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
			}
			if err := g.renderer.DetachResource(contextID, resourceID); err != nil {
				return gpuResponse(request, gpuRespErrUnspecified, nil)
			}
			delete(context.resources, resourceID)
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdResourceCreate3D:
		if g.renderer == nil || len(request) < 72 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		resource := GPUResource3D{
			ID:        binary.LittleEndian.Uint32(request[24:28]),
			Target:    binary.LittleEndian.Uint32(request[28:32]),
			Format:    binary.LittleEndian.Uint32(request[32:36]),
			Bind:      binary.LittleEndian.Uint32(request[36:40]),
			Width:     binary.LittleEndian.Uint32(request[40:44]),
			Height:    binary.LittleEndian.Uint32(request[44:48]),
			Depth:     binary.LittleEndian.Uint32(request[48:52]),
			ArraySize: binary.LittleEndian.Uint32(request[52:56]),
			LastLevel: binary.LittleEndian.Uint32(request[56:60]),
			Samples:   binary.LittleEndian.Uint32(request[60:64]),
			Flags:     binary.LittleEndian.Uint32(request[64:68]),
		}
		if resource.ID == 0 || resource.Width == 0 || resource.Height == 0 ||
			resource.Depth == 0 || resource.ArraySize == 0 || g.resources[resource.ID] != nil {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		if err := g.renderer.CreateResource(resource); err != nil {
			log.Printf("virtio-gpu rejected 3D resource %+v: %v", resource, err)
			return gpuResponse(request, gpuRespErrUnspecified, nil)
		}
		g.resources[resource.ID] = &gpuResource{
			id:         resource.ID,
			format:     resource.Format,
			width:      resource.Width,
			height:     resource.Height,
			resource3D: &resource,
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdTransferToHost3D, gpuCmdTransferFromHost3D:
		if g.renderer == nil || len(request) < 72 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		contextID := binary.LittleEndian.Uint32(request[16:20])
		resourceID := binary.LittleEndian.Uint32(request[56:60])
		resource := g.resources[resourceID]
		if contextID != 0 && g.contexts[contextID] == nil {
			return gpuResponse(request, gpuRespErrInvalidContextID, nil)
		}
		if resource == nil || resource.resource3D == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		box, ok := decodeGPUBox(request[24:48])
		if !ok || len(resource.backing) == 0 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		transfer := GPUTransfer3D{
			ContextID:   contextID,
			ResourceID:  resourceID,
			Box:         box,
			Offset:      binary.LittleEndian.Uint64(request[48:56]),
			Level:       binary.LittleEndian.Uint32(request[60:64]),
			Stride:      binary.LittleEndian.Uint32(request[64:68]),
			LayerStride: binary.LittleEndian.Uint32(request[68:72]),
			Backing:     gpuResourceBacking{gpu: g, resource: resource},
		}
		var err error
		if command == gpuCmdTransferToHost3D {
			err = g.renderer.TransferToHost(transfer)
		} else {
			err = g.renderer.TransferFromHost(transfer)
		}
		if err != nil {
			return gpuResponse(request, gpuRespErrUnspecified, nil)
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdSubmit3D:
		if g.renderer == nil || len(request) < 32 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		contextID := binary.LittleEndian.Uint32(request[16:20])
		size := binary.LittleEndian.Uint32(request[24:28])
		if g.contexts[contextID] == nil {
			return gpuResponse(request, gpuRespErrInvalidContextID, nil)
		}
		if uint64(size) > uint64(len(request)-32) || size&3 != 0 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		if err := g.renderer.Submit(contextID, append([]byte(nil), request[32:32+size]...)); err != nil {
			g.reportGPUErrorLocked(err)
			return gpuResponse(request, gpuRespErrUnspecified, nil)
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdTransferToHost2D:
		if len(request) < 56 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		rect, ok := decodeGPURect(request[24:40])
		offset := binary.LittleEndian.Uint64(request[40:48])
		id := binary.LittleEndian.Uint32(request[48:52])
		resource := g.resources[id]
		if resource == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		if !ok || !gpuRectWithin(rect, resource.width, resource.height) || len(resource.backing) == 0 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		rowBytes := rect.Dx() * 4
		for y := 0; y < rect.Dy(); y++ {
			resourceOffset := (uint64(rect.Min.Y+y)*uint64(resource.width) + uint64(rect.Min.X)) * 4
			dstOffset := int(resourceOffset)
			backingOffset := offset + uint64(y)*uint64(resource.width)*4
			if backingOffset < offset {
				return gpuResponse(request, gpuRespErrInvalidParameter, nil)
			}
			if err := g.readBackingLocked(resource, backingOffset, resource.pixels[dstOffset:dstOffset+rowBytes]); err != nil {
				return gpuResponse(request, gpuRespErrInvalidParameter, nil)
			}
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdSetScanout:
		if len(request) < 48 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		rect, ok := decodeGPURect(request[24:40])
		scanout := binary.LittleEndian.Uint32(request[40:44])
		id := binary.LittleEndian.Uint32(request[44:48])
		if scanout != 0 {
			g.reportGPUErrorLocked(fmt.Errorf("set scanout rejected unsupported scanout %d", scanout))
			return gpuResponse(request, gpuRespErrInvalidScanoutID, nil)
		}
		if id == 0 {
			g.releaseNativeFrameLocked(0)
			g.scanoutResource = 0
			g.scanoutRect = image.Rectangle{}
			return gpuResponse(request, gpuRespOKNoData, nil)
		}
		resource := g.resources[id]
		if resource == nil {
			g.reportGPUErrorLocked(fmt.Errorf("set scanout refers to unknown resource %d", id))
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		if !ok || !gpuRectWithin(rect, resource.width, resource.height) {
			g.reportGPUErrorLocked(fmt.Errorf("set scanout rectangle %v exceeds resource %d dimensions %dx%d", rect, id, resource.width, resource.height))
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		fbWidth, fbHeight := g.framebuffer.Size()
		if rect.Dx() != fbWidth || rect.Dy() != fbHeight {
			if err := g.framebuffer.Resize(rect.Dx(), rect.Dy()); err != nil {
				g.reportGPUErrorLocked(fmt.Errorf("resize scanout framebuffer to %dx%d: %w", rect.Dx(), rect.Dy(), err))
				return gpuResponse(request, gpuRespErrInvalidParameter, nil)
			}
		}
		if g.scanoutResource != id || g.scanoutRect != rect {
			g.releaseNativeFrameLocked(0)
		}
		g.scanoutResource = id
		g.scanoutRect = rect
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdResourceFlush:
		if len(request) < 48 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		rect, ok := decodeGPURect(request[24:40])
		id := binary.LittleEndian.Uint32(request[40:44])
		resource := g.resources[id]
		if resource == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		if !ok || !gpuRectWithin(rect, resource.width, resource.height) {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		if id == g.scanoutResource {
			if resource.resource3D == nil {
				g.flushResourceLocked(resource, rect)
			} else if err := g.flushResource3DLocked(resource, rect); err != nil {
				g.reportGPUErrorLocked(err)
				return gpuResponse(request, gpuRespErrUnspecified, nil)
			}
		}
		return gpuResponse(request, gpuRespOKNoData, nil)

	case gpuCmdUpdateCursor, gpuCmdMoveCursor:
		if len(request) < 56 {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		if binary.LittleEndian.Uint32(request[24:28]) != 0 {
			return gpuResponse(request, gpuRespErrInvalidScanoutID, nil)
		}
		if command == gpuCmdMoveCursor {
			g.cursor.Move(int(int32(binary.LittleEndian.Uint32(request[28:32]))), int(int32(binary.LittleEndian.Uint32(request[32:36]))))
			return gpuResponse(request, gpuRespOKNoData, nil)
		}
		id := binary.LittleEndian.Uint32(request[40:44])
		if id == 0 {
			g.cursorResource = 0
			g.cursor.Hide()
			return gpuResponse(request, gpuRespOKNoData, nil)
		}
		resource := g.resources[id]
		if resource == nil {
			return gpuResponse(request, gpuRespErrInvalidResourceID, nil)
		}
		hotX := binary.LittleEndian.Uint32(request[44:48])
		hotY := binary.LittleEndian.Uint32(request[48:52])
		if hotX >= resource.width || hotY >= resource.height {
			return gpuResponse(request, gpuRespErrInvalidParameter, nil)
		}
		g.cursor.Move(int(int32(binary.LittleEndian.Uint32(request[28:32]))), int(int32(binary.LittleEndian.Uint32(request[32:36]))))
		g.cursorResource = id
		g.cursor.Update(int(resource.width), int(resource.height), int(hotX), int(hotY), resource.pixels)
		return gpuResponse(request, gpuRespOKNoData, nil)
	}
	return gpuResponse(request, gpuRespErrUnspecified, nil)
}

func (g *GPU) reportGPUErrorLocked(err error) {
	message := err.Error()
	if _, reported := g.rendererErrors[message]; reported {
		return
	}
	g.rendererErrors[message] = struct{}{}
	log.Printf("virtio-gpu rejected an operation: %v", err)
}

func (g *GPU) readBackingLocked(resource *gpuResource, offset uint64, dst []byte) error {
	remaining := dst
	position := uint64(0)
	for _, entry := range resource.backing {
		end := position + uint64(entry.length)
		if offset >= end {
			position = end
			continue
		}
		entryOffset := uint64(0)
		if offset > position {
			entryOffset = offset - position
		}
		count := uint64(len(remaining))
		if available := uint64(entry.length) - entryOffset; count > available {
			count = available
		}
		raw, err := g.mem.ReadIPA(entry.addr+entryOffset, int(count))
		if err != nil {
			return err
		}
		copy(remaining, raw)
		remaining = remaining[count:]
		offset += count
		position = end
		if len(remaining) == 0 {
			return nil
		}
	}
	return fmt.Errorf("virtio-gpu backing is short by %d bytes", len(remaining))
}

func (g *GPU) writeBackingLocked(resource *gpuResource, offset uint64, src []byte) error {
	remaining := src
	position := uint64(0)
	for _, entry := range resource.backing {
		end := position + uint64(entry.length)
		if offset >= end {
			position = end
			continue
		}
		entryOffset := uint64(0)
		if offset > position {
			entryOffset = offset - position
		}
		count := uint64(len(remaining))
		if available := uint64(entry.length) - entryOffset; count > available {
			count = available
		}
		if err := g.mem.WriteIPA(entry.addr+entryOffset, remaining[:count]); err != nil {
			return err
		}
		remaining = remaining[count:]
		offset += count
		position = end
		if len(remaining) == 0 {
			return nil
		}
	}
	return fmt.Errorf("virtio-gpu backing is short by %d bytes", len(remaining))
}

type gpuResourceBacking struct {
	gpu      *GPU
	resource *gpuResource
}

func (b gpuResourceBacking) Size() uint64 {
	var size uint64
	for _, entry := range b.resource.backing {
		size += uint64(entry.length)
	}
	return size
}

func (b gpuResourceBacking) ReadAt(offset uint64, dst []byte) error {
	return b.gpu.readBackingLocked(b.resource, offset, dst)
}

func (b gpuResourceBacking) WriteAt(offset uint64, src []byte) error {
	return b.gpu.writeBackingLocked(b.resource, offset, src)
}

func (g *GPU) flushResource3DLocked(resource *gpuResource, rect image.Rectangle) error {
	if renderer, ok := g.renderer.(GPUNativeScanoutRenderer); ok {
		// A frame the frontend has not acquired yet is obsolete as soon as a
		// newer guest flush arrives. Retire it before asking the renderer for
		// another lease so a bounded native-frame pool cannot fill and force
		// every subsequent flush through the CPU readback fallback.
		g.releaseNativeFrameLocked(0)
		frame, available, err := renderer.NativeScanout(resource.id, rect)
		if err != nil {
			return fmt.Errorf("publish native 3D scanout resource %d: %w", resource.id, err)
		}
		if available {
			g.nativeGeneration++
			frame.Generation = g.nativeGeneration
			frame.Damage = rect.Sub(g.scanoutRect.Min)
			g.nativeFrame = &frame
			g.framebuffer.MarkDirty(frame.Damage)
			return nil
		}
	}
	pixels, stride, err := g.renderer.ReadScanout(resource.id, rect)
	if err != nil {
		return fmt.Errorf("read 3D scanout resource %d: %w", resource.id, err)
	}
	dstRect := rect.Sub(g.scanoutRect.Min)
	if err := g.framebuffer.Update(dstRect, pixels, stride); err != nil {
		return fmt.Errorf("update 3D scanout resource %d: %w", resource.id, err)
	}
	return nil
}

func (g *GPU) AcquireNativeFrame(since uint64) (GPUNativeFrame, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.nativeFrame == nil || g.nativeFrame.Generation <= since {
		return GPUNativeFrame{}, false, nil
	}
	frame := *g.nativeFrame
	g.nativeFrame = nil
	return frame, true, nil
}

func (g *GPU) Snapshot(request image.Rectangle, since uint64, incremental bool) (FramebufferUpdate, error) {
	g.mu.Lock()
	err := g.syncNativeScanoutLocked()
	g.mu.Unlock()
	if err != nil {
		return FramebufferUpdate{}, err
	}
	return g.framebuffer.Snapshot(request, since, incremental), nil
}

func (g *GPU) syncNativeScanoutLocked() error {
	if g.nativeGeneration == 0 || g.cpuGeneration == g.nativeGeneration ||
		g.scanoutResource == 0 || g.scanoutRect.Empty() {
		return nil
	}
	resource := g.resources[g.scanoutResource]
	if resource == nil || resource.resource3D == nil {
		return nil
	}
	pixels, stride, err := g.renderer.ReadScanout(resource.id, g.scanoutRect)
	if err != nil {
		return fmt.Errorf("read native 3D scanout resource %d for CPU consumer: %w", resource.id, err)
	}
	dstRect := image.Rect(0, 0, g.scanoutRect.Dx(), g.scanoutRect.Dy())
	if err := g.framebuffer.Update(dstRect, pixels, stride); err != nil {
		return fmt.Errorf("update native 3D scanout resource %d for CPU consumer: %w", resource.id, err)
	}
	g.cpuGeneration = g.nativeGeneration
	return nil
}

func (g *GPU) releaseNativeFrameLocked(consumerFence uintptr) {
	if g.nativeFrame == nil {
		return
	}
	if g.nativeFrame.ReleaseFrame != nil {
		g.nativeFrame.ReleaseFrame(consumerFence)
	}
	g.nativeFrame = nil
}

func (g *GPU) flushResourceLocked(resource *gpuResource, requested image.Rectangle) {
	rect := requested.Intersect(g.scanoutRect)
	if rect.Empty() {
		return
	}
	dstRect := rect.Sub(g.scanoutRect.Min)
	rowBytes := rect.Dx() * 4
	pixels := make([]byte, rowBytes*rect.Dy())
	for y := 0; y < rect.Dy(); y++ {
		srcOffset := ((rect.Min.Y+y)*int(resource.width) + rect.Min.X) * 4
		dstOffset := y * rowBytes
		copy(pixels[dstOffset:dstOffset+rowBytes], resource.pixels[srcOffset:srcOffset+rowBytes])
	}
	_ = g.framebuffer.Update(dstRect, pixels, rowBytes)
}

func gpuResponse(request []byte, responseType uint32, extra []byte) []byte {
	response := make([]byte, gpuControlHeaderSize+len(extra))
	binary.LittleEndian.PutUint32(response[0:4], responseType)
	if len(request) >= gpuControlHeaderSize {
		copy(response[4:gpuControlHeaderSize], request[4:gpuControlHeaderSize])
	}
	copy(response[gpuControlHeaderSize:], extra)
	return response
}

func decodeGPURect(raw []byte) (image.Rectangle, bool) {
	if len(raw) < 16 {
		return image.Rectangle{}, false
	}
	x := binary.LittleEndian.Uint32(raw[0:4])
	y := binary.LittleEndian.Uint32(raw[4:8])
	width := binary.LittleEndian.Uint32(raw[8:12])
	height := binary.LittleEndian.Uint32(raw[12:16])
	if width == 0 || height == 0 || uint64(x)+uint64(width) > uint64(^uint32(0)) ||
		uint64(y)+uint64(height) > uint64(^uint32(0)) {
		return image.Rectangle{}, false
	}
	return image.Rect(int(x), int(y), int(x+width), int(y+height)), true
}

func decodeGPUBox(raw []byte) (GPUBox, bool) {
	if len(raw) < 24 {
		return GPUBox{}, false
	}
	box := GPUBox{
		X:      binary.LittleEndian.Uint32(raw[0:4]),
		Y:      binary.LittleEndian.Uint32(raw[4:8]),
		Z:      binary.LittleEndian.Uint32(raw[8:12]),
		Width:  binary.LittleEndian.Uint32(raw[12:16]),
		Height: binary.LittleEndian.Uint32(raw[16:20]),
		Depth:  binary.LittleEndian.Uint32(raw[20:24]),
	}
	if box.Width == 0 || box.Height == 0 || box.Depth == 0 {
		return GPUBox{}, false
	}
	return box, true
}

func gpuRectWithin(rect image.Rectangle, width, height uint32) bool {
	return !rect.Empty() && rect.Min.X >= 0 && rect.Min.Y >= 0 &&
		uint64(rect.Max.X) <= uint64(width) && uint64(rect.Max.Y) <= uint64(height)
}

func supportedGPUFormat(format uint32) bool {
	switch format {
	case gpuFormatB8G8R8A8, gpuFormatB8G8R8X8, gpuFormatA8R8G8B8, gpuFormatX8R8G8B8:
		return true
	default:
		return false
	}
}

func writeConfigUint32(current uint32, offset uint64, size int, value uint64) uint32 {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, current)
	for index := 0; index < size && int(offset)+index < len(raw); index++ {
		raw[int(offset)+index] = byte(value >> (index * 8))
	}
	return binary.LittleEndian.Uint32(raw)
}

func (g *GPU) configBytesLocked() []byte {
	cfg := make([]byte, 16)
	binary.LittleEndian.PutUint32(cfg[0:4], g.eventsRead)
	binary.LittleEndian.PutUint32(cfg[8:12], 1)
	if g.renderer != nil {
		binary.LittleEndian.PutUint32(cfg[12:16], uint32(len(g.renderer.Capsets())))
	}
	return cfg
}

func (g *GPU) selectedQueueLocked() *queue {
	if g.queueSel >= uint32(len(g.queues)) {
		return nil
	}
	return &g.queues[g.queueSel]
}

func (g *GPU) updateIRQLocked() error {
	if g.irq == nil {
		return nil
	}
	level := g.interruptStatus != 0
	if level == g.irqHigh {
		return nil
	}
	g.irqHigh = level
	return g.irq.SetIRQ(g.IRQ, level)
}

func (g *GPU) resetLocked() {
	g.releaseNativeFrameLocked(0)
	if g.renderer != nil {
		g.renderer.Reset()
	}
	g.deviceFeatureSel = 0
	g.driverFeatureSel = 0
	g.driverFeatures = 0
	g.queueSel = 0
	g.sharedMemorySel = 0
	g.status = 0
	g.interruptStatus = 0
	g.irqHigh = false
	g.eventsRead = 0
	g.resources = make(map[uint32]*gpuResource)
	g.contexts = make(map[uint32]*gpuContext)
	g.scanoutResource = 0
	g.scanoutRect = image.Rectangle{}
	g.cursorResource = 0
	g.cursor.Hide()
	g.nativeGeneration = 0
	g.cpuGeneration = 0
	for index := range g.queues {
		g.queues[index] = queue{}
	}
}
