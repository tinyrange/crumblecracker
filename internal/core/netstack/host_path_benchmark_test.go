package netstack

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	benchmarkHostMAC  = net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x01}
	benchmarkGuestMAC = net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}
	benchmarkGuestIP  = net.IPv4(10, 42, 0, 2)
	benchmarkHostIP   = net.IPv4(10, 42, 0, 1)
)

type benchmarkTCPFrame struct {
	seq   uint32
	ack   uint32
	flags uint8
}

type benchmarkHarness struct {
	stack *NetStack
	nic   *NetworkInterface

	tcpFrames chan benchmarkTCPFrame
}

func newBenchmarkHarness(tb testing.TB) *benchmarkHarness {
	tb.Helper()

	ns := New(nil)
	if err := ns.SetGuestMAC(benchmarkGuestMAC); err != nil {
		tb.Fatal(err)
	}
	if err := ns.SetHostMAC(benchmarkHostMAC); err != nil {
		tb.Fatal(err)
	}
	nic, err := ns.AttachNetworkInterface()
	if err != nil {
		tb.Fatal(err)
	}

	h := &benchmarkHarness{
		stack:     ns,
		nic:       nic,
		tcpFrames: make(chan benchmarkTCPFrame, 1024),
	}
	nic.AttachVirtioBackend(func(frame []byte) error {
		if tcp, ok := parseBenchmarkTCPFrame(frame); ok {
			select {
			case h.tcpFrames <- tcp:
			default:
			}
		}
		return nil
	})
	tb.Cleanup(func() {
		_ = ns.Close()
	})
	return h
}

func TestChecksumValidationIsOptIn(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enable  bool
		wantPkt uint64
	}{
		{name: "defaultDisabled", wantPkt: 1},
		{name: "enabled", enable: true, wantPkt: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newBenchmarkHarness(t)
			h.stack.SetChecksumValidationEnabled(tt.enable)

			var packets atomic.Uint64
			if err := h.stack.BindUDPCallback(":10053", func(_ *udpCallbackEndpoint, data []byte, _ net.UDPAddr) {
				if len(data) != 32 {
					t.Fatalf("payload size = %d, want 32", len(data))
				}
				packets.Add(1)
			}); err != nil {
				t.Fatal(err)
			}

			frame := buildBenchmarkUDPFrame(40200, 10053, benchmarkPayload(32))
			frame[len(frame)-1] ^= 0xff
			if err := h.nic.DeliverGuestPacket(frame, true); err != nil {
				t.Fatal(err)
			}

			if got := packets.Load(); got != tt.wantPkt {
				t.Fatalf("delivered packets = %d, want %d", got, tt.wantPkt)
			}
		})
	}
}

func BenchmarkHostPathUDPReadFrom(b *testing.B) {
	for _, size := range []int{64, 512, 1400} {
		b.Run(payloadBenchmarkName(size), func(b *testing.B) {
			h := newBenchmarkHarness(b)
			pc, err := h.stack.ListenPacketInternal("udp", ":10053")
			if err != nil {
				b.Fatal(err)
			}

			var readWG sync.WaitGroup
			readWG.Add(1)
			go func() {
				defer readWG.Done()
				buf := make([]byte, 2048)
				for {
					if _, _, err := pc.ReadFrom(buf); err != nil {
						return
					}
				}
			}()
			b.Cleanup(func() {
				_ = pc.Close()
				readWG.Wait()
			})

			payload := benchmarkPayload(size)
			frame := buildBenchmarkUDPFrame(40200, 10053, payload)

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := h.nic.DeliverGuestPacket(frame, true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkHostPathUDPCallback(b *testing.B) {
	for _, size := range []int{64, 512, 1400} {
		b.Run(payloadBenchmarkName(size), func(b *testing.B) {
			h := newBenchmarkHarness(b)
			var packets atomic.Uint64
			if err := h.stack.BindUDPCallback(":10053", func(_ *udpCallbackEndpoint, data []byte, _ net.UDPAddr) {
				if len(data) != size {
					b.Fatalf("payload size = %d, want %d", len(data), size)
				}
				packets.Add(1)
			}); err != nil {
				b.Fatal(err)
			}

			payload := benchmarkPayload(size)
			frame := buildBenchmarkUDPFrame(40200, 10053, payload)

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := h.nic.DeliverGuestPacket(frame, true); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if got := packets.Load(); got != uint64(b.N) {
				b.Fatalf("delivered packets = %d, want %d", got, b.N)
			}
		})
	}
}

func BenchmarkHostPathTCPListenerIngress(b *testing.B) {
	for _, size := range []int{64, 512, 1400, 16 * 1024, 64 * 1024} {
		b.Run(payloadBenchmarkName(size), func(b *testing.B) {
			h := newBenchmarkHarness(b)
			conn, guestSeq, hostAck := establishBenchmarkTCPConn(b, h, 40300, 10080)

			var readWG sync.WaitGroup
			readWG.Add(1)
			go func() {
				defer readWG.Done()
				buf := make([]byte, 2048)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
			b.Cleanup(func() {
				_ = conn.Close()
				readWG.Wait()
			})

			payload := benchmarkPayload(size)
			frame := buildBenchmarkTCPFrame(40300, 10080, guestSeq, hostAck, tcpFlagACK|tcpFlagPSH, payload)

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				updateBenchmarkTCPFrame(frame, guestSeq+uint32(i*size), hostAck)
				if err := h.nic.DeliverGuestPacket(frame, true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkHostPathTCPWriteTo(b *testing.B) {
	for _, size := range []int{512, 1400, 16 * 1024, 64 * 1024} {
		b.Run(payloadBenchmarkName(size), func(b *testing.B) {
			h := newBenchmarkHarness(b)
			conn, _, _ := establishBenchmarkTCPConn(b, h, 40300, 10080)
			tcpConn := conn.(*tcpConn)
			writer := &benchmarkCountingWriter{}
			done := make(chan error, 1)
			go func() {
				_, err := tcpConn.WriteTo(writer)
				done <- err
			}()

			payload := benchmarkPayload(size)

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tcpConn.recvBuf <- retainPayload(payload)
			}
			b.StopTimer()
			tcpConn.recvBuf <- nil
			if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				b.Fatal(err)
			}
			_ = conn.Close()
			if got := writer.bytes.Load(); got != uint64(b.N*size) {
				b.Fatalf("writer bytes = %d, want %d", got, b.N*size)
			}
			b.ReportMetric(float64(writer.writes.Load())/float64(b.N), "writes/pkt")
		})
	}
}

type benchmarkCountingWriter struct {
	bytes  atomic.Uint64
	writes atomic.Uint64
}

func (w *benchmarkCountingWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	w.bytes.Add(uint64(len(p)))
	return len(p), nil
}

func establishBenchmarkTCPConn(
	b *testing.B,
	h *benchmarkHarness,
	srcPort, dstPort uint16,
) (net.Conn, uint32, uint32) {
	b.Helper()

	ln, err := h.stack.ListenInternal("tcp", net.JoinHostPort("", itoa(int(dstPort))))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = ln.Close()
	})

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	const guestInitialSeq = 1_000_000
	syn := buildBenchmarkTCPFrame(srcPort, dstPort, guestInitialSeq, 0, tcpFlagSYN, nil)
	if err := h.nic.DeliverGuestPacket(syn, true); err != nil {
		b.Fatal(err)
	}

	synAck := <-h.tcpFrames
	if synAck.flags&(tcpFlagSYN|tcpFlagACK) != tcpFlagSYN|tcpFlagACK {
		b.Fatalf("handshake frame flags = 0x%02x, want SYN|ACK", synAck.flags)
	}

	guestSeq := uint32(guestInitialSeq + 1)
	hostAck := synAck.seq + 1
	ack := buildBenchmarkTCPFrame(srcPort, dstPort, guestSeq, hostAck, tcpFlagACK, nil)
	if err := h.nic.DeliverGuestPacket(ack, true); err != nil {
		b.Fatal(err)
	}

	select {
	case conn := <-accepted:
		return conn, guestSeq, hostAck
	case <-time.After(2 * time.Second):
		b.Fatal("listener did not accept benchmark TCP connection")
		return nil, 0, 0
	}
}

func buildBenchmarkUDPFrame(srcPort, dstPort uint16, payload []byte) []byte {
	return buildBenchmarkUDPFrameTo(srcPort, dstPort, benchmarkHostIP, payload)
}

func buildBenchmarkUDPFrameTo(srcPort, dstPort uint16, dstIP net.IP, payload []byte) []byte {
	frame := make([]byte, ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+len(payload))
	buildEthernetHeaderInto(frame[:ethernetHeaderLen], mustBenchmarkMAC(benchmarkHostMAC), mustBenchmarkMAC(benchmarkGuestMAC), etherTypeIPv4)

	udp := frame[ethernetHeaderLen+ipv4HeaderLen:]
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpHeaderLen+len(payload)))
	copy(udp[udpHeaderLen:], payload)
	binary.BigEndian.PutUint16(udp[6:8], 0)
	binary.BigEndian.PutUint16(udp[6:8], udpChecksum(benchmarkGuestIP, dstIP, udp))

	buildIPv4HeaderInto(frame[ethernetHeaderLen:ethernetHeaderLen+ipv4HeaderLen], benchmarkGuestIP, dstIP, udpProtocolNumber, len(udp))
	return frame
}

func buildBenchmarkTCPFrame(srcPort, dstPort uint16, seq, ack uint32, flags uint16, payload []byte) []byte {
	frame := make([]byte, ethernetHeaderLen+ipv4HeaderLen+tcpHeaderLen+len(payload))
	buildEthernetHeaderInto(frame[:ethernetHeaderLen], mustBenchmarkMAC(benchmarkHostMAC), mustBenchmarkMAC(benchmarkGuestMAC), etherTypeIPv4)

	tcp := frame[ethernetHeaderLen+ipv4HeaderLen:]
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], dstPort)
	tcp[12] = byte(tcpHeaderLen/4) << 4
	tcp[13] = byte(flags)
	binary.BigEndian.PutUint16(tcp[14:16], 0xffff)
	copy(tcp[tcpHeaderLen:], payload)

	buildIPv4HeaderInto(frame[ethernetHeaderLen:ethernetHeaderLen+ipv4HeaderLen], benchmarkGuestIP, benchmarkHostIP, tcpProtocolNumber, len(tcp))
	updateBenchmarkTCPFrame(frame, seq, ack)
	return frame
}

func updateBenchmarkTCPFrame(frame []byte, seq, ack uint32) {
	tcp := frame[ethernetHeaderLen+ipv4HeaderLen:]
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], tcpChecksum(benchmarkGuestIP, benchmarkHostIP, tcp))
}

func parseBenchmarkTCPFrame(frame []byte) (benchmarkTCPFrame, bool) {
	if len(frame) < ethernetHeaderLen+ipv4HeaderLen+tcpHeaderLen {
		return benchmarkTCPFrame{}, false
	}
	if etherType(binary.BigEndian.Uint16(frame[12:14])) != etherTypeIPv4 {
		return benchmarkTCPFrame{}, false
	}
	ip := frame[ethernetHeaderLen:]
	if protocolNumber(ip[9]) != tcpProtocolNumber {
		return benchmarkTCPFrame{}, false
	}
	ihl := int(ip[0]&0x0f) * 4
	if len(ip) < ihl+tcpHeaderLen {
		return benchmarkTCPFrame{}, false
	}
	tcp := ip[ihl:]
	return benchmarkTCPFrame{
		seq:   binary.BigEndian.Uint32(tcp[4:8]),
		ack:   binary.BigEndian.Uint32(tcp[8:12]),
		flags: tcp[13],
	}, true
}

func benchmarkPayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	return payload
}

func payloadBenchmarkName(size int) string {
	return itoa(size) + "B"
}

func mustBenchmarkMAC(mac net.HardwareAddr) macAddr {
	value, ok := macToUint64(mac)
	if !ok {
		panic("invalid benchmark MAC")
	}
	return value
}
