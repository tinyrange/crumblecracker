package virtio

import "image"

// GPUCapset describes one renderer command protocol exposed through
// VIRTIO_GPU_CMD_GET_CAPSET_INFO and VIRTIO_GPU_CMD_GET_CAPSET.
type GPUCapset struct {
	ID      uint32
	Version uint32
	Data    []byte
}

// GPUResource3D is the renderer-facing form of
// struct virtio_gpu_resource_create_3d.
type GPUResource3D struct {
	ID        uint32
	Target    uint32
	Format    uint32
	Bind      uint32
	Width     uint32
	Height    uint32
	Depth     uint32
	ArraySize uint32
	LastLevel uint32
	Samples   uint32
	Flags     uint32
}

// GPUBox is the renderer-facing form of struct virtio_gpu_box.
type GPUBox struct {
	X, Y, Z       uint32
	Width, Height uint32
	Depth         uint32
}

// GPUTransfer3D describes a guest/host resource transfer. Backing is the
// resource's current scatter/gather guest-memory backing.
type GPUTransfer3D struct {
	ContextID   uint32
	ResourceID  uint32
	Box         GPUBox
	Offset      uint64
	Level       uint32
	Stride      uint32
	LayerStride uint32
	Backing     GPUResourceBacking
}

// GPUResourceBacking provides bounded access to a resource's attached guest
// memory without exposing the VM's complete address space to a renderer.
type GPUResourceBacking interface {
	Size() uint64
	ReadAt(offset uint64, dst []byte) error
	WriteAt(offset uint64, src []byte) error
}

// GPUNativeFrame is a leased host texture in the frontend's OpenGL share
// group. ReleaseFrame receives an optional consumer fence inserted after the
// frontend's final sample of Texture.
type GPUNativeFrame struct {
	Width, Height int
	Generation    uint64
	Damage        image.Rectangle
	Texture       uint32
	ProducerFence uintptr
	ReleaseFrame  func(uintptr)
}

// GPUNativeScanoutRenderer is implemented when the renderer can publish
// scanout frames without a CPU readback.
type GPUNativeScanoutRenderer interface {
	NativeScanout(resourceID uint32, rect image.Rectangle) (GPUNativeFrame, bool, error)
}

// GPURenderer owns the host implementation of an advertised virtio-gpu 3D
// command protocol. Calls are ordered exactly as they arrive on controlq.
// Returning from a call means the command is complete for virtio fence
// purposes.
type GPURenderer interface {
	Capsets() []GPUCapset
	CreateContext(id uint32, name string, contextInit uint32) error
	DestroyContext(id uint32) error
	CreateResource(resource GPUResource3D) error
	UnrefResource(id uint32) error
	AttachResource(contextID, resourceID uint32) error
	DetachResource(contextID, resourceID uint32) error
	TransferToHost(transfer GPUTransfer3D) error
	TransferFromHost(transfer GPUTransfer3D) error
	Submit(contextID uint32, commands []byte) error
	ReadScanout(resourceID uint32, rect image.Rectangle) ([]byte, int, error)
	Reset()
	Close() error
}
