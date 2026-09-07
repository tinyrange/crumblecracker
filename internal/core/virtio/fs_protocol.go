package virtio

import (
	"encoding/binary"
	"fmt"
)

// fuseRequest is the immutable protocol boundary between the virtqueue driver
// and the filesystem server. raw is limited to the length declared by the FUSE
// header, so filesystem operations cannot accidentally consume descriptor
// padding or bytes belonging to another request.
type fuseRequest struct {
	raw       []byte
	body      []byte
	opcode    uint32
	unique    uint64
	nodeID    uint64
	callerUID uint32
	callerGID uint32
	callerPID uint32
}

func decodeFUSERequest(raw []byte) (fuseRequest, error) {
	if len(raw) < fuseInHeaderSize {
		return fuseRequest{}, fmt.Errorf("virtio-fs short request: %d", len(raw))
	}
	request := fuseRequest{
		opcode:    binary.LittleEndian.Uint32(raw[4:8]),
		unique:    binary.LittleEndian.Uint64(raw[8:16]),
		nodeID:    binary.LittleEndian.Uint64(raw[16:24]),
		callerUID: binary.LittleEndian.Uint32(raw[24:28]),
		callerGID: binary.LittleEndian.Uint32(raw[28:32]),
		callerPID: binary.LittleEndian.Uint32(raw[32:36]),
	}
	declared := binary.LittleEndian.Uint32(raw[0:4])
	if declared < fuseInHeaderSize {
		return request, fmt.Errorf("virtio-fs invalid request length: %d", declared)
	}
	if uint64(declared) > uint64(len(raw)) {
		return request, fmt.Errorf("virtio-fs truncated request: header declares %d bytes, descriptor provides %d", declared, len(raw))
	}
	raw = raw[:declared]
	request.raw = raw
	request.body = raw[fuseInHeaderSize:]
	return request, nil
}

func (r fuseRequest) requireBody(size int, operation string) error {
	if size < 0 || len(r.body) < size {
		return fmt.Errorf("virtio-fs %s too short: need %d body bytes, have %d", operation, size, len(r.body))
	}
	return nil
}

func (r fuseRequest) bodyBytes(offset int, size uint32, operation string) ([]byte, error) {
	if offset < 0 || offset > len(r.body) || uint64(size) > uint64(len(r.body)-offset) {
		return nil, fmt.Errorf("virtio-fs %s short payload", operation)
	}
	return r.body[offset : offset+int(size)], nil
}
