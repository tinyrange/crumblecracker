package virgl

import (
	"fmt"
	"image"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

const maxResourceBytes = 256 << 20

type context struct {
	id        uint32
	resources map[uint32]struct{}
}

type resource struct {
	description virtio.GPUResource3D
	data        []byte
}

type hostBackend interface {
	createContext(id uint32) error
	destroyContext(id uint32) error
	createResource(description virtio.GPUResource3D) error
	unrefResource(id uint32) error
	transferToHost(resource *resource, transfer virtio.GPUTransfer3D) error
	transferFromHost(resource *resource, transfer virtio.GPUTransfer3D) error
	execute(contextID uint32, commands []command, resources map[uint32]*resource) error
	readScanout(resource *resource, rect image.Rectangle) ([]byte, int, error)
	nativeScanout(resource *resource, rect image.Rectangle) (virtio.GPUNativeFrame, bool, error)
	reset() error
	close() error
}

type bufferTransferQueuer interface {
	queueBufferTransfer(resource *resource, transfer virtio.GPUTransfer3D) error
}

// Renderer is cc's first-party implementation of the VirGL Gallium command
// protocol. Backend GL execution is platform-specific; protocol and object
// validation remain here.
type Renderer struct {
	mu        sync.Mutex
	capsets   []virtio.GPUCapset
	contexts  map[uint32]*context
	resources map[uint32]*resource
	host      hostBackend
}

func NewRenderer(host hostBackend) *Renderer {
	r := &Renderer{
		capsets: []virtio.GPUCapset{
			{
				ID:      capsetVirGL,
				Version: capsetVersion,
				Data:    buildCapsetV1(),
			},
			{
				ID:      capsetVirGL2,
				Version: capsetVersion2,
				Data:    buildCapsetV2(),
			},
		},
		host: host,
	}
	r.Reset()
	return r
}

func (r *Renderer) Capsets() []virtio.GPUCapset {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]virtio.GPUCapset, len(r.capsets))
	for index, capset := range r.capsets {
		result[index] = capset
		result[index].Data = append([]byte(nil), capset.Data...)
	}
	return result
}

func (r *Renderer) CreateContext(id uint32, _ string, _ uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == 0 || r.contexts[id] != nil {
		return fmt.Errorf("invalid VirGL context %d", id)
	}
	if r.host == nil {
		return fmt.Errorf("VirGL host execution backend is unavailable")
	}
	if err := r.host.createContext(id); err != nil {
		return err
	}
	r.contexts[id] = &context{id: id, resources: make(map[uint32]struct{})}
	return nil
}

func (r *Renderer) DestroyContext(id uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.contexts[id] == nil {
		return fmt.Errorf("unknown VirGL context %d", id)
	}
	if err := r.host.destroyContext(id); err != nil {
		return err
	}
	delete(r.contexts, id)
	return nil
}

func (r *Renderer) CreateResource(description virtio.GPUResource3D) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if description.ID == 0 || r.resources[description.ID] != nil {
		return fmt.Errorf("invalid VirGL resource %d", description.ID)
	}
	if r.host == nil {
		return fmt.Errorf("VirGL host execution backend is unavailable")
	}
	if err := r.host.createResource(description); err != nil {
		return err
	}
	r.resources[description.ID] = &resource{description: description}
	return nil
}

func (r *Renderer) UnrefResource(id uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resources[id] == nil {
		return fmt.Errorf("unknown VirGL resource %d", id)
	}
	if err := r.host.unrefResource(id); err != nil {
		return err
	}
	delete(r.resources, id)
	for _, context := range r.contexts {
		delete(context.resources, id)
	}
	return nil
}

func (r *Renderer) AttachResource(contextID, resourceID uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	context := r.contexts[contextID]
	if context == nil || r.resources[resourceID] == nil {
		return fmt.Errorf("cannot attach resource %d to context %d", resourceID, contextID)
	}
	context.resources[resourceID] = struct{}{}
	return nil
}

func (r *Renderer) DetachResource(contextID, resourceID uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	context := r.contexts[contextID]
	if context == nil {
		return fmt.Errorf("unknown VirGL context %d", contextID)
	}
	if _, ok := context.resources[resourceID]; !ok {
		return fmt.Errorf("resource %d is not attached to context %d", resourceID, contextID)
	}
	delete(context.resources, resourceID)
	return nil
}

func (r *Renderer) TransferToHost(transfer virtio.GPUTransfer3D) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.resources[transfer.ResourceID]
	if target == nil {
		return fmt.Errorf("unknown VirGL resource %d", transfer.ResourceID)
	}
	if transfer.Backing == nil {
		return fmt.Errorf("VirGL resource %d has no guest backing", transfer.ResourceID)
	}
	data, normalized, err := stageTransferData(target.description, transfer)
	if err != nil {
		return err
	}
	staged := &resource{description: target.description, data: data}
	if target.description.Target == 0 {
		if host, ok := r.host.(bufferTransferQueuer); ok {
			return host.queueBufferTransfer(staged, normalized)
		}
	}
	return r.host.transferToHost(staged, normalized)
}

func stageTransferData(description virtio.GPUResource3D, transfer virtio.GPUTransfer3D) ([]byte, virtio.GPUTransfer3D, error) {
	size, err := transferDataSize(description, transfer)
	if err != nil {
		return nil, transfer, err
	}
	end := transfer.Offset + size
	if end < transfer.Offset || end > transfer.Backing.Size() {
		return nil, transfer, fmt.Errorf("VirGL resource %d transfer backing range %d..%d exceeds %d bytes",
			transfer.ResourceID, transfer.Offset, end, transfer.Backing.Size())
	}
	if size > maxResourceBytes {
		return nil, transfer, fmt.Errorf("VirGL resource %d transfer is %d bytes, limit is %d", transfer.ResourceID, size, maxResourceBytes)
	}
	if description.Target == 0 {
		data := make([]byte, int(size))
		if err := transfer.Backing.ReadAt(transfer.Offset, data); err != nil {
			return nil, transfer, err
		}
		transfer.Offset = 0
		return data, transfer, nil
	}

	layout, err := describeTextureTransfer(description, transfer)
	if err != nil {
		return nil, transfer, err
	}
	if layout.packedSize > maxResourceBytes {
		return nil, transfer, fmt.Errorf("VirGL resource %d packed transfer is %d bytes, limit is %d",
			transfer.ResourceID, layout.packedSize, maxResourceBytes)
	}
	data := make([]byte, int(layout.packedSize))
	for layer := uint32(0); layer < transfer.Box.Depth; layer++ {
		for row := uint32(0); row < transfer.Box.Height; row++ {
			destination := uint64(layer)*layout.packedLayer + uint64(row)*layout.rowBytes
			source := transfer.Offset + uint64(layer)*layout.layerStride + uint64(row)*layout.stride
			if err := transfer.Backing.ReadAt(source, data[int(destination):int(destination+layout.rowBytes)]); err != nil {
				return nil, transfer, err
			}
		}
	}
	transfer.Offset = 0
	transfer.Stride = uint32(layout.rowBytes)
	transfer.LayerStride = uint32(layout.packedLayer)
	return data, transfer, nil
}

func transferDataSize(description virtio.GPUResource3D, transfer virtio.GPUTransfer3D) (uint64, error) {
	if description.Target == 0 {
		return uint64(transfer.Box.Width), nil
	}
	layout, err := describeTextureTransfer(description, transfer)
	return layout.span, err
}

type textureTransferLayout struct {
	rowBytes, stride, layerStride uint64
	packedLayer, packedSize, span uint64
}

func describeTextureTransfer(description virtio.GPUResource3D, transfer virtio.GPUTransfer3D) (textureTransferLayout, error) {
	var layout textureTransferLayout
	bytesPerPixel := textureFormatBytes(description.Format)
	if bytesPerPixel == 0 {
		return layout, fmt.Errorf("VirGL resource %d uses unsupported texture format %d", transfer.ResourceID, description.Format)
	}
	checkedMultiply := func(left, right uint64) (uint64, bool) {
		if left != 0 && right > ^uint64(0)/left {
			return 0, false
		}
		return left * right, true
	}
	checkedAdd := func(left, right uint64) (uint64, bool) {
		if right > ^uint64(0)-left {
			return 0, false
		}
		return left + right, true
	}
	var ok bool
	layout.rowBytes, ok = checkedMultiply(uint64(transfer.Box.Width), bytesPerPixel)
	if !ok {
		return layout, fmt.Errorf("VirGL resource %d transfer row size overflows", transfer.ResourceID)
	}
	layout.stride = uint64(transfer.Stride)
	levelWidth := description.Width >> transfer.Level
	if levelWidth == 0 {
		levelWidth = 1
	}
	levelHeight := description.Height >> transfer.Level
	if levelHeight == 0 {
		levelHeight = 1
	}
	if layout.stride == 0 {
		layout.stride, ok = checkedMultiply(uint64(levelWidth), bytesPerPixel)
		if !ok {
			return layout, fmt.Errorf("VirGL resource %d transfer stride overflows", transfer.ResourceID)
		}
	}
	if layout.stride < layout.rowBytes {
		return layout, fmt.Errorf("VirGL texture transfer stride %d is smaller than row size %d", layout.stride, layout.rowBytes)
	}
	layout.layerStride = uint64(transfer.LayerStride)
	if layout.layerStride == 0 {
		layout.layerStride, ok = checkedMultiply(layout.stride, uint64(levelHeight))
		if !ok {
			return layout, fmt.Errorf("VirGL resource %d transfer layer stride overflows", transfer.ResourceID)
		}
	}
	if transfer.Box.Height == 0 || transfer.Box.Depth == 0 {
		return layout, nil
	}
	rowsTail, ok := checkedMultiply(uint64(transfer.Box.Height-1), layout.stride)
	if !ok {
		return layout, fmt.Errorf("VirGL resource %d transfer size overflows", transfer.ResourceID)
	}
	layerSpan, ok := checkedAdd(layout.rowBytes, rowsTail)
	if !ok {
		return layout, fmt.Errorf("VirGL resource %d transfer size overflows", transfer.ResourceID)
	}
	if transfer.Box.Depth > 1 && layout.layerStride < layerSpan {
		return layout, fmt.Errorf("VirGL texture transfer layer stride %d is smaller than layer size %d", layout.layerStride, layerSpan)
	}
	layersTail, ok := checkedMultiply(uint64(transfer.Box.Depth-1), layout.layerStride)
	if !ok {
		return layout, fmt.Errorf("VirGL resource %d transfer size overflows", transfer.ResourceID)
	}
	layout.span, ok = checkedAdd(layerSpan, layersTail)
	if !ok {
		return layout, fmt.Errorf("VirGL resource %d transfer size overflows", transfer.ResourceID)
	}
	layout.packedLayer, ok = checkedMultiply(layout.rowBytes, uint64(transfer.Box.Height))
	if !ok {
		return layout, fmt.Errorf("VirGL resource %d packed layer size overflows", transfer.ResourceID)
	}
	layout.packedSize, ok = checkedMultiply(layout.packedLayer, uint64(transfer.Box.Depth))
	if !ok {
		return layout, fmt.Errorf("VirGL resource %d packed transfer size overflows", transfer.ResourceID)
	}
	return layout, nil
}

func (r *Renderer) TransferFromHost(transfer virtio.GPUTransfer3D) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.resources[transfer.ResourceID]
	if target == nil {
		return fmt.Errorf("unknown VirGL resource %d", transfer.ResourceID)
	}
	if transfer.Backing == nil {
		return fmt.Errorf("VirGL resource %d has no guest backing", transfer.ResourceID)
	}
	data, normalized, err := stageTransferFromHost(target.description, transfer)
	if err != nil {
		return err
	}
	staged := &resource{description: target.description, data: data}
	if err := r.host.transferFromHost(staged, normalized); err != nil {
		return err
	}
	return commitTransferFromHost(target.description, transfer, staged.data)
}

func stageTransferFromHost(description virtio.GPUResource3D, transfer virtio.GPUTransfer3D) ([]byte, virtio.GPUTransfer3D, error) {
	size, err := transferDataSize(description, transfer)
	if err != nil {
		return nil, transfer, err
	}
	end := transfer.Offset + size
	if end < transfer.Offset || end > transfer.Backing.Size() {
		return nil, transfer, fmt.Errorf("VirGL resource %d transfer backing range %d..%d exceeds %d bytes",
			transfer.ResourceID, transfer.Offset, end, transfer.Backing.Size())
	}

	packedSize := uint64(transfer.Box.Width)
	packedLayer := packedSize
	if description.Target != 0 {
		layout, err := describeTextureTransfer(description, transfer)
		if err != nil {
			return nil, transfer, err
		}
		packedSize = layout.packedSize
		packedLayer = layout.packedLayer
	}
	if packedSize > maxResourceBytes {
		return nil, transfer, fmt.Errorf("VirGL resource %d packed transfer is %d bytes, limit is %d",
			transfer.ResourceID, packedSize, maxResourceBytes)
	}
	data := make([]byte, int(packedSize))
	transfer.Offset = 0
	if description.Target != 0 {
		transfer.Stride = transfer.Box.Width * uint32(textureFormatBytes(description.Format))
		transfer.LayerStride = uint32(packedLayer)
	}
	return data, transfer, nil
}

func commitTransferFromHost(description virtio.GPUResource3D, transfer virtio.GPUTransfer3D, data []byte) error {
	if description.Target == 0 {
		return transfer.Backing.WriteAt(transfer.Offset, data)
	}
	layout, err := describeTextureTransfer(description, transfer)
	if err != nil {
		return err
	}
	for layer := uint32(0); layer < transfer.Box.Depth; layer++ {
		for row := uint32(0); row < transfer.Box.Height; row++ {
			source := uint64(layer)*layout.packedLayer + uint64(row)*layout.rowBytes
			destination := transfer.Offset + uint64(layer)*layout.layerStride + uint64(row)*layout.stride
			if err := transfer.Backing.WriteAt(destination, data[int(source):int(source+layout.rowBytes)]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Renderer) Submit(contextID uint32, stream []byte) error {
	commands, err := decodeCommands(stream)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.contexts[contextID] == nil {
		return fmt.Errorf("unknown VirGL context %d", contextID)
	}
	if r.host == nil {
		return fmt.Errorf("VirGL host execution backend is unavailable")
	}
	return r.host.execute(contextID, commands, r.resources)
}

func (r *Renderer) ReadScanout(resourceID uint32, rect image.Rectangle) ([]byte, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	resource := r.resources[resourceID]
	if resource == nil {
		return nil, 0, fmt.Errorf("unknown VirGL scanout resource %d", resourceID)
	}
	return r.host.readScanout(resource, rect)
}

func (r *Renderer) NativeScanout(resourceID uint32, rect image.Rectangle) (virtio.GPUNativeFrame, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	resource := r.resources[resourceID]
	if resource == nil {
		return virtio.GPUNativeFrame{}, false, fmt.Errorf("unknown VirGL scanout resource %d", resourceID)
	}
	return r.host.nativeScanout(resource, rect)
}

func (r *Renderer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contexts = make(map[uint32]*context)
	r.resources = make(map[uint32]*resource)
	if r.host != nil {
		_ = r.host.reset()
	}
}

func (r *Renderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.host == nil {
		return nil
	}
	return r.host.close()
}
