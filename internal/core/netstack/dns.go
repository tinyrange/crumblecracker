package netstack

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
)

const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
	dnsTypeSRV  = 33
	dnsClassIN  = 1

	dnsRCodeFormatError = 1
	dnsRCodeNameError   = 3
)

type dnsResolver func(q dnsQuestion) ([]dnsResource, []dnsResource, error)

type dnsServer struct {
	pc      net.PacketConn
	lookup  func(name string) (string, error)
	resolve dnsResolver

	closeOnce sync.Once
	done      chan struct{}
}

func newDNSServer(pc net.PacketConn, lookup func(name string) (string, error), resolve dnsResolver) *dnsServer {
	s := &dnsServer{
		pc:      pc,
		lookup:  lookup,
		resolve: resolve,
		done:    make(chan struct{}),
	}
	go s.serve()
	return s
}

func (s *dnsServer) close() {
	s.closeOnce.Do(func() {
		_ = s.pc.Close()
		<-s.done
	})
}

func (s *dnsServer) serve() {
	defer close(s.done)

	var buf [1500]byte
	for {
		n, addr, err := s.pc.ReadFrom(buf[:])
		if err != nil {
			return
		}
		resp := buildDNSResponse(buf[:n], s.lookup, s.resolve)
		if len(resp) == 0 {
			continue
		}
		_, _ = s.pc.WriteTo(resp, addr)
	}
}

func (ns *NetStack) StopDNSServer() {
	if ns.dnsServer == nil {
		return
	}
	srv := ns.dnsServer
	ns.dnsServer = nil
	srv.close()
}

type dnsQuestion struct {
	nameStart int
	nameEnd   int
	name      string
	qtype     uint16
	qclass    uint16
}

type dnsResource struct {
	name      string
	nameStart int
	typ       uint16
	class     uint16
	ttl       uint32
	data      []byte
}

func buildDNSResponse(query []byte, lookup func(name string) (string, error), resolvers ...dnsResolver) []byte {
	if len(query) < 12 {
		return nil
	}
	var resolve dnsResolver
	if len(resolvers) != 0 {
		resolve = resolvers[0]
	}

	qdCount := int(binary.BigEndian.Uint16(query[4:6]))
	if qdCount == 0 {
		return buildDNSError(query, dnsRCodeFormatError)
	}

	questions := make([]dnsQuestion, 0, qdCount)
	off := 12
	for i := 0; i < qdCount; i++ {
		q, next, err := parseDNSQuestion(query, off)
		if err != nil {
			return buildDNSError(query, dnsRCodeFormatError)
		}
		questions = append(questions, q)
		off = next
	}

	answers := make([]dnsResource, 0, len(questions))
	additionals := make([]dnsResource, 0)
	rcode := uint16(0)
	for _, q := range questions {
		if q.qclass != dnsClassIN {
			continue
		}
		if q.qtype == dnsTypeA && lookup != nil {
			ipText, err := lookup(q.name)
			if err == nil && ipText != "" {
				ip4 := net.ParseIP(ipText).To4()
				if ip4 != nil {
					answers = append(answers, dnsResource{
						nameStart: q.nameStart,
						typ:       dnsTypeA,
						class:     dnsClassIN,
						ttl:       30,
						data:      append([]byte(nil), ip4...),
					})
					continue
				}
			}
			rcode = dnsRCodeNameError
		}
		if resolve == nil {
			continue
		}
		resolved, extra, err := resolve(q)
		if err != nil {
			rcode = dnsRCodeNameError
			continue
		}
		answers = append(answers, resolved...)
		additionals = append(additionals, extra...)
	}
	if len(answers) > 0 {
		rcode = 0
	}

	size := off + encodedDNSResourcesLen(answers) + encodedDNSResourcesLen(additionals)
	resp := make([]byte, size)
	copy(resp[:off], query[:off])
	flags := uint16(0x8000 | 0x0080 | rcode) // response + recursion available.
	if binary.BigEndian.Uint16(query[2:4])&0x0100 != 0 {
		flags |= 0x0100 // preserve recursion desired.
	}
	binary.BigEndian.PutUint16(resp[2:4], flags)
	binary.BigEndian.PutUint16(resp[6:8], uint16(len(answers)))
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], uint16(len(additionals)))

	out := off
	for _, ans := range answers {
		out = appendDNSResource(resp, out, ans)
	}
	for _, ans := range additionals {
		out = appendDNSResource(resp, out, ans)
	}

	return resp
}

func encodedDNSResourcesLen(resources []dnsResource) int {
	size := 0
	for _, res := range resources {
		size += encodedDNSResourceNameLen(res) + 10 + len(res.data)
	}
	return size
}

func encodedDNSResourceNameLen(res dnsResource) int {
	if res.nameStart >= 0 {
		return 2
	}
	return encodedDNSNameLen(res.name)
}

func appendDNSResource(resp []byte, off int, res dnsResource) int {
	if res.nameStart >= 0 {
		binary.BigEndian.PutUint16(resp[off:off+2], 0xc000|uint16(res.nameStart))
		off += 2
	} else {
		off = appendDNSName(resp, off, res.name)
	}
	binary.BigEndian.PutUint16(resp[off:off+2], res.typ)
	binary.BigEndian.PutUint16(resp[off+2:off+4], res.class)
	binary.BigEndian.PutUint32(resp[off+4:off+8], res.ttl)
	binary.BigEndian.PutUint16(resp[off+8:off+10], uint16(len(res.data)))
	copy(resp[off+10:off+10+len(res.data)], res.data)
	return off + 10 + len(res.data)
}

func encodedDNSNameLen(name string) int {
	if strings.Trim(name, ".") == "" {
		return 1
	}
	size := 1
	for _, label := range splitDNSLabels(name) {
		size += 1 + len(label)
	}
	return size
}

func appendDNSName(buf []byte, off int, name string) int {
	for _, label := range splitDNSLabels(name) {
		buf[off] = byte(len(label))
		off++
		copy(buf[off:off+len(label)], label)
		off += len(label)
	}
	buf[off] = 0
	return off + 1
}

func encodeDNSName(name string) ([]byte, error) {
	if strings.Trim(name, ".") == "" {
		return []byte{0}, nil
	}
	labels := splitDNSLabels(name)
	size := 1
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("dns: invalid label %q", label)
		}
		size += 1 + len(label)
	}
	buf := make([]byte, size)
	appendDNSName(buf, 0, name)
	return buf, nil
}

func splitDNSLabels(name string) []string {
	name = strings.Trim(name, ".")
	if name == "" {
		return nil
	}
	return strings.Split(name, ".")
}

func buildDNSError(query []byte, rcode uint16) []byte {
	resp := make([]byte, 12)
	copy(resp, query[:12])
	flags := uint16(0x8000 | 0x0080 | (rcode & 0x000f))
	if binary.BigEndian.Uint16(query[2:4])&0x0100 != 0 {
		flags |= 0x0100
	}
	binary.BigEndian.PutUint16(resp[2:4], flags)
	binary.BigEndian.PutUint16(resp[4:6], 0)
	binary.BigEndian.PutUint16(resp[6:8], 0)
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	return resp
}

func parseDNSQuestion(msg []byte, off int) (dnsQuestion, int, error) {
	start := off
	var b strings.Builder
	for {
		if off >= len(msg) {
			return dnsQuestion{}, 0, fmt.Errorf("dns: name exceeds packet")
		}
		labelLen := int(msg[off])
		off++
		if labelLen == 0 {
			break
		}
		if labelLen&0xc0 != 0 {
			return dnsQuestion{}, 0, fmt.Errorf("dns: compressed question name unsupported")
		}
		if labelLen > 63 || off+labelLen > len(msg) {
			return dnsQuestion{}, 0, fmt.Errorf("dns: invalid label length")
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		for _, c := range msg[off : off+labelLen] {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b.WriteByte(c)
		}
		off += labelLen
	}
	if off+4 > len(msg) {
		return dnsQuestion{}, 0, fmt.Errorf("dns: question truncated")
	}
	q := dnsQuestion{
		nameStart: start,
		nameEnd:   off,
		name:      b.String(),
		qtype:     binary.BigEndian.Uint16(msg[off : off+2]),
		qclass:    binary.BigEndian.Uint16(msg[off+2 : off+4]),
	}
	return q, off + 4, nil
}
