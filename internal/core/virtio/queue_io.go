package virtio

import (
	"encoding/binary"
	"fmt"
)

const maxQueueRequestBytes = 1 << 20

type queueBuffer struct {
	addr   uint64
	length uint32
	write  bool
}

func readQueueDescriptor(mem GuestMemory, q *queue, index uint16) (descriptor, error) {
	if q == nil || q.size == 0 || index >= q.size {
		return descriptor{}, fmt.Errorf("virtio descriptor index %d out of range", index)
	}
	raw, err := mem.ReadIPA(q.descAddr+uint64(index)*16, 16)
	if err != nil {
		return descriptor{}, err
	}
	return descriptor{
		addr:   binary.LittleEndian.Uint64(raw[0:8]),
		length: binary.LittleEndian.Uint32(raw[8:12]),
		flags:  binary.LittleEndian.Uint16(raw[12:14]),
		next:   binary.LittleEndian.Uint16(raw[14:16]),
	}, nil
}

func readQueueChain(mem GuestMemory, q *queue, head uint16) ([]queueBuffer, error) {
	if mem == nil {
		return nil, fmt.Errorf("virtio queue has no guest memory")
	}
	index := head
	out := make([]queueBuffer, 0, 4)
	for count := uint16(0); count < q.size; count++ {
		desc, err := readQueueDescriptor(mem, q, index)
		if err != nil {
			return nil, err
		}
		out = append(out, queueBuffer{
			addr:   desc.addr,
			length: desc.length,
			write:  desc.flags&descFWrite != 0,
		})
		if desc.flags&descFNext == 0 {
			return out, nil
		}
		index = desc.next
	}
	return nil, fmt.Errorf("virtio descriptor chain loop")
}

func readQueueRequest(mem GuestMemory, buffers []queueBuffer) ([]byte, error) {
	var total uint64
	sawWritable := false
	for _, buffer := range buffers {
		if buffer.write {
			sawWritable = true
			continue
		}
		if sawWritable {
			return nil, fmt.Errorf("virtio readable descriptor follows writable descriptor")
		}
		total += uint64(buffer.length)
		if total > uint64(^uint(0)>>1) {
			return nil, fmt.Errorf("virtio request is too large")
		}
		if total > maxQueueRequestBytes {
			return nil, fmt.Errorf("virtio request exceeds %d bytes", maxQueueRequestBytes)
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("virtio request has no readable bytes")
	}
	out := make([]byte, int(total))
	offset := 0
	for _, buffer := range buffers {
		if buffer.write {
			break
		}
		if buffer.length == 0 {
			continue
		}
		raw, err := mem.ReadIPA(buffer.addr, int(buffer.length))
		if err != nil {
			return nil, err
		}
		copy(out[offset:], raw)
		offset += len(raw)
	}
	return out, nil
}

func writeQueueResponse(mem GuestMemory, buffers []queueBuffer, data []byte) (uint32, error) {
	offset := 0
	for _, buffer := range buffers {
		if !buffer.write || buffer.length == 0 {
			continue
		}
		count := len(data) - offset
		if count <= 0 {
			break
		}
		if count > int(buffer.length) {
			count = int(buffer.length)
		}
		if err := mem.WriteIPA(buffer.addr, data[offset:offset+count]); err != nil {
			return uint32(offset), err
		}
		offset += count
	}
	if offset != len(data) {
		return uint32(offset), fmt.Errorf("virtio response needs %d writable bytes, has %d", len(data), offset)
	}
	return uint32(offset), nil
}

func readAvailableIndex(mem GuestMemory, q *queue) (uint16, error) {
	raw, err := mem.ReadIPA(q.availAddr+2, 2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(raw), nil
}

func readAvailableHead(mem GuestMemory, q *queue, index uint16) (uint16, error) {
	raw, err := mem.ReadIPA(q.availAddr+4+uint64(index%q.size)*2, 2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(raw), nil
}

func writeQueueUsed(mem GuestMemory, q *queue, head uint16, usedLen uint32) error {
	slot := q.usedIdx % q.size
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(head))
	binary.LittleEndian.PutUint32(raw[4:8], usedLen)
	if err := mem.WriteIPA(q.usedAddr+4+uint64(slot)*8, raw); err != nil {
		return err
	}
	q.usedIdx++
	binary.LittleEndian.PutUint16(raw[0:2], q.usedIdx)
	return mem.WriteIPA(q.usedAddr+2, raw[:2])
}
