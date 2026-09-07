package netstack

import "sync"

const maxRetainedPayloadPoolSize = 256 * 1024
const byteSlicePoolDepth = 256

type byteSlicePool struct {
	ch         chan []byte
	defaultCap int
	maxCap     int
}

func newByteSlicePool(defaultCap, maxCap int) *byteSlicePool {
	return &byteSlicePool{
		ch:         make(chan []byte, byteSlicePoolDepth),
		defaultCap: defaultCap,
		maxCap:     maxCap,
	}
}

func (p *byteSlicePool) get(size int) []byte {
	if size > p.maxCap {
		return make([]byte, size)
	}
	select {
	case buf := <-p.ch:
		if cap(buf) >= size {
			return buf[:size]
		}
	default:
	}
	capacity := p.defaultCap
	if capacity < size {
		capacity = size
	}
	return make([]byte, size, capacity)
}

func (p *byteSlicePool) put(buf []byte) {
	if buf == nil || cap(buf) > p.maxCap {
		return
	}
	select {
	case p.ch <- buf[:0]:
	default:
	}
}

var retainedPayloadPool = newByteSlicePool(4096, maxRetainedPayloadPoolSize)

var proxyCopyBufferPool = sync.Pool{
	New: func() any {
		return make([]byte, 256*1024)
	},
}

// retainPayload copies packet data that must outlive the caller-owned packet.
// The copy is intentional: guest TX buffers may be released as soon as
// DeliverGuestPacket returns, while TCP/UDP readers and retransmission queues
// can hold payloads for much longer.
func retainPayload(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	if len(src) > maxRetainedPayloadPoolSize {
		dst := make([]byte, len(src))
		copy(dst, src)
		return dst
	}
	dst := retainedPayloadPool.get(len(src))
	copy(dst, src)
	return dst
}

func releasePayload(buf []byte) {
	if buf == nil || cap(buf) > maxRetainedPayloadPoolSize {
		return
	}
	retainedPayloadPool.put(buf)
}

func getProxyCopyBuffer(size int) []byte {
	buf := proxyCopyBufferPool.Get().([]byte)
	if cap(buf) < size {
		proxyCopyBufferPool.Put(buf[:cap(buf)])
		return make([]byte, size)
	}
	return buf[:size]
}

func releaseProxyCopyBuffer(buf []byte) {
	if cap(buf) != 256*1024 {
		return
	}
	proxyCopyBufferPool.Put(buf[:cap(buf)])
}
