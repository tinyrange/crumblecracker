package virtio

import (
	"encoding/binary"
	"fmt"
)

const vsockHeaderSize = 44

const (
	vsockTypeStream = 1
)

const (
	vsockOpInvalid       = 0
	vsockOpRequest       = 1
	vsockOpResponse      = 2
	vsockOpRST           = 3
	vsockOpShutdown      = 4
	vsockOpRW            = 5
	vsockOpCreditUpdate  = 6
	vsockOpCreditRequest = 7
)

const (
	vsockShutdownRecv = 1
	vsockShutdownSend = 2
)

const (
	VSockCIDHypervisor = 0
	VSockCIDLocal      = 1
	VSockCIDHost       = 2
)

type vsockHeader struct {
	SrcCID   uint64
	DstCID   uint64
	SrcPort  uint32
	DstPort  uint32
	Len      uint32
	Type     uint16
	Op       uint16
	Flags    uint32
	BufAlloc uint32
	FwdCnt   uint32
}

func parseVsockHeader(data []byte) (vsockHeader, error) {
	if len(data) < vsockHeaderSize {
		return vsockHeader{}, fmt.Errorf("vsock header too short: %d < %d", len(data), vsockHeaderSize)
	}
	return vsockHeader{
		SrcCID:   binary.LittleEndian.Uint64(data[0:8]),
		DstCID:   binary.LittleEndian.Uint64(data[8:16]),
		SrcPort:  binary.LittleEndian.Uint32(data[16:20]),
		DstPort:  binary.LittleEndian.Uint32(data[20:24]),
		Len:      binary.LittleEndian.Uint32(data[24:28]),
		Type:     binary.LittleEndian.Uint16(data[28:30]),
		Op:       binary.LittleEndian.Uint16(data[30:32]),
		Flags:    binary.LittleEndian.Uint32(data[32:36]),
		BufAlloc: binary.LittleEndian.Uint32(data[36:40]),
		FwdCnt:   binary.LittleEndian.Uint32(data[40:44]),
	}, nil
}

func encodeVsockHeader(hdr vsockHeader) []byte {
	buf := make([]byte, vsockHeaderSize)
	binary.LittleEndian.PutUint64(buf[0:8], hdr.SrcCID)
	binary.LittleEndian.PutUint64(buf[8:16], hdr.DstCID)
	binary.LittleEndian.PutUint32(buf[16:20], hdr.SrcPort)
	binary.LittleEndian.PutUint32(buf[20:24], hdr.DstPort)
	binary.LittleEndian.PutUint32(buf[24:28], hdr.Len)
	binary.LittleEndian.PutUint16(buf[28:30], hdr.Type)
	binary.LittleEndian.PutUint16(buf[30:32], hdr.Op)
	binary.LittleEndian.PutUint32(buf[32:36], hdr.Flags)
	binary.LittleEndian.PutUint32(buf[36:40], hdr.BufAlloc)
	binary.LittleEndian.PutUint32(buf[40:44], hdr.FwdCnt)
	return buf
}
