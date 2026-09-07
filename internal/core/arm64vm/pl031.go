package arm64vm

import (
	"fmt"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	pl031DR  = 0x00
	pl031LR  = 0x08
	pl031CR  = 0x0c
	pl031RIS = 0x14
	pl031MIS = 0x18

	pl031CRStart = 1
)

type PL031 struct {
	Base      uint64
	Size      uint64
	start     time.Time
	baseUnix  int64
	control   uint32
	interrupt uint32
}

func NewPL031(base, size uint64, now time.Time) *PL031 {
	if size == 0 {
		size = RTCSize
	}
	if now.IsZero() {
		now = time.Now()
	}
	return &PL031{
		Base:     base,
		Size:     size,
		start:    now,
		baseUnix: now.Unix(),
		control:  pl031CRStart,
	}
}

func (r *PL031) Contains(addr uint64, size int) bool {
	if r == nil || size <= 0 {
		return false
	}
	return addr >= r.Base && addr+uint64(size) <= r.Base+r.Size
}

func (r *PL031) Read(addr uint64, size int) (uint64, error) {
	if r == nil {
		return 0, nil
	}
	switch addr - r.Base {
	case pl031DR:
		return truncatePL031(uint64(r.currentUnix()), size), nil
	case pl031LR:
		return truncatePL031(uint64(r.baseUnix), size), nil
	case pl031CR:
		return truncatePL031(uint64(r.control), size), nil
	case pl031RIS, pl031MIS:
		return truncatePL031(uint64(r.interrupt), size), nil
	default:
		return 0, nil
	}
}

func (r *PL031) Write(addr uint64, size int, value uint64) error {
	if r == nil {
		return nil
	}
	switch addr - r.Base {
	case pl031LR:
		r.baseUnix = int64(uint32(value))
		r.start = time.Now()
	case pl031CR:
		r.control = uint32(value) & pl031CRStart
	}
	return nil
}

func (r *PL031) DeviceTreeNode() fdt.Node {
	return fdt.Node{
		Name: fmt.Sprintf("rtc@%x", r.Base),
		Properties: map[string]fdt.Property{
			"compatible": {Strings: []string{"arm,pl031"}},
			"reg":        {U64: []uint64{r.Base, r.Size}},
			"status":     {Strings: []string{"okay"}},
		},
	}
}

func (r *PL031) currentUnix() int64 {
	if r.control&pl031CRStart == 0 {
		return r.baseUnix
	}
	return r.baseUnix + int64(time.Since(r.start)/time.Second)
}

func truncatePL031(value uint64, size int) uint64 {
	if size >= 8 {
		return value
	}
	if size <= 0 {
		return 0
	}
	return value & ((uint64(1) << (uint(size) * 8)) - 1)
}
