package virtio

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	mmioDeviceIDInput = 18

	inputQueueEvent  = 0
	inputQueueStatus = 1

	inputConfigUnset    = 0x00
	inputConfigName     = 0x01
	inputConfigSerial   = 0x02
	inputConfigIDs      = 0x03
	inputConfigPropBits = 0x10
	inputConfigEVBits   = 0x11
	inputConfigABSInfo  = 0x12

	inputEventSyn = 0x00
	inputEventKey = 0x01
	inputEventRel = 0x02
	inputEventAbs = 0x03

	inputSynReport      = 0
	inputRelX           = 0
	inputRelY           = 1
	inputRelHWheel      = 0x06
	inputRelWheel       = 0x08
	inputRelWheelHiRes  = 0x0b
	inputRelHWheelHiRes = 0x0c
	inputAbsX           = 0
	inputAbsY           = 1
	inputBtnLeft        = 0x110
	inputBtnRight       = 0x111
	inputBtnMiddle      = 0x112
)

type InputKind uint8

const (
	InputKeyboard InputKind = iota
	InputAbsolutePointer
	InputRelativePointer
)

type InputEvent struct {
	Type  uint16
	Code  uint16
	Value int32
}

type Input struct {
	Base uint64
	Size uint64
	IRQ  uint32
	Kind InputKind

	mu               sync.Mutex
	mem              GuestMemory
	irq              IRQController
	deviceFeatureSel uint32
	driverFeatureSel uint32
	driverFeatures   uint64
	queueSel         uint32
	status           uint32
	interruptStatus  uint32
	irqHigh          bool
	configGeneration uint32
	configSelect     byte
	configSubsel     byte
	width            uint32
	height           uint32
	pending          []InputEvent
	scrollX120       int64
	scrollY120       int64
	queues           [2]queue
}

func NewKeyboardInput(base, size uint64, irq uint32) *Input {
	i := &Input{Base: base, Size: size, IRQ: irq, Kind: InputKeyboard}
	i.resetLocked()
	return i
}

func NewAbsolutePointerInput(base, size uint64, irq uint32, width, height uint32) *Input {
	i := &Input{
		Base:   base,
		Size:   size,
		IRQ:    irq,
		Kind:   InputAbsolutePointer,
		width:  width,
		height: height,
	}
	i.resetLocked()
	return i
}

func NewRelativePointerInput(base, size uint64, irq uint32) *Input {
	i := &Input{Base: base, Size: size, IRQ: irq, Kind: InputRelativePointer}
	i.resetLocked()
	return i
}

// RelativePointerEvent preserves signed deltas; movement must never be scaled
// to the absolute tablet range or coalesced by discarding earlier deltas.
func (i *Input) RelativePointerEvent(dx, dy int32, buttons, previous uint8) error {
	events := []InputEvent{}
	if dx != 0 {
		events = append(events, InputEvent{Type: inputEventRel, Code: inputRelX, Value: dx})
	}
	if dy != 0 {
		events = append(events, InputEvent{Type: inputEventRel, Code: inputRelY, Value: dy})
	}
	for _, b := range []struct {
		mask uint8
		code uint16
	}{{1, inputBtnLeft}, {2, inputBtnMiddle}, {4, inputBtnRight}} {
		if buttons&b.mask != previous&b.mask {
			value := int32(0)
			if buttons&b.mask != 0 {
				value = 1
			}
			events = append(events, InputEvent{Type: inputEventKey, Code: b.code, Value: value})
		}
	}
	if len(events) == 0 {
		return nil
	}
	return i.Send(append(events, InputEvent{Type: inputEventSyn, Code: inputSynReport})...)
}

func (i *Input) Attach(mem GuestMemory, irq IRQController) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.mem = mem
	i.irq = irq
}

func (i *Input) IRQAsserted() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.irqHigh
}

func (i *Input) Contains(addr uint64, size int) bool {
	if size <= 0 || addr < i.Base {
		return false
	}
	end := addr + uint64(size)
	return end >= addr && end <= i.Base+i.Size
}

func (i *Input) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("virtio@%x", i.Base),
		Properties: map[string]fdt.Property{
			"compatible":   {Strings: []string{"virtio,mmio"}},
			"dma-coherent": {Flag: true},
			"reg":          {U64: []uint64{i.Base, i.Size}},
			"interrupts":   {U32: []uint32{0, i.IRQ, 4}},
			"status":       {Strings: []string{"okay"}},
		},
	}
}

func (i *Input) Read(addr uint64, size int) (uint64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	offset := addr - i.Base
	switch offset {
	case regMagicValue:
		return truncateValue(mmioMagicValue, size), nil
	case regVersion:
		return truncateValue(mmioVersion, size), nil
	case regDeviceID:
		return truncateValue(mmioDeviceIDInput, size), nil
	case regVendorID:
		return truncateValue(mmioVendorID, size), nil
	case regDeviceFeatures:
		if i.deviceFeatureSel == 1 {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regQueueNumMax:
		if i.selectedQueueLocked() != nil {
			return truncateValue(64, size), nil
		}
		return 0, nil
	case regQueueNum:
		if q := i.selectedQueueLocked(); q != nil {
			return truncateValue(uint64(q.size), size), nil
		}
		return 0, nil
	case regQueueReady:
		if q := i.selectedQueueLocked(); q != nil && q.ready {
			return truncateValue(1, size), nil
		}
		return 0, nil
	case regInterruptStatus:
		return truncateValue(uint64(i.interruptStatus), size), nil
	case regStatus:
		return truncateValue(uint64(i.status), size), nil
	case regConfigGen:
		return truncateValue(uint64(i.configGeneration), size), nil
	default:
		if offset >= regConfig && offset+uint64(size) <= regConfig+136 {
			cfg := i.configBytesLocked()
			return truncateValue(readConfigValue(cfg[offset-regConfig:], size), size), nil
		}
		return 0, nil
	}
}

func (i *Input) Write(addr uint64, size int, value uint64) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	offset := addr - i.Base
	switch offset {
	case regDeviceFeatSel:
		i.deviceFeatureSel = uint32(value)
	case regDriverFeatSel:
		i.driverFeatureSel = uint32(value)
	case regDriverFeatures:
		if i.driverFeatureSel == 0 {
			i.driverFeatures = (i.driverFeatures &^ 0xffffffff) | uint64(uint32(value))
		} else if i.driverFeatureSel == 1 {
			i.driverFeatures = (i.driverFeatures & 0xffffffff) | uint64(uint32(value))<<32
		}
	case regQueueSel:
		i.queueSel = uint32(value)
	case regQueueNum:
		if q := i.selectedQueueLocked(); q != nil {
			if value <= 64 {
				q.size = uint16(value)
			} else {
				q.size = 0
			}
		}
	case regQueueReady:
		if q := i.selectedQueueLocked(); q != nil {
			q.ready = value != 0
			if value == 0 {
				q.lastAvailIdx = 0
				q.usedIdx = 0
			}
		}
	case regQueueDescLow:
		if q := i.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrDesc, true)
		}
	case regQueueDescHigh:
		if q := i.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrDesc, false)
		}
	case regQueueAvailLow:
		if q := i.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrAvail, true)
		}
	case regQueueAvailHigh:
		if q := i.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrAvail, false)
		}
	case regQueueUsedLow:
		if q := i.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrUsed, true)
		}
	case regQueueUsedHigh:
		if q := i.selectedQueueLocked(); q != nil {
			setQueueAddr(q, uint32(value), queueAddrUsed, false)
		}
	case regInterruptAck:
		i.interruptStatus &^= uint32(value)
		return i.updateIRQLocked()
	case regStatus:
		i.status = uint32(value)
		if i.status == 0 {
			i.resetLocked()
		}
	case regQueueNotify:
		switch int(value) {
		case inputQueueEvent:
			return i.flushEventsLocked()
		case inputQueueStatus:
			return i.processStatusLocked()
		}
	default:
		if offset == regConfig && size > 0 {
			i.configSelect = byte(value)
		}
		if offset <= regConfig+1 && offset+uint64(size) > regConfig+1 {
			shift := uint((regConfig + 1 - offset) * 8)
			i.configSubsel = byte(value >> shift)
		}
	}
	return nil
}

func (i *Input) Send(events ...InputEvent) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pending = append(i.pending, events...)
	return i.flushEventsLocked()
}

func (i *Input) Key(code uint16, down bool) error {
	value := int32(0)
	if down {
		value = 1
	}
	return i.Send(
		InputEvent{Type: inputEventKey, Code: code, Value: value},
		InputEvent{Type: inputEventSyn, Code: inputSynReport},
	)
}

func (i *Input) PointerEvent(x, y uint32, buttons uint8, previous uint8) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	x = scaleAbsolutePosition(x, i.width)
	y = scaleAbsolutePosition(y, i.height)
	events := []InputEvent{
		{Type: inputEventAbs, Code: inputAbsX, Value: int32(x)},
		{Type: inputEventAbs, Code: inputAbsY, Value: int32(y)},
	}
	for _, button := range []struct {
		mask uint8
		code uint16
	}{
		{1, inputBtnLeft},
		{2, inputBtnMiddle},
		{4, inputBtnRight},
	} {
		if buttons&button.mask == previous&button.mask {
			continue
		}
		value := int32(0)
		if buttons&button.mask != 0 {
			value = 1
		}
		events = append(events, InputEvent{Type: inputEventKey, Code: button.code, Value: value})
	}
	// RFB represents vertical wheel movement as momentary presses of buttons
	// four and five. Linux expects those pulses as relative wheel events rather
	// than ordinary buttons. Report only the rising edge because viewers send a
	// matching release after each wheel step.
	if buttons&8 != 0 && previous&8 == 0 {
		events = append(events, InputEvent{Type: inputEventRel, Code: inputRelWheel, Value: 1})
	}
	if buttons&16 != 0 && previous&16 == 0 {
		events = append(events, InputEvent{Type: inputEventRel, Code: inputRelWheel, Value: -1})
	}
	events = append(events, InputEvent{Type: inputEventSyn, Code: inputSynReport})

	// A viewer can report pointer motion faster than the guest replenishes its
	// event queue. Only the newest unconsumed position matters when no button
	// transition separates the reports; retaining every intermediate position
	// makes the pointer visibly trail the viewer.
	if buttons == previous && len(i.pending) >= 3 {
		tail := i.pending[len(i.pending)-3:]
		if tail[0].Type == inputEventAbs && tail[0].Code == inputAbsX &&
			tail[1].Type == inputEventAbs && tail[1].Code == inputAbsY &&
			tail[2].Type == inputEventSyn && tail[2].Code == inputSynReport {
			tail[0].Value = int32(x)
			tail[1].Value = int32(y)
			return i.flushEventsLocked()
		}
	}
	i.pending = append(i.pending, events...)
	return i.flushEventsLocked()
}

// ScrollEvent reports high-resolution wheel movement in v120 units. Linux
// expects the high-resolution events for every movement and a legacy wheel
// event whenever the accumulated movement crosses a 120-unit detent.
func (i *Input) ScrollEvent(deltaX120, deltaY120 int32) error {
	if deltaX120 == 0 && deltaY120 == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	events := make([]InputEvent, 0, 5)
	if deltaX120 != 0 {
		events = append(events, InputEvent{Type: inputEventRel, Code: inputRelHWheelHiRes, Value: deltaX120})
		i.scrollX120 += int64(deltaX120)
		if steps := i.scrollX120 / 120; steps != 0 {
			events = append(events, InputEvent{Type: inputEventRel, Code: inputRelHWheel, Value: int32(steps)})
			i.scrollX120 -= steps * 120
		}
	}
	if deltaY120 != 0 {
		events = append(events, InputEvent{Type: inputEventRel, Code: inputRelWheelHiRes, Value: deltaY120})
		i.scrollY120 += int64(deltaY120)
		if steps := i.scrollY120 / 120; steps != 0 {
			events = append(events, InputEvent{Type: inputEventRel, Code: inputRelWheel, Value: int32(steps)})
			i.scrollY120 -= steps * 120
		}
	}
	events = append(events, InputEvent{Type: inputEventSyn, Code: inputSynReport})
	i.pending = append(i.pending, events...)
	return i.flushEventsLocked()
}

func scaleAbsolutePosition(position, extent uint32) uint32 {
	if extent <= 1 {
		return 0
	}
	if position >= extent {
		position = extent - 1
	}
	return position * 0xffff / (extent - 1)
}

func (i *Input) SetDimensions(width, height uint32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.width = width
	i.height = height
	i.configGeneration++
}

func (i *Input) flushEventsLocked() error {
	q := &i.queues[inputQueueEvent]
	if !q.ready || q.size == 0 || i.mem == nil || len(i.pending) == 0 {
		return nil
	}
	availIdx, err := readAvailableIndex(i.mem, q)
	if err != nil {
		return err
	}
	processed := false
	for len(i.pending) > 0 && q.lastAvailIdx != availIdx {
		head, err := readAvailableHead(i.mem, q, q.lastAvailIdx)
		if err != nil {
			return err
		}
		buffers, err := readQueueChain(i.mem, q, head)
		if err != nil {
			return err
		}
		event := i.pending[0]
		raw := make([]byte, 8)
		binary.LittleEndian.PutUint16(raw[0:2], event.Type)
		binary.LittleEndian.PutUint16(raw[2:4], event.Code)
		binary.LittleEndian.PutUint32(raw[4:8], uint32(event.Value))
		written, err := writeQueueResponse(i.mem, buffers, raw)
		if err != nil {
			return err
		}
		if err := writeQueueUsed(i.mem, q, head, written); err != nil {
			return err
		}
		q.lastAvailIdx++
		i.pending = i.pending[1:]
		processed = true
	}
	if processed {
		i.interruptStatus |= intVring
		return i.updateIRQLocked()
	}
	return nil
}

func (i *Input) processStatusLocked() error {
	q := &i.queues[inputQueueStatus]
	if !q.ready || q.size == 0 || i.mem == nil {
		return nil
	}
	availIdx, err := readAvailableIndex(i.mem, q)
	if err != nil {
		return err
	}
	processed := false
	for q.lastAvailIdx != availIdx {
		head, err := readAvailableHead(i.mem, q, q.lastAvailIdx)
		if err != nil {
			return err
		}
		if _, err := readQueueChain(i.mem, q, head); err != nil {
			return err
		}
		if err := writeQueueUsed(i.mem, q, head, 0); err != nil {
			return err
		}
		q.lastAvailIdx++
		processed = true
	}
	if processed {
		i.interruptStatus |= intVring
		return i.updateIRQLocked()
	}
	return nil
}

func (i *Input) configBytesLocked() []byte {
	cfg := make([]byte, 136)
	cfg[0] = i.configSelect
	cfg[1] = i.configSubsel
	var data []byte
	switch i.configSelect {
	case inputConfigName:
		name := "cc keyboard"
		if i.Kind == InputAbsolutePointer {
			name = "cc absolute pointer"
		}
		if i.Kind == InputRelativePointer {
			name = "cc relative pointer"
		}
		data = []byte(name)
	case inputConfigSerial:
		data = []byte("glass")
	case inputConfigIDs:
		data = make([]byte, 8)
		binary.LittleEndian.PutUint16(data[0:2], 0x06)
		binary.LittleEndian.PutUint16(data[2:4], 0x1af4)
		binary.LittleEndian.PutUint16(data[4:6], uint16(i.Kind)+1)
		binary.LittleEndian.PutUint16(data[6:8], 1)
	case inputConfigPropBits:
		if i.Kind == InputAbsolutePointer {
			data = []byte{1 << 1}
		}
	case inputConfigEVBits:
		data = i.eventBitmapLocked(i.configSubsel)
	case inputConfigABSInfo:
		if i.Kind == InputAbsolutePointer && (i.configSubsel == inputAbsX || i.configSubsel == inputAbsY) {
			data = make([]byte, 20)
			binary.LittleEndian.PutUint32(data[4:8], 0xffff)
		}
	}
	if len(data) > 128 {
		data = data[:128]
	}
	cfg[2] = byte(len(data))
	copy(cfg[8:], data)
	return cfg
}

func (i *Input) eventBitmapLocked(eventType byte) []byte {
	switch eventType {
	case 0:
		bitmap := make([]byte, 1)
		setInputBit(bitmap, inputEventSyn)
		setInputBit(bitmap, inputEventKey)
		if i.Kind == InputRelativePointer {
			setInputBit(bitmap, inputEventRel)
		}
		if i.Kind == InputAbsolutePointer {
			setInputBit(bitmap, inputEventRel)
			setInputBit(bitmap, inputEventAbs)
		}
		return bitmap
	case inputEventKey:
		if i.Kind == InputKeyboard {
			bitmap := make([]byte, 32)
			for code := uint16(1); code < 256; code++ {
				setInputBit(bitmap, code)
			}
			return bitmap
		}
		bitmap := make([]byte, inputBtnMiddle/8+1)
		setInputBit(bitmap, inputBtnLeft)
		setInputBit(bitmap, inputBtnRight)
		setInputBit(bitmap, inputBtnMiddle)
		return bitmap
	case inputEventRel:
		if i.Kind == InputAbsolutePointer || i.Kind == InputRelativePointer {
			bitmap := make([]byte, inputRelHWheelHiRes/8+1)
			if i.Kind == InputRelativePointer {
				setInputBit(bitmap, inputRelX)
				setInputBit(bitmap, inputRelY)
			}
			setInputBit(bitmap, inputRelHWheel)
			setInputBit(bitmap, inputRelWheel)
			setInputBit(bitmap, inputRelWheelHiRes)
			setInputBit(bitmap, inputRelHWheelHiRes)
			return bitmap
		}
	case inputEventAbs:
		if i.Kind == InputAbsolutePointer {
			bitmap := make([]byte, 1)
			setInputBit(bitmap, inputAbsX)
			setInputBit(bitmap, inputAbsY)
			return bitmap
		}
	}
	return nil
}

func setInputBit(bitmap []byte, bit uint16) {
	index := int(bit / 8)
	if index >= len(bitmap) {
		return
	}
	bitmap[index] |= 1 << (bit % 8)
}

func (i *Input) selectedQueueLocked() *queue {
	if i.queueSel >= uint32(len(i.queues)) {
		return nil
	}
	return &i.queues[i.queueSel]
}

func (i *Input) updateIRQLocked() error {
	if i.irq == nil {
		return nil
	}
	level := i.interruptStatus != 0
	if level == i.irqHigh {
		return nil
	}
	i.irqHigh = level
	return i.irq.SetIRQ(i.IRQ, level)
}

func (i *Input) resetLocked() {
	i.deviceFeatureSel = 0
	i.driverFeatureSel = 0
	i.driverFeatures = 0
	i.queueSel = 0
	i.status = 0
	i.interruptStatus = 0
	i.irqHigh = false
	i.configSelect = 0
	i.configSubsel = 0
	i.pending = nil
	for index := range i.queues {
		i.queues[index] = queue{}
	}
}
