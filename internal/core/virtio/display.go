package virtio

import (
	"fmt"
	"image"
	"sync"
	"time"
)

// Framebuffer is the host-visible scanout shared by virtio-gpu and display
// frontends. Pixels are stored as little-endian XRGB8888 (B, G, R, X).
type Framebuffer struct {
	mu         sync.Mutex
	width      int
	height     int
	pixels     []byte
	generation uint64
	dirty      image.Rectangle
	changed    chan struct{}
}

type FramebufferUpdate struct {
	Width      int
	Height     int
	Generation uint64
	Rect       image.Rectangle
	Pixels     []byte
}

type CursorUpdate struct {
	X, Y               int
	PositionGeneration uint64
	Width              int
	Height             int
	HotX               int
	HotY               int
	Visible            bool
	Generation         uint64
	Pixels             []byte
}

// Cursor is the host-visible virtio-gpu cursor plane.
type Cursor struct {
	x, y               int
	positionGeneration uint64
	mu                 sync.Mutex
	width              int
	height             int
	hotX               int
	hotY               int
	visible            bool
	generation         uint64
	pixels             []byte
}

func (c *Cursor) Update(width, height, hotX, hotY int, pixels []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.width = width
	c.height = height
	c.hotX = hotX
	c.hotY = hotY
	c.visible = true
	// Publish a new immutable backing slice so snapshots can be consumed after
	// releasing the lock without racing the next cursor update.
	c.pixels = append([]byte(nil), pixels...)
	c.generation++
}

func (c *Cursor) Move(x, y int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.x, c.y = x, y
	c.positionGeneration++
}

func (c *Cursor) Hide() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.visible {
		return
	}
	c.visible = false
	c.generation++
}

func (c *Cursor) Snapshot() CursorUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CursorUpdate{
		X: c.x, Y: c.y, PositionGeneration: c.positionGeneration,
		Width: c.width, Height: c.height, HotX: c.hotX, HotY: c.hotY,
		Visible: c.visible, Generation: c.generation,
		Pixels: c.pixels,
	}
}

func NewFramebuffer(width, height int) (*Framebuffer, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("framebuffer dimensions must be positive")
	}
	size, ok := checkedPixelBytes(width, height)
	if !ok {
		return nil, fmt.Errorf("framebuffer dimensions %dx%d overflow", width, height)
	}
	return &Framebuffer{
		width:   width,
		height:  height,
		pixels:  make([]byte, size),
		dirty:   image.Rect(0, 0, width, height),
		changed: make(chan struct{}, 1),
	}, nil
}

func checkedPixelBytes(width, height int) (int, bool) {
	if width <= 0 || height <= 0 {
		return 0, false
	}
	const maxInt = int(^uint(0) >> 1)
	if width > maxInt/4 || height > maxInt/(width*4) {
		return 0, false
	}
	return width * height * 4, true
}

func (f *Framebuffer) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.width, f.height
}

// Resize changes the scanout dimensions while preserving the overlapping
// pixels until the guest redraws its new mode.
func (f *Framebuffer) Resize(width, height int) error {
	size, ok := checkedPixelBytes(width, height)
	if !ok {
		return fmt.Errorf("invalid framebuffer dimensions %dx%d", width, height)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if width == f.width && height == f.height {
		return nil
	}
	pixels := make([]byte, size)
	copyWidth := min(width, f.width)
	copyHeight := min(height, f.height)
	for y := 0; y < copyHeight; y++ {
		copy(pixels[y*width*4:y*width*4+copyWidth*4], f.pixels[y*f.width*4:y*f.width*4+copyWidth*4])
	}
	f.width = width
	f.height = height
	f.pixels = pixels
	f.dirty = image.Rect(0, 0, width, height)
	f.generation++
	select {
	case f.changed <- struct{}{}:
	default:
	}
	return nil
}

func (f *Framebuffer) Changed() <-chan struct{} {
	return f.changed
}

// Update copies an XRGB8888 rectangle into the scanout. srcStride is measured
// in bytes and may be wider than rect.
func (f *Framebuffer) Update(rect image.Rectangle, src []byte, srcStride int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	bounds := image.Rect(0, 0, f.width, f.height)
	rect = rect.Intersect(bounds)
	if rect.Empty() {
		return nil
	}
	rowBytes := rect.Dx() * 4
	if srcStride < rowBytes {
		return fmt.Errorf("framebuffer source stride %d is smaller than row %d", srcStride, rowBytes)
	}
	required := (rect.Dy()-1)*srcStride + rowBytes
	if required < 0 || required > len(src) {
		return fmt.Errorf("framebuffer update needs %d bytes, has %d", required, len(src))
	}
	for y := 0; y < rect.Dy(); y++ {
		dstOffset := ((rect.Min.Y+y)*f.width + rect.Min.X) * 4
		srcOffset := y * srcStride
		copy(f.pixels[dstOffset:dstOffset+rowBytes], src[srcOffset:srcOffset+rowBytes])
	}
	f.markDirtyLocked(rect)
	return nil
}

func (f *Framebuffer) MarkDirty(rect image.Rectangle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markDirtyLocked(rect.Intersect(image.Rect(0, 0, f.width, f.height)))
}

func (f *Framebuffer) markDirtyLocked(rect image.Rectangle) {
	if rect.Empty() {
		return
	}
	if f.dirty.Empty() {
		f.dirty = rect
	} else {
		f.dirty = f.dirty.Union(rect)
	}
	f.generation++
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// Snapshot returns the requested pixels. For incremental requests it returns
// no pixels when generation is current, otherwise it intersects the request
// with the accumulated dirty region.
func (f *Framebuffer) Snapshot(request image.Rectangle, since uint64, incremental bool) FramebufferUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()

	bounds := image.Rect(0, 0, f.width, f.height)
	request = request.Intersect(bounds)
	rect := request
	if incremental {
		if !f.dirty.Empty() {
			rect = rect.Intersect(f.dirty)
		} else if since == f.generation {
			rect = image.Rectangle{}
		}
	}
	update := FramebufferUpdate{
		Width:      f.width,
		Height:     f.height,
		Generation: f.generation,
		Rect:       rect,
	}
	if rect.Empty() {
		return update
	}
	rowBytes := rect.Dx() * 4
	update.Pixels = make([]byte, rowBytes*rect.Dy())
	for y := 0; y < rect.Dy(); y++ {
		srcOffset := ((rect.Min.Y+y)*f.width + rect.Min.X) * 4
		dstOffset := y * rowBytes
		copy(update.Pixels[dstOffset:dstOffset+rowBytes], f.pixels[srcOffset:srcOffset+rowBytes])
	}
	if rect == f.dirty {
		f.dirty = image.Rectangle{}
	}
	return update
}

// Desktop binds a scanout to the virtio input devices used by a frontend.
type Desktop struct {
	Framebuffer     *Framebuffer
	GPU             *GPU
	Keyboard        *Input
	Pointer         *Input
	RelativePointer *Input
	Clipboard       *Clipboard

	pointerMu           sync.Mutex
	relativePointerMode bool
	pointerX, pointerY  int
	pointerMotionAt     time.Time
	resizeMu            sync.Mutex
	resizeRequests      chan DisplaySize
}

type DisplaySize struct {
	Width  uint32
	Height uint32
}

func (d *Desktop) Snapshot(request image.Rectangle, since uint64, incremental bool) (FramebufferUpdate, error) {
	if d == nil || d.Framebuffer == nil {
		return FramebufferUpdate{}, fmt.Errorf("desktop framebuffer is unavailable")
	}
	if d.GPU != nil {
		return d.GPU.Snapshot(request, since, incremental)
	}
	return d.Framebuffer.Snapshot(request, since, incremental), nil
}

func (d *Desktop) AcquireNativeFrame(since uint64) (GPUNativeFrame, bool, error) {
	if d == nil || d.GPU == nil {
		return GPUNativeFrame{}, false, nil
	}
	return d.GPU.AcquireNativeFrame(since)
}

func (d *Desktop) Resize(width, height int) error {
	if d == nil || d.GPU == nil || d.Framebuffer == nil {
		return fmt.Errorf("desktop resize is unavailable")
	}
	// Linux's virtio-gpu DRM driver constructs hotplug modes using CVT's
	// eight-pixel horizontal character cells. Normalize at the shared desktop
	// boundary so every frontend requests a mode the guest can actually apply.
	if aligned := width &^ 7; aligned > 0 {
		width = aligned
	}
	if width <= 0 || height <= 0 || width > 8192 || height > 8192 {
		return fmt.Errorf("invalid desktop dimensions %dx%d", width, height)
	}
	if err := d.GPU.Resize(width, height); err != nil {
		return err
	}
	if d.Pointer != nil {
		d.Pointer.SetDimensions(uint32(width), uint32(height))
	}
	d.queueResize(DisplaySize{Width: uint32(width), Height: uint32(height)})
	return nil
}

// ResizeRequests carries the newest requested display mode to an optional
// guest desktop agent. The virtio-GPU hotplug event updates the kernel's
// connector state, while userspace agents apply that mode to a running
// desktop session.
func (d *Desktop) ResizeRequests() <-chan DisplaySize {
	return d.resizeChannel()
}

func (d *Desktop) resizeChannel() chan DisplaySize {
	if d == nil {
		return nil
	}
	d.resizeMu.Lock()
	defer d.resizeMu.Unlock()
	if d.resizeRequests == nil {
		d.resizeRequests = make(chan DisplaySize, 1)
	}
	return d.resizeRequests
}

func (d *Desktop) queueResize(size DisplaySize) {
	requests := d.resizeChannel()
	if requests == nil {
		return
	}
	select {
	case requests <- size:
		return
	default:
	}
	select {
	case <-requests:
	default:
	}
	select {
	case requests <- size:
	default:
	}
}
