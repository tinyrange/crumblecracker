package virtio

import (
	"encoding/binary"
	"net"
	"strconv"
	"testing"
)

const (
	benchNetQueuePFN = 0x10
	benchNetQueue    = benchNetQueuePFN * 4096
	benchNetBufBase  = 0x20000
)

type benchNetBackend struct {
	packets int
	bytes   int
}

func (b *benchNetBackend) HandleTxPacket(packet []byte) error {
	b.packets++
	b.bytes += len(packet)
	return nil
}

func BenchmarkNetLegacyTXNotify(b *testing.B) {
	for _, size := range []int{64, 512, 1400} {
		b.Run(benchNetPayloadName(size), func(b *testing.B) {
			mem := make(testGuestMemory, 0x120000)
			backend := &benchNetBackend{}
			dev := NewNet(0, 0x1000, 11, net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}, backend)
			dev.DisableMergeRX = true
			dev.Attach(mem, &testIRQ{})
			configureLegacyNetQueue(b, dev, netQueueTX)

			payload := benchNetPayload(size)
			for i := 0; i < netQueueSize; i++ {
				buf := uint64(benchNetBufBase + i*2048)
				copy(mem[buf+netHeaderLen:], payload)
				writeDesc(mem, benchNetQueue+uint64(i)*16, buf, uint32(netHeaderLen+len(payload)), 0, 0)
				binary.LittleEndian.PutUint16(mem[benchNetQueue+16*netQueueSize+4+uint64(i)*2:], uint16(i))
			}

			availIdx := uint16(0)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				availIdx++
				binary.LittleEndian.PutUint16(mem[benchNetQueue+16*netQueueSize+2:], availIdx)
				if err := dev.WriteLegacy(16, 2, netQueueTX); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if backend.packets != b.N {
				b.Fatalf("packets = %d, want %d", backend.packets, b.N)
			}
		})
	}
}

func BenchmarkNetLegacyRXEnqueue(b *testing.B) {
	for _, size := range []int{64, 512, 1400} {
		b.Run(benchNetPayloadName(size), func(b *testing.B) {
			mem := make(testGuestMemory, 0x120000)
			dev := NewNet(0, 0x1000, 11, nil, nil)
			dev.DisableMergeRX = true
			dev.Attach(mem, &testIRQ{})
			configureLegacyNetQueue(b, dev, netQueueRX)

			for i := 0; i < netQueueSize; i++ {
				buf := uint64(benchNetBufBase + i*2048)
				writeDesc(mem, benchNetQueue+uint64(i)*16, buf, 2048, descFWrite, 0)
				binary.LittleEndian.PutUint16(mem[benchNetQueue+16*netQueueSize+4+uint64(i)*2:], uint16(i))
			}

			packet := benchNetPayload(size)
			availIdx := uint16(0)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				availIdx++
				binary.LittleEndian.PutUint16(mem[benchNetQueue+16*netQueueSize+2:], availIdx)
				if err := dev.EnqueueRxPacketOwned(packet); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if got := binary.LittleEndian.Uint16(mem[benchNetQueue+16*netQueueSize+4096+2:]); got != uint16(b.N) {
				b.Fatalf("used idx = %d, want %d", got, uint16(b.N))
			}
		})
	}
}

func configureLegacyNetQueue(b *testing.B, dev *Net, queue uint64) {
	b.Helper()
	if err := dev.WriteLegacy(14, 2, queue); err != nil {
		b.Fatal(err)
	}
	if err := dev.WriteLegacy(8, 4, benchNetQueuePFN); err != nil {
		b.Fatal(err)
	}
}

func benchNetPayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	return payload
}

func benchNetPayloadName(size int) string {
	return strconv.Itoa(size) + "B"
}
