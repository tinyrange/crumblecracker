package virtio

import "time"

// SetRelativePointerMode keeps coordinate-based clients (including automation)
// on the same guest device as captured native input. The frontend enables this
// only after the image advertises the relative desktop handshake.
func (d *Desktop) SetRelativePointerMode(enabled bool) {
	d.pointerMu.Lock()
	defer d.pointerMu.Unlock()
	d.relativePointerMode = enabled
	d.pointerMotionAt = time.Time{}
}

func (d *Desktop) reconcilePointerLocked() {
	if d.GPU == nil || d.GPU.Cursor() == nil || time.Since(d.pointerMotionAt) < 80*time.Millisecond {
		return
	}
	c := d.GPU.Cursor().Snapshot()
	if c.PositionGeneration != 0 {
		d.pointerX, d.pointerY = c.X, c.Y
	}
}

func (d *Desktop) SendPointer(x, y uint32, buttons, previous uint8) error {
	d.pointerMu.Lock()
	defer d.pointerMu.Unlock()
	if !d.relativePointerMode {
		if d.Pointer == nil {
			return nil
		}
		return d.Pointer.PointerEvent(x, y, buttons, previous)
	}
	d.reconcilePointerLocked()
	return d.sendRelativePointerLocked(int32(int64(x)-int64(d.pointerX)), int32(int64(y)-int64(d.pointerY)), buttons, previous)
}

func (d *Desktop) SendRelativePointer(x, y int32, buttons, previous uint8) error {
	d.pointerMu.Lock()
	defer d.pointerMu.Unlock()
	d.reconcilePointerLocked()
	return d.sendRelativePointerLocked(x, y, buttons, previous)
}

func (d *Desktop) sendRelativePointerLocked(x, y int32, buttons, previous uint8) error {
	if d.RelativePointer == nil {
		return nil
	}
	if err := d.RelativePointer.RelativePointerEvent(x, y, buttons, previous); err != nil {
		return err
	}
	d.pointerX += int(x)
	d.pointerY += int(y)
	if d.Framebuffer != nil {
		w, h := d.Framebuffer.Size()
		d.pointerX = max(0, min(w-1, d.pointerX))
		d.pointerY = max(0, min(h-1, d.pointerY))
	}
	if x != 0 || y != 0 {
		d.pointerMotionAt = time.Now()
	}
	return nil
}

func (d *Desktop) SendScroll(x, y int32) error {
	d.pointerMu.Lock()
	defer d.pointerMu.Unlock()
	pointer := d.Pointer
	if d.relativePointerMode {
		pointer = d.RelativePointer
	}
	if pointer == nil {
		return nil
	}
	return pointer.ScrollEvent(x, y)
}
