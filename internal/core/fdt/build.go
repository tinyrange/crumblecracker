package fdt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

const (
	headerSize  = 0x28
	version     = 17
	lastCompVer = 16
	magic       = 0xd00dfeed

	beginNodeToken = 0x1
	endNodeToken   = 0x2
	propToken      = 0x3
	endToken       = 0x9
)

func Build(root Node) ([]byte, error) {
	b := &builder{stringsOff: make(map[string]uint32)}
	if err := b.emitNode(root); err != nil {
		return nil, err
	}
	return b.finish(), nil
}

type builder struct {
	structBuf  bytes.Buffer
	strings    bytes.Buffer
	stringsOff map[string]uint32
}

func (b *builder) emitNode(n Node) error {
	b.beginNode(n.Name)

	if len(n.Properties) > 0 {
		keys := make([]string, 0, len(n.Properties))
		for name := range n.Properties {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if err := b.emitProperty(name, n.Properties[name]); err != nil {
				return err
			}
		}
	}

	for _, child := range n.Children {
		if err := b.emitNode(child); err != nil {
			return err
		}
	}

	b.endNode()
	return nil
}

func (b *builder) emitProperty(name string, prop Property) error {
	if prop.DefinedCount() == 0 {
		return fmt.Errorf("fdt property %q has no values", name)
	}
	if prop.DefinedCount() > 1 {
		return fmt.Errorf("fdt property %q has multiple value kinds", name)
	}

	var data []byte
	switch prop.Kind() {
	case "strings":
		var buf bytes.Buffer
		for _, v := range prop.Strings {
			buf.WriteString(v)
			buf.WriteByte(0)
		}
		data = buf.Bytes()
	case "u32":
		data = make([]byte, 0, len(prop.U32)*4)
		for _, v := range prop.U32 {
			var tmp [4]byte
			binary.BigEndian.PutUint32(tmp[:], v)
			data = append(data, tmp[:]...)
		}
	case "u64":
		data = make([]byte, 0, len(prop.U64)*8)
		for _, v := range prop.U64 {
			var tmp [8]byte
			binary.BigEndian.PutUint64(tmp[:], v)
			data = append(data, tmp[:]...)
		}
	case "bytes":
		data = append(data, prop.Bytes...)
	case "flag":
		data = nil
	default:
		return fmt.Errorf("fdt property %q has unsupported kind %q", name, prop.Kind())
	}

	b.property(name, data)
	return nil
}

func (b *builder) beginNode(name string) {
	b.writeToken(beginNodeToken)
	b.structBuf.WriteString(name)
	b.structBuf.WriteByte(0)
	b.padStruct()
}

func (b *builder) endNode() {
	b.writeToken(endNodeToken)
}

func (b *builder) property(name string, value []byte) {
	b.writeToken(propToken)
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(len(value)))
	b.structBuf.Write(tmp[:])
	binary.BigEndian.PutUint32(tmp[:], b.stringOffset(name))
	b.structBuf.Write(tmp[:])
	b.structBuf.Write(value)
	b.padStruct()
}

func (b *builder) finish() []byte {
	b.writeToken(endToken)
	b.padStruct()

	structBytes := b.structBuf.Bytes()
	stringsBytes := b.strings.Bytes()
	memReserve := make([]byte, 16)

	offMemReserve := headerSize
	offStruct := offMemReserve + len(memReserve)
	offStrings := offStruct + len(structBytes)
	totalSize := offStrings + len(stringsBytes)

	blob := make([]byte, totalSize)
	header := blob[:headerSize]
	binary.BigEndian.PutUint32(header[0:4], magic)
	binary.BigEndian.PutUint32(header[4:8], uint32(totalSize))
	binary.BigEndian.PutUint32(header[8:12], uint32(offStruct))
	binary.BigEndian.PutUint32(header[12:16], uint32(offStrings))
	binary.BigEndian.PutUint32(header[16:20], uint32(offMemReserve))
	binary.BigEndian.PutUint32(header[20:24], version)
	binary.BigEndian.PutUint32(header[24:28], lastCompVer)
	binary.BigEndian.PutUint32(header[28:32], 0)
	binary.BigEndian.PutUint32(header[32:36], uint32(len(stringsBytes)))
	binary.BigEndian.PutUint32(header[36:40], uint32(len(structBytes)))

	copy(blob[offMemReserve:], memReserve)
	copy(blob[offStruct:], structBytes)
	copy(blob[offStrings:], stringsBytes)
	return blob
}

func (b *builder) stringOffset(name string) uint32 {
	if off, ok := b.stringsOff[name]; ok {
		return off
	}
	off := uint32(b.strings.Len())
	b.strings.WriteString(name)
	b.strings.WriteByte(0)
	b.stringsOff[name] = off
	return off
}

func (b *builder) writeToken(token uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], token)
	b.structBuf.Write(tmp[:])
}

func (b *builder) padStruct() {
	for b.structBuf.Len()%4 != 0 {
		b.structBuf.WriteByte(0)
	}
}
