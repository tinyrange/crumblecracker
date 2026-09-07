package amd64

import (
	"encoding/binary"
	"testing"
)

func TestPrepareBootLoadsHigherHalfELF(t *testing.T) {
	const (
		physical = uint64(0x200000)
		entry    = HigherHalfBase + physical
	)
	payload := []byte{0xf4}
	kernel := testELF(entry, physical, payload, 0x1000)
	memory := make([]byte, 8<<20)
	plan, err := PrepareBoot(memory, kernel, BootOptions{MemorySize: uint64(len(memory))})
	if err != nil {
		t.Fatal(err)
	}
	if plan.EntryGVA != entry || plan.KernelPhysicalMin != physical || plan.KernelPhysicalEnd != physical+0x1000 {
		t.Fatalf("unexpected boot plan: %+v", plan)
	}
	if memory[physical] != 0xf4 || memory[physical+1] != 0 {
		t.Fatalf("kernel segment was not loaded and zero-filled: %x %x", memory[physical], memory[physical+1])
	}
	if got := binary.LittleEndian.Uint64(memory[plan.BootInfoGPA:]); got != BootInfoMagic {
		t.Fatalf("boot info magic = %#x, want %#x", got, BootInfoMagic)
	}
}

func TestPrepareBootRejectsNonHigherHalfELF(t *testing.T) {
	kernel := testELF(0x200000, 0x200000, []byte{0xf4}, 1)
	_, err := PrepareBoot(make([]byte, 8<<20), kernel, BootOptions{MemorySize: 8 << 20})
	if err == nil {
		t.Fatal("expected non-higher-half ELF to be rejected")
	}
}

func TestPrepareBootRejectsUserspaceOSABI(t *testing.T) {
	kernel := testELF(HigherHalfBase+0x200000, 0x200000, []byte{0xf4}, 1)
	kernel[7] = byte(UserOSABI)
	_, err := PrepareBoot(make([]byte, 8<<20), kernel, BootOptions{MemorySize: 8 << 20})
	if err == nil {
		t.Fatal("expected userspace OSABI to be rejected by kernel loader")
	}
}

func testELF(entry, physical uint64, payload []byte, memorySize uint64) []byte {
	const dataOffset = 0x1000
	out := make([]byte, dataOffset+len(payload))
	copy(out, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	out[7] = byte(KernelOSABI)
	out[8] = 1
	binary.LittleEndian.PutUint16(out[16:], 2)
	binary.LittleEndian.PutUint16(out[18:], 62)
	binary.LittleEndian.PutUint32(out[20:], 1)
	binary.LittleEndian.PutUint64(out[24:], entry)
	binary.LittleEndian.PutUint64(out[32:], 64)
	binary.LittleEndian.PutUint16(out[52:], 64)
	binary.LittleEndian.PutUint16(out[54:], 56)
	binary.LittleEndian.PutUint16(out[56:], 1)
	program := out[64:]
	binary.LittleEndian.PutUint32(program[0:], 1)
	binary.LittleEndian.PutUint32(program[4:], 5)
	binary.LittleEndian.PutUint64(program[8:], dataOffset)
	binary.LittleEndian.PutUint64(program[16:], HigherHalfBase+physical)
	binary.LittleEndian.PutUint64(program[24:], physical)
	binary.LittleEndian.PutUint64(program[32:], uint64(len(payload)))
	binary.LittleEndian.PutUint64(program[40:], memorySize)
	binary.LittleEndian.PutUint64(program[48:], pageSize)
	copy(out[dataOffset:], payload)
	return out
}
