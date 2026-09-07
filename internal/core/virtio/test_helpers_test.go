package virtio

import "encoding/binary"

type testGuestMemory []byte

func (m testGuestMemory) ReadIPA(addr uint64, size int) ([]byte, error) {
	out := make([]byte, size)
	copy(out, m[addr:addr+uint64(size)])
	return out, nil
}

func (m testGuestMemory) ReadIPAInto(addr uint64, dst []byte) error {
	copy(dst, m[addr:addr+uint64(len(dst))])
	return nil
}

func (m testGuestMemory) WriteIPA(addr uint64, data []byte) error {
	copy(m[addr:addr+uint64(len(data))], data)
	return nil
}

func (m testGuestMemory) SliceIPA(addr uint64, size int) ([]byte, error) {
	return m[addr : addr+uint64(size)], nil
}

type testIRQ struct {
	line  uint32
	level bool
}

func (i *testIRQ) SetIRQ(line uint32, level bool) error {
	i.line = line
	i.level = level
	return nil
}

func writeDesc(mem testGuestMemory, off uint64, addr uint64, length uint32, flags uint16, next uint16) {
	binary.LittleEndian.PutUint64(mem[off+0:], addr)
	binary.LittleEndian.PutUint32(mem[off+8:], length)
	binary.LittleEndian.PutUint16(mem[off+12:], flags)
	binary.LittleEndian.PutUint16(mem[off+14:], next)
}
