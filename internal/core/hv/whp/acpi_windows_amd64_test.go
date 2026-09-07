//go:build windows && amd64

package whp

import (
	"encoding/binary"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
)

func TestBootMADTMarksVirtioIRQsLevelTriggered(t *testing.T) {
	body := buildBootMADT(1)
	overrides := make(map[byte]uint16)
	for offset := 28; offset+2 <= len(body); {
		length := int(body[offset+1])
		if length < 2 || offset+length > len(body) {
			t.Fatalf("invalid MADT entry at offset %d", offset)
		}
		if body[offset] == 2 && length == 10 {
			overrides[body[offset+3]] = binary.LittleEndian.Uint16(body[offset+8:])
		}
		offset += length
	}
	for irq := byte(amd64vm.RootFSIRQ); irq <= amd64vm.PointerIRQ; irq++ {
		if flags := overrides[irq]; flags != 0x000d {
			t.Fatalf("IRQ %d override flags = %#x, want active-high level-triggered", irq, flags)
		}
	}
}
