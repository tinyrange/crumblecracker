package shmem

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

const (
	Magic = uint64(0x0100006d656d6873)

	StatusEmpty     = uint32(0)
	StatusRequested = uint32(1)
	StatusMapped    = uint32(2)
	StatusError     = uint32(3)

	ErrorNone         = uint32(0)
	ErrorInvalidID    = uint32(1)
	ErrorInvalidSize  = uint32(2)
	ErrorSizeConflict = uint32(3)
	ErrorInvalidGPA   = uint32(4)
	ErrorMapping      = uint32(5)

	descriptorBase = uint64(0x10)
	descriptorSize = uint64(0x20)
)

type Mapper interface {
	MapSharedMemory(mem []byte, guestPhysAddr uint64) error
}

type descriptor struct {
	id     uint32
	status uint32
	size   uint64
	gpa    uint64
	err    uint32
}

type Device struct {
	Base uint64
	Size uint64

	mu         sync.Mutex
	attachment *Attachment
	mapper     Mapper
	desc       [MaxRegions]descriptor
}

func NewDevice(base uint64, attachment *Attachment, mapper Mapper) (*Device, error) {
	if attachment == nil || mapper == nil {
		return nil, fmt.Errorf("shared memory device requires an attachment and mapper")
	}
	if base != attachment.Config().PhysAddr {
		return nil, fmt.Errorf("shared memory device address does not match attachment")
	}
	return &Device{Base: base, Size: ControlWindowSize, attachment: attachment, mapper: mapper}, nil
}

func (d *Device) Contains(addr uint64, size int) bool {
	return d != nil && size > 0 && addr >= d.Base && uint64(size) <= d.Size && addr-d.Base <= d.Size-uint64(size)
}

func (d *Device) Read(addr uint64, size int) (uint64, error) {
	if !d.Contains(addr, size) {
		return 0, fmt.Errorf("shared memory read outside control window")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	offset := addr - d.Base
	switch offset {
	case 0:
		return truncate(Magic, size), nil
	case 8:
		return truncate(uint64(MaxRegions)|uint64(12)<<32, size), nil
	}
	index, field, ok := descriptorOffset(offset, size)
	if !ok {
		return 0, fmt.Errorf("invalid shared memory register read offset=%#x size=%d", offset, size)
	}
	entry := d.desc[index]
	switch field {
	case 0:
		return truncate(uint64(entry.id), size), nil
	case 4:
		return truncate(uint64(entry.status), size), nil
	case 8:
		return truncate(entry.size, size), nil
	case 16:
		return truncate(entry.gpa, size), nil
	case 24:
		return truncate(uint64(entry.err), size), nil
	default:
		return 0, nil
	}
}

func (d *Device) Write(addr uint64, size int, value uint64) error {
	if !d.Contains(addr, size) {
		return fmt.Errorf("shared memory write outside control window")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	offset := addr - d.Base
	index, field, ok := descriptorOffset(offset, size)
	if !ok {
		return fmt.Errorf("invalid shared memory register write offset=%#x size=%d", offset, size)
	}
	entry := &d.desc[index]
	if entry.status == StatusMapped {
		return nil
	}
	switch field {
	case 0:
		if size != 4 {
			return fmt.Errorf("shared memory region ID requires a 32-bit write")
		}
		entry.id = uint32(value)
	case 4:
		if size != 4 {
			return fmt.Errorf("shared memory status requires a 32-bit write")
		}
		if uint32(value) == StatusEmpty {
			*entry = descriptor{}
			return nil
		}
		if uint32(value) != StatusRequested {
			return fmt.Errorf("invalid shared memory descriptor status %d", value)
		}
		entry.status = StatusRequested
		d.commit(entry)
	case 8:
		if size != 8 {
			return fmt.Errorf("shared memory size requires a 64-bit write")
		}
		entry.size = value
	case 16:
		if size != 8 {
			return fmt.Errorf("shared memory GPA requires a 64-bit write")
		}
		entry.gpa = value
	case 24:
		return nil
	default:
		return fmt.Errorf("write to reserved shared memory descriptor field")
	}
	return nil
}

func (d *Device) commit(entry *descriptor) {
	entry.err = ErrorNone
	fail := func(code uint32) {
		entry.status = StatusError
		entry.err = code
	}
	if entry.id == 0 {
		fail(ErrorInvalidID)
		return
	}
	if entry.size == 0 || entry.size%PageSize != 0 || entry.size > MaxRegionSize {
		fail(ErrorInvalidSize)
		return
	}
	if entry.gpa == 0 || entry.gpa%PageSize != 0 || entry.gpa > ^uint64(0)-(entry.size-1) {
		fail(ErrorInvalidGPA)
		return
	}
	if rangesOverlap(entry.gpa, entry.size, d.Base, d.Size) {
		fail(ErrorInvalidGPA)
		return
	}
	mem, err := d.attachment.Region(entry.id, entry.size)
	if errors.Is(err, ErrRegionSizeConflict) {
		fail(ErrorSizeConflict)
		return
	}
	if err != nil {
		fail(ErrorMapping)
		return
	}
	if err := d.mapper.MapSharedMemory(mem, entry.gpa); err != nil {
		fail(ErrorMapping)
		return
	}
	entry.status = StatusMapped
}

func rangesOverlap(a, aSize, b, bSize uint64) bool {
	return a < b+bSize && b < a+aSize
}

func descriptorOffset(offset uint64, size int) (int, uint64, bool) {
	if offset < descriptorBase {
		return 0, 0, false
	}
	relative := offset - descriptorBase
	index := relative / descriptorSize
	field := relative % descriptorSize
	if index >= MaxRegions || size != 4 && size != 8 || field+uint64(size) > descriptorSize {
		return 0, 0, false
	}
	return int(index), field, true
}

func truncate(value uint64, size int) uint64 {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], value)
	switch size {
	case 1:
		return uint64(buf[0])
	case 2:
		return uint64(binary.LittleEndian.Uint16(buf[:2]))
	case 4:
		return uint64(binary.LittleEndian.Uint32(buf[:4]))
	case 8:
		return value
	default:
		return 0
	}
}
