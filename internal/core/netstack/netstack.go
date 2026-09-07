// Package raw implements a tiny, purpose-built, in-VM L2/L3 network stack.
//
// The goals are:
//   - Minimal correctness for ARP, IPv4, ICMP, UDP, and a very small TCP
//     subset sufficient for inbound connections to a handful of services.
//   - Zero external dependencies beyond the project itself and stdlib.
//   - Explicit memory management: packet/frame buffers are drawn from small
//     sync.Pools to reduce allocations.
//
// Notes and limitations:
//   - No IPv6 support.
//   - No IP fragmentation/reassembly.
//   - Very small portion of TCP is implemented (SYN/ACK/FIN, no retransmits,
//     no congestion control, no window scaling, no options beyond header size).
//   - MAC learning is simplistic: records latest observed source MAC.
//   - Certain counters and debug helpers are best effort only.
package netstack

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/pcap"
)

////////////////////////////////////////////////////////////////////////////////
// Top-level constants and protocol numbers.
////////////////////////////////////////////////////////////////////////////////

type etherType uint16

// EtherTypes we care about.
const (
	etherTypeIPv4   etherType = 0x0800
	etherTypeIPv6   etherType = 0x86DD
	etherTypeARP    etherType = 0x0806
	etherTypeCustom etherType = 0x1234
)

func (e etherType) String() string {
	switch e {
	case etherTypeIPv4:
		return "ipv4"
	case etherTypeIPv6:
		return "ipv6"
	case etherTypeARP:
		return "arp"
	case etherTypeCustom:
		return "custom"
	}
	return fmt.Sprintf("unknown ether type 0x%04x", uint16(e))
}

type protocolNumber uint8

// Basic protocol numbers for IPv4's Protocol field.
const (
	tcpProtocolNumber protocolNumber = 6
	udpProtocolNumber protocolNumber = 17
	icmpProtocol      protocolNumber = 1
)

func (p protocolNumber) String() string {
	switch p {
	case tcpProtocolNumber:
		return "tcp"
	case udpProtocolNumber:
		return "udp"
	case icmpProtocol:
		return "icmp"
	}
	return fmt.Sprintf("unknown protocol 0x%02x", uint8(p))
}

// ARP constants (Ethernet + IPv4).
const (
	arpHardwareEthernet = 1
	arpProtoIPv4        = 0x0800
)

// Header sizes (bytes).
const (
	tcpHeaderLen      = 20
	udpHeaderLen      = 8
	ipv4HeaderLen     = 20
	ethernetHeaderLen = 14
)

const macMask macAddr = (1 << 48) - 1
const macUnset macAddr = ^macAddr(0)

////////////////////////////////////////////////////////////////////////////////
// Defaults for the synthetic network.
////////////////////////////////////////////////////////////////////////////////

// Default network parameters matching the legacy configuration.
var (
	defaultHostIPv4    = [4]byte{10, 42, 0, 1}   // Address of the "host" (this stack)
	defaultGuestIPv4   = [4]byte{10, 42, 0, 2}   // Address expected for the guest
	defaultServiceIPv4 = [4]byte{10, 42, 0, 100} // Virtual "service" endpoint
)

////////////////////////////////////////////////////////////////////////////////
// Buffer pools for TCP, IPv4, and Ethernet frames.
// This is an allocation optimization to reduce GC churn when IO is heavy.
////////////////////////////////////////////////////////////////////////////////

const (
	defaultPacketCapacity   = 64*1024 + tcpHeaderLen
	maxEthernetFramePoolLen = 256*1024 + ethernetHeaderLen
)

var ethernetFramePool = newByteSlicePool(defaultPacketCapacity+ethernetHeaderLen, maxEthernetFramePoolLen)

func getEthernetFrameBuffer(payloadLen int) []byte {
	total := ethernetHeaderLen + payloadLen
	if total <= ethernetHeaderLen {
		total = ethernetHeaderLen
	}
	if total > maxEthernetFramePoolLen {
		return make([]byte, total)
	}
	return ethernetFramePool.get(total)
}

func putEthernetFrameBuffer(buf []byte) {
	if buf == nil {
		return
	}
	if cap(buf) > maxEthernetFramePoolLen {
		return
	}
	ethernetFramePool.put(buf)
}

////////////////////////////////////////////////////////////////////////////////
// NetStack: central struct tying together interface, routing and transport.
////////////////////////////////////////////////////////////////////////////////

type udpEndpoint interface {
	Close() error

	enqueue(data []byte, addr net.UDPAddr) error
}

// NetStack implements the ns.NetStack interface for our raw stack.
type NetStack struct {
	log *slog.Logger

	// MAC and addressing state.
	hostMAC          atomic.Uint64 // MAC of this stack ("host")
	guestMAC         atomic.Uint64 // Configured guest MAC (optional)
	observedGuestMAC atomic.Uint64 // Last source MAC seen from the guest

	hostIPv4    [4]byte // Primary address of the stack
	guestIPv4   [4]byte // Expected guest IPv4 address
	serviceIPv4 [4]byte // Special service-ip used for proxying
	hostDNSName string  // Name resolved to hostIPv4 by the embedded DNS server.

	serviceProxyEnabled bool // Forward connections destined to serviceIPv4
	hostAccessEnabled   bool // Expose host/service addresses to guest-originated flows
	allowInternet       bool // Allow DNS fallback et al
	validateChecksums   atomic.Bool
	serviceProxyPortsMu sync.RWMutex
	serviceProxyPorts   map[uint16]struct{}

	// tcpDial is used for outbound TCP proxying (transparent gateway mode).
	// It is injectable for tests.
	tcpDial func(ctx context.Context, addr *net.TCPAddr) (net.Conn, error)

	// Wire interface and optional packet capture.
	mu         sync.RWMutex
	iface      *NetworkInterface
	packetDump *pcap.Writer
	pcapMu     sync.Mutex

	// TCP state.
	tcpMu      sync.Mutex
	tcpListen  map[uint16]*tcpListener
	tcpConns   map[tcpFourTuple]*tcpConn
	randSource *rand.Rand

	// UDP state.
	udpMu      sync.RWMutex
	udpSockets map[uint16]udpEndpoint

	udpProxyMu        sync.Mutex
	udpServiceProxies map[udpProxyKey]*udpServiceProxyConn

	// Embedded DNS server (optional).
	dnsServer *dnsServer

	// Debug HTTP server.
	debugMu       sync.Mutex
	debugSrv      *http.Server
	debugListener net.Listener
	debugWG       sync.WaitGroup
	debugAddr     string

	// Simple counters.
	udpRxPackets     atomic.Uint64
	udpTxPackets     atomic.Uint64
	sourceViolations [6]atomic.Uint64
	closeOnce        sync.Once
}

// New constructs a NetStack with defaults.
func New(l *slog.Logger) *NetStack {
	if l == nil {
		l = slog.Default()
	}
	now := time.Now().UnixNano()
	stack := &NetStack{
		log:                 l,
		hostIPv4:            defaultHostIPv4,
		guestIPv4:           defaultGuestIPv4,
		serviceIPv4:         defaultServiceIPv4,
		hostDNSName:         "host.containers.internal",
		serviceProxyEnabled: true,
		hostAccessEnabled:   true,
		allowInternet:       true,
		tcpListen:           make(map[uint16]*tcpListener),
		tcpConns:            make(map[tcpFourTuple]*tcpConn),
		udpSockets:          make(map[uint16]udpEndpoint),
		udpServiceProxies:   make(map[udpProxyKey]*udpServiceProxyConn),
		randSource:          rand.New(rand.NewSource(now)),
	}
	stack.tcpDial = stack.defaultOutboundTCPDial
	stack.hostMAC.Store(uint64(macUnset))
	stack.guestMAC.Store(uint64(macUnset))
	stack.observedGuestMAC.Store(uint64(macUnset))
	return stack
}

func (ns *NetStack) defaultOutboundTCPDial(ctx context.Context, addr *net.TCPAddr) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr.String())
}

// SetOutboundTCPDialer overrides how outbound TCP connections are created for
// transparent proxying. If dial is nil, the default dialer is restored.
func (ns *NetStack) SetOutboundTCPDialer(dial func(ctx context.Context, addr *net.TCPAddr) (net.Conn, error)) {
	if dial == nil {
		ns.tcpDial = ns.defaultOutboundTCPDial
		return
	}
	ns.tcpDial = dial
}

////////////////////////////////////////////////////////////////////////////////
// Lifecycle and configuration.
////////////////////////////////////////////////////////////////////////////////

// Close tears down listeners, connections, endpoints and the debug server.
// It is best-effort and idempotent.
func (ns *NetStack) Close() error {
	ns.closeOnce.Do(func() {
		ns.StopDNSServer()

		// Debug HTTP shutdown.
		ns.debugMu.Lock()
		srv := ns.debugSrv
		ln := ns.debugListener
		ns.debugSrv = nil
		ns.debugListener = nil
		ns.debugAddr = ""
		ns.debugMu.Unlock()

		if ln != nil {
			_ = ln.Close()
		}
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := srv.Shutdown(ctx); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				slog.Error("raw: debug http shutdown", "err", err)
			}
			cancel()
		}

		// Wait on background goroutines bound to debugWG.
		ns.debugWG.Wait()

		// Detach the virtio backend before closing connections.
		//
		// Close() is used during host shutdown (timeouts, VM teardown, etc). In
		// those cases the guest may no longer have valid RX descriptors, and the
		// virtio device / memory mapping may be in the process of being torn down.
		// Avoid emitting any frames during teardown to prevent the virtio path from
		// touching guest memory.
		ns.mu.Lock()
		if ns.iface != nil {
			ns.iface.backend = nil
		}
		ns.iface = nil
		ns.mu.Unlock()

		// Close TCP listeners and connections.
		//
		// Note: tcpListener.Close and tcpConn.Close both take tcpMu to remove
		// themselves from these maps, so we must not call them while holding
		// tcpMu (deadlock). Snapshot+clear under lock, then close outside.
		var listeners []*tcpListener
		var conns []*tcpConn
		ns.tcpMu.Lock()
		if ns.tcpListen != nil {
			listeners = make([]*tcpListener, 0, len(ns.tcpListen))
			for _, l := range ns.tcpListen {
				listeners = append(listeners, l)
			}
			// Keep non-nil maps to avoid panics if any late operations race
			// (inserts into a nil map panic).
			ns.tcpListen = make(map[uint16]*tcpListener)
		}
		if ns.tcpConns != nil {
			conns = make([]*tcpConn, 0, len(ns.tcpConns))
			for _, c := range ns.tcpConns {
				conns = append(conns, c)
			}
			ns.tcpConns = make(map[tcpFourTuple]*tcpConn)
		}
		ns.tcpMu.Unlock()

		for _, l := range listeners {
			_ = l.Close()
		}
		for _, c := range conns {
			_ = c.Close()
		}

		var udpEndpoints []udpEndpoint
		ns.udpMu.Lock()
		if ns.udpSockets != nil {
			udpEndpoints = make([]udpEndpoint, 0, len(ns.udpSockets))
			for _, ep := range ns.udpSockets {
				udpEndpoints = append(udpEndpoints, ep)
			}
			ns.udpSockets = make(map[uint16]udpEndpoint)
		}
		ns.udpMu.Unlock()

		for _, ep := range udpEndpoints {
			_ = ep.Close()
		}

		var udpProxies []*udpServiceProxyConn
		ns.udpProxyMu.Lock()
		if ns.udpServiceProxies != nil {
			udpProxies = make([]*udpServiceProxyConn, 0, len(ns.udpServiceProxies))
			for _, proxy := range ns.udpServiceProxies {
				udpProxies = append(udpProxies, proxy)
			}
			ns.udpServiceProxies = make(map[udpProxyKey]*udpServiceProxyConn)
		}
		ns.udpProxyMu.Unlock()
		for _, proxy := range udpProxies {
			_ = proxy.conn.Close()
			<-proxy.done
		}

		// Stop packet capture.
		ns.pcapMu.Lock()
		ns.mu.Lock()
		ns.packetDump = nil
		ns.mu.Unlock()
		ns.pcapMu.Unlock()
	})
	return nil
}

// SetGuestMAC sets the expected guest MAC for filtering and transmission.
func (ns *NetStack) SetGuestMAC(mac net.HardwareAddr) error {
	if mac == nil {
		ns.guestMAC.Store(uint64(macUnset))
		return nil
	}
	if len(mac) != 6 {
		return fmt.Errorf("invalid MAC address length: %d", len(mac))
	}
	value, ok := macToUint64(mac)
	if !ok {
		return fmt.Errorf("invalid MAC address length: %d", len(mac))
	}
	ns.guestMAC.Store(uint64(value))
	return nil
}

// SetHostMAC sets the MAC address used by the synthetic host side of the
// network. It must be called before attaching the network interface.
func (ns *NetStack) SetHostMAC(mac net.HardwareAddr) error {
	if len(mac) != 6 {
		return fmt.Errorf("invalid MAC address length: %d", len(mac))
	}
	if isBroadcast(mac) || mac[0]&1 != 0 {
		return fmt.Errorf("host MAC %s is not a unicast address", mac.String())
	}
	value, ok := macToUint64(mac)
	if !ok {
		return fmt.Errorf("invalid MAC address length: %d", len(mac))
	}
	ns.hostMAC.Store(uint64(value))
	return nil
}

// SetGuestIPv4 sets the guest IPv4 address expected by synthetic host-side
// connections such as port forwards.
func (ns *NetStack) SetGuestIPv4(ip net.IP) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("guest ip %q is not an ipv4 address", ip.String())
	}
	copy(ns.guestIPv4[:], ip4)
	return nil
}

// SetServiceProxyEnabled toggles the localhost proxy feature for TCP flows
// addressed to serviceIPv4.
func (ns *NetStack) SetServiceProxyEnabled(enabled bool) {
	ns.serviceProxyEnabled = enabled
}

// SetHostAccessEnabled toggles guest-originated access to synthetic host and
// service addresses. DNS and outbound internet proxying remain available.
func (ns *NetStack) SetHostAccessEnabled(enabled bool) {
	ns.hostAccessEnabled = enabled
}

// SetAllowedServiceProxyPorts permits selected serviceIPv4 ports even when
// general host access is disabled.
func (ns *NetStack) SetAllowedServiceProxyPorts(ports []int) {
	ns.serviceProxyPortsMu.Lock()
	defer ns.serviceProxyPortsMu.Unlock()
	if len(ports) == 0 {
		ns.serviceProxyPorts = nil
		return
	}
	allowed := make(map[uint16]struct{}, len(ports))
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			continue
		}
		allowed[uint16(port)] = struct{}{}
	}
	if len(allowed) == 0 {
		ns.serviceProxyPorts = nil
		return
	}
	ns.serviceProxyPorts = allowed
}

// AllowServiceProxyPort permits one serviceIPv4 port while preserving the
// current allowlist.
func (ns *NetStack) AllowServiceProxyPort(port int) {
	if port <= 0 || port > 65535 {
		return
	}
	ns.serviceProxyPortsMu.Lock()
	defer ns.serviceProxyPortsMu.Unlock()
	if ns.serviceProxyPorts == nil {
		ns.serviceProxyPorts = make(map[uint16]struct{}, 1)
	}
	ns.serviceProxyPorts[uint16(port)] = struct{}{}
}

// SetInternetAccessEnabled toggles access to real DNS lookups, etc.
func (ns *NetStack) SetInternetAccessEnabled(enabled bool) {
	ns.allowInternet = enabled
}

// SetChecksumValidationEnabled toggles inbound IPv4, ICMP, UDP, and TCP
// checksum validation. Validation is disabled by default because the managed
// guest path is a same-host virtual link where the checksum cost is usually
// higher than its value as a corruption detector.
func (ns *NetStack) SetChecksumValidationEnabled(enabled bool) {
	ns.validateChecksums.Store(enabled)
}

// SetHostDNSName configures the synthetic DNS name for the host computer.
// Empty names restore the default name.
func (ns *NetStack) SetHostDNSName(name string) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" {
		name = "host.containers.internal"
	}
	ns.hostDNSName = name
}

////////////////////////////////////////////////////////////////////////////////
// Packet capture (pcap).
////////////////////////////////////////////////////////////////////////////////

// OpenPacketCapture enables streaming packet capture to the given writer.
func (ns *NetStack) OpenPacketCapture(out io.Writer) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	writer := pcap.NewWriter(out)
	if err := writer.WriteFileHeader(8192, pcap.LinkTypeEthernet); err != nil {
		return fmt.Errorf("write pcap header: %w", err)
	}
	ns.packetDump = writer
	return nil
}

func (ns *NetStack) writePacketCapture(data []byte) {
	ns.pcapMu.Lock()
	defer ns.pcapMu.Unlock()

	ns.mu.RLock()
	writer := ns.packetDump
	ns.mu.RUnlock()

	if writer == nil {
		return
	}

	if err := writer.WritePacket(pcap.CaptureInfo{
		Timestamp:     time.Now(),
		CaptureLength: len(data),
		Length:        len(data),
	}, data); err != nil {
		ns.log.Warn("pcap: write frame failed", "err", err)
	}
}

////////////////////////////////////////////////////////////////////////////////
// Interface attachment and IO glue.
////////////////////////////////////////////////////////////////////////////////

// NetworkInterface is the concrete virtio-like interface that the guest uses
// to deliver and receive frames. It satisfies ns.NetworkInterface.
type NetworkInterface struct {
	stack   *NetStack
	backend func(frame []byte) error // Provided by VirtIO backend driver
}

// AttachNetworkInterface binds a new interface to the stack.
//
// The returned object is used by the hypervisor side to deliver packets.
func (ns *NetStack) AttachNetworkInterface() (*NetworkInterface, error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	if ns.iface != nil {
		return nil, fmt.Errorf("network interface already attached")
	}

	if !macIsSet(macAddr(ns.guestMAC.Load())) {
		return nil, errors.New("guest mac must be configured before attaching interface")
	}

	if !macIsSet(macAddr(ns.hostMAC.Load())) {
		hostMAC := make([]byte, 6)
		if _, err := cryptoRand.Read(hostMAC); err != nil {
			return nil, fmt.Errorf("generate host mac: %w", err)
		}
		hostMAC[0] = (hostMAC[0] | 2) &^ 1 // Locally administered unicast.
		value, _ := macToUint64(hostMAC)
		ns.hostMAC.Store(uint64(value))
	}

	iface := &NetworkInterface{stack: ns}
	ns.iface = iface
	return iface, nil
}

// AttachVirtioBackend sets the transmit callback to the hypervisor.
//
// The frame passed to handler is borrowed and only valid until handler returns.
// A backend that queues frames asynchronously must copy the bytes before
// returning. Keeping this ownership boundary here lets the stack build packets
// directly into pooled buffers without hidden per-packet copies.
func (nic *NetworkInterface) AttachVirtioBackend(handler func(frame []byte) error) {
	nic.backend = handler
	tracef("netstack.AttachVirtioBackend", "attached=%t", handler != nil)
}

// DeliverGuestPacket is called by the hypervisor when the guest transmits.
//
// packet is borrowed from the caller. If needsCopy is true, packet storage may
// be invalid as soon as DeliverGuestPacket returns; handlers that need payloads
// after return must retain them explicitly.
func (nic *NetworkInterface) DeliverGuestPacket(
	packet []byte,
	needsCopy bool,
) error {
	if len(packet) < ethernetHeaderLen {
		tracef("netstack.DeliverGuestPacket drop tooShort", "len=%d", len(packet))
		return fmt.Errorf("packet too short: %d", len(packet))
	}
	eth := etherType(binary.BigEndian.Uint16(packet[12:14]))
	if netstackTraceEnabled {
		tracef("netstack.DeliverGuestPacket", "len=%d needsCopy=%t src=%s dst=%s ethertype=%s payloadLen=%d",
			len(packet), needsCopy,
			net.HardwareAddr(packet[6:12]).String(),
			net.HardwareAddr(packet[:6]).String(),
			eth.String(),
			len(packet)-ethernetHeaderLen,
		)
	}
	nic.stack.writePacketCapture(packet)
	return nic.stack.handleEthernetFrameWithReuse(packet, needsCopy)
}

// sendFrame transmits a frame back to the guest via the backend.
func (nic *NetworkInterface) sendFrame(frame []byte) error {
	if nic.backend == nil {
		tracef("netstack.sendFrame(nic) drop backend=nil", "len=%d", len(frame))
		return fmt.Errorf("virtio backend not attached")
	}
	if netstackTraceEnabled && len(frame) >= ethernetHeaderLen {
		eth := etherType(binary.BigEndian.Uint16(frame[12:14]))
		tracef("netstack.sendFrame(nic)", "len=%d src=%s dst=%s ethertype=%s payloadLen=%d",
			len(frame),
			net.HardwareAddr(frame[6:12]).String(),
			net.HardwareAddr(frame[:6]).String(),
			eth.String(),
			len(frame)-ethernetHeaderLen,
		)
	} else if netstackTraceEnabled {
		tracef("netstack.sendFrame(nic) too short", "len=%d", len(frame))
	}
	nic.stack.writePacketCapture(frame)
	// Ownership/lifetime: `frame` is only valid for the duration of this call.
	// Backends must not retain the slice; if they need to keep it (e.g. queue
	// asynchronously), they must make their own copy.
	return nic.backend(frame)
}

// sendFrame transmits a prebuilt Ethernet frame to the attached NIC backend.
//
// IMPORTANT: Do not hold ns.mu while calling into the backend. Some backends
// (including the inline ACK backend used in benchmarks/tests) may synchronously
// re-enter the stack by delivering packets back via DeliverGuestPacket. That
// delivery path calls handleEthernetFrame -> recordGuestMAC which takes ns.mu
// for writing. If we kept a read lock here, the write would block forever,
// deadlocking the caller.
//
// This change fixes TCP benchmark hangs caused by a read->write upgrade
// deadlock when the backend immediately feeds frames back into the stack.
func (ns *NetStack) sendFrame(frame []byte) error {
	ns.mu.RLock()
	iface := ns.iface
	ns.mu.RUnlock()
	if iface == nil {
		tracef("netstack.sendFrame(ns) drop iface=nil", "len=%d", len(frame))
		return fmt.Errorf("network interface detached")
	}
	return iface.sendFrame(frame)
}

////////////////////////////////////////////////////////////////////////////////
// Ethernet handling and MAC learning.
////////////////////////////////////////////////////////////////////////////////

func (ns *NetStack) handleEthernetFrameWithReuse(frame []byte, releaseUnsafe bool) error {
	dst := net.HardwareAddr(frame[:6])
	src := net.HardwareAddr(frame[6:12])
	etherType := etherType(binary.BigEndian.Uint16(frame[12:14]))
	payload := frame[14:]

	if netstackTraceEnabled {
		tracef("netstack.handleEthernetFrame", "src=%s dst=%s ethertype=%s payloadLen=%d releaseUnsafe=%t",
			src.String(), dst.String(), etherType.String(), len(payload), releaseUnsafe)
	}

	expectedMAC := macFromUint64(macAddr(ns.guestMAC.Load()))
	if violation := ValidateGuestSource(frame, expectedMAC, net.IP(ns.guestIPv4[:])); violation != SourceValid {
		if violation != SourceUnsupportedProtocol {
			ns.recordSourceViolation(violation, src)
		}
		return nil
	}

	ns.recordGuestMAC(src)

	// Apply simple L2 filter: accept broadcast, host MAC, and configured guest
	// MAC when present. Drop other unicast frames when a guestMAC is set.
	guestMACVal := macAddr(ns.guestMAC.Load())
	hostMACVal := macAddr(ns.hostMAC.Load())
	if macIsSet(guestMACVal) &&
		!isBroadcast(dst) &&
		!macEqualUint64(dst, hostMACVal) &&
		!macEqualUint64(dst, guestMACVal) {
		tracef("netstack.handleEthernetFrame drop L2Filter", "src=%s dst=%s guestMACSet=%t", src.String(), dst.String(), macIsSet(guestMACVal))
		return nil
	}

	switch etherType {
	case etherTypeARP:
		return ns.handleARP(src, payload)
	case etherTypeIPv4:
		return ns.handleIPv4Internal(src, payload, releaseUnsafe)
	case etherTypeCustom:
		return ns.handleCustom(src, payload)
	default:
		tracef("netstack.handleEthernetFrame drop unsupported", "ethertype=%s", etherType.String())
		return nil
	}
}

func (ns *NetStack) recordSourceViolation(violation SourceViolation, sourceMAC net.HardwareAddr) {
	if violation <= SourceValid || int(violation) >= len(ns.sourceViolations) {
		return
	}
	count := ns.sourceViolations[violation].Add(1)
	// Log the first violation and powers of two thereafter. Counters preserve
	// every event while a hostile guest cannot flood logs per packet.
	if count&(count-1) == 0 {
		ns.log.Warn("dropping guest frame with invalid source identity",
			"reason", violation.String(), "source_mac", sourceMAC.String(), "count", count)
	}
}

func isBroadcast(addr net.HardwareAddr) bool {
	for _, b := range addr {
		if b != 0xff {
			return false
		}
	}
	return true
}

type macAddr uint64

func macToUint64(mac net.HardwareAddr) (macAddr, bool) {
	if len(mac) != 6 {
		return 0, false
	}
	return macAddr((uint64(mac[0]) << 40) |
		(uint64(mac[1]) << 32) |
		(uint64(mac[2]) << 24) |
		(uint64(mac[3]) << 16) |
		(uint64(mac[4]) << 8) |
		uint64(mac[5])), true
}

func macFromUint64(v macAddr) net.HardwareAddr {
	if v == macUnset {
		return nil
	}
	v &= macMask
	var buf [6]byte
	buf[0] = byte(v >> 40)
	buf[1] = byte(v >> 32)
	buf[2] = byte(v >> 24)
	buf[3] = byte(v >> 16)
	buf[4] = byte(v >> 8)
	buf[5] = byte(v)
	return net.HardwareAddr(buf[:])
}

func macIsSet(v macAddr) bool {
	return v != macUnset
}

func macEqualUint64(addr net.HardwareAddr, mac macAddr) bool {
	if !macIsSet(mac) {
		return false
	}
	value, ok := macToUint64(addr)
	if !ok {
		return false
	}
	return value == (mac & macMask)
}

func writeMAC(dst []byte, mac macAddr) {
	if len(dst) < 6 || !macIsSet(mac) {
		return
	}
	dst[0] = byte(mac >> 40)
	dst[1] = byte(mac >> 32)
	dst[2] = byte(mac >> 24)
	dst[3] = byte(mac >> 16)
	dst[4] = byte(mac >> 8)
	dst[5] = byte(mac)
}

func (ns *NetStack) recordGuestMAC(mac net.HardwareAddr) {
	if len(mac) != 6 || isBroadcast(mac) {
		return
	}
	value, ok := macToUint64(mac)
	if !ok {
		return
	}
	host := macAddr(ns.hostMAC.Load())
	if macIsSet(host) && value == (host&macMask) {
		return
	}
	if macAddr(ns.observedGuestMAC.Load()) == value {
		return
	}
	ns.observedGuestMAC.Store(uint64(value))
	tracef("netstack.recordGuestMAC", "observed=%s", mac.String())
}

// guestMACForTransmit returns a destination MAC usable for outbound frames.
// Prefers the most recent observed MAC, falling back to configured guestMAC.
// Don't modify the returned slice.
func (ns *NetStack) guestMACForTransmit() macAddr {
	if mac := macAddr(ns.observedGuestMAC.Load()); macIsSet(mac) {
		return mac
	}
	if mac := macAddr(ns.guestMAC.Load()); macIsSet(mac) {
		return mac
	}
	return macUnset
}

////////////////////////////////////////////////////////////////////////////////
// Custom: custom protocol handler
////////////////////////////////////////////////////////////////////////////////

func (ns *NetStack) handleCustom(srcMAC net.HardwareAddr, payload []byte) error {
	ns.log.Debug(
		"custom: handle custom packet",
		"src", srcMAC.String(),
		"payload", payload,
	)

	// Respond with the same packet but with the destination MAC changed to the source MAC
	frame := make([]byte, ethernetHeaderLen+len(payload))
	copy(frame[0:6], srcMAC)
	copy(frame[6:12], macFromUint64(macAddr(ns.hostMAC.Load())))
	binary.BigEndian.PutUint16(frame[12:14], uint16(etherTypeCustom))
	copy(frame[ethernetHeaderLen:], payload)
	return ns.sendFrame(frame)
}

////////////////////////////////////////////////////////////////////////////////
// ARP (Address Resolution Protocol).
////////////////////////////////////////////////////////////////////////////////

func (ns *NetStack) handleARP(srcMAC net.HardwareAddr, payload []byte) error {
	if len(payload) < 28 {
		tracef("netstack.handleARP drop tooShort", "len=%d", len(payload))
		return fmt.Errorf("arp packet too short: %d", len(payload))
	}

	hwType := binary.BigEndian.Uint16(payload[0:2])
	protoType := binary.BigEndian.Uint16(payload[2:4])
	hwSize := payload[4]
	protoSize := payload[5]
	op := binary.BigEndian.Uint16(payload[6:8])

	// We only speak Ethernet/IPv4.
	if hwType != arpHardwareEthernet ||
		protoType != arpProtoIPv4 ||
		hwSize != 6 || protoSize != 4 {
		tracef("netstack.handleARP drop unsupported", "hwType=%d protoType=0x%x hwSize=%d protoSize=%d", hwType, protoType, hwSize, protoSize)
		return nil
	}

	senderMAC := net.HardwareAddr(payload[8:14])
	senderIP := net.IP(payload[14:18])
	targetIP := net.IP(payload[24:28])
	tracef("netstack.handleARP", "op=%d srcMAC=%s senderIP=%s targetIP=%s", op, senderMAC.String(), senderIP.String(), targetIP.String())

	// Only handle ARP requests (op=1).
	if op != 1 {
		return nil
	}

	// Respond if the request targets host IPv4 or a reachable service IPv4.
	if ipEqual(targetIP, ns.hostIPv4[:]) || (ns.serviceARPEnabled() && ipEqual(targetIP, ns.serviceIPv4[:])) {
		tracef("netstack.handleARP reply", "targetIP=%s", targetIP.String())
		return ns.sendARPReply(srcMAC, senderMAC, senderIP, targetIP)
	}
	tracef("netstack.handleARP ignore", "targetIP=%s", targetIP.String())
	return nil
}

// sendARPReply crafts a unicast ARP reply to the requester.
//
// dstMAC: destination Ethernet MAC.
// senderMAC/senderIP: fields from the ARP request.
// targetIP: IP being queried (we're answering for it).
func (ns *NetStack) sendARPReply(
	dstMAC, senderMAC net.HardwareAddr,
	senderIP, targetIP net.IP,
) error {
	tracef("netstack.sendARPReply", "dstMAC=%s senderMAC=%s senderIP=%s targetIP=%s", dstMAC.String(), senderMAC.String(), senderIP.String(), targetIP.String())
	frame := make([]byte, ethernetHeaderLen+28)
	copy(frame[0:6], dstMAC)
	host := macAddr(ns.hostMAC.Load())
	writeMAC(frame[6:12], host)
	binary.BigEndian.PutUint16(frame[12:14], uint16(etherTypeARP))

	payload := frame[ethernetHeaderLen:]
	binary.BigEndian.PutUint16(payload[0:2], arpHardwareEthernet)
	binary.BigEndian.PutUint16(payload[2:4], arpProtoIPv4)
	payload[4] = 6
	payload[5] = 4
	binary.BigEndian.PutUint16(payload[6:8], 2) // reply
	writeMAC(payload[8:14], host)
	copy(payload[14:18], targetIP.To4())
	copy(payload[18:24], senderMAC)
	copy(payload[24:28], senderIP.To4())

	return ns.sendFrame(frame)
}

////////////////////////////////////////////////////////////////////////////////
// IPv4: header parsing/building and ICMP/UDP/TCP demux.
////////////////////////////////////////////////////////////////////////////////

// ipv4Header captures the fixed 20B header + optional options and payload.
//
// BUG: Fragmentation is not supported. The Flags/Fragment Offset field is
// simply captured as-is (in "flags") and ignored by the stack.
type ipv4Header struct {
	version  uint8
	ihl      uint8
	tos      uint8
	length   uint16
	id       uint16
	flags    uint16 // includes flags and fragment offset
	ttl      uint8
	protocol protocolNumber
	checksum uint16
	src      net.IP
	dst      net.IP
	options  []byte
	payload  []byte
}

// parseIPv4Header decodes minimal IPv4 header and returns the header struct.
//
// BUG: Incoming header checksum isn't verified.
// BUG: Options are not interpreted beyond slicing them out.
func parseIPv4Header(data []byte) (ipv4Header, error) {
	if len(data) < ipv4HeaderLen {
		return ipv4Header{}, fmt.Errorf("ipv4 header too short: %d", len(data))
	}
	verIHL := data[0]
	version := verIHL >> 4
	ihl := verIHL & 0x0f
	if version != 4 {
		return ipv4Header{}, fmt.Errorf("unsupported ipv4 version: %d", version)
	}
	headerLen := int(ihl) * 4
	if len(data) < headerLen {
		return ipv4Header{}, fmt.Errorf("ipv4 header length mismatch: %d", headerLen)
	}

	h := ipv4Header{
		version:  version,
		ihl:      ihl,
		tos:      data[1],
		length:   binary.BigEndian.Uint16(data[2:4]),
		id:       binary.BigEndian.Uint16(data[4:6]),
		flags:    binary.BigEndian.Uint16(data[6:8]),
		ttl:      data[8],
		protocol: protocolNumber(data[9]),
		checksum: binary.BigEndian.Uint16(data[10:12]),
		src:      net.IP(data[12:16]),
		dst:      net.IP(data[16:20]),
	}

	if headerLen > ipv4HeaderLen {
		h.options = data[ipv4HeaderLen:headerLen]
	}
	h.payload = data[headerLen:]
	return h, nil
}

func buildEthernetHeaderInto(buf []byte, dstMac, srcMac macAddr, etherType etherType) {
	if len(buf) < ethernetHeaderLen {
		panic("buildEthernetHeaderInto: buffer too small")
	}
	writeMAC(buf[0:6], dstMac)
	writeMAC(buf[6:12], srcMac)
	binary.BigEndian.PutUint16(buf[12:14], uint16(etherType))
}

func buildIPv4HeaderInto(
	packet []byte,
	src, dst net.IP,
	protocol protocolNumber,
	payloadLen int,
) {
	if len(packet) < ipv4HeaderLen {
		panic("buildIPv4HeaderInto: buffer too small")
	}
	totalLen := ipv4HeaderLen + payloadLen

	packet[0] = byte((4 << 4) | (ipv4HeaderLen / 4)) // Version/IHL
	packet[1] = 0                                    // TOS
	binary.BigEndian.PutUint16(packet[2:4], uint16(totalLen))
	binary.BigEndian.PutUint16(packet[4:6], 0) // ID
	binary.BigEndian.PutUint16(packet[6:8], 0) // Flags/FragOff
	packet[8] = 64                             // TTL
	packet[9] = byte(protocol)
	copy(packet[12:16], src.To4())
	copy(packet[16:20], dst.To4())

	// IMPORTANT: The IPv4 header checksum must be computed with the checksum
	// field zeroed. Our callers frequently use pooled buffers; without this,
	// stale bytes can make the checksum invalid and cause receivers (gVisor)
	// to drop packets intermittently.
	packet[10] = 0
	packet[11] = 0
	check := ipv4Checksum(packet[:ipv4HeaderLen])
	binary.BigEndian.PutUint16(packet[10:12], check)
}

func ipv4Checksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i < len(data)-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

func (ns *NetStack) handleIPv4Internal(srcMAC net.HardwareAddr, payload []byte, releaseUnsafe bool) error {
	hdr, err := parseIPv4Header(payload)
	if err != nil {
		tracef("netstack.handleIPv4 parse error", "error=%v", err)
		return err
	}
	if netstackTraceEnabled {
		tracef("netstack.handleIPv4", "srcMAC=%s srcIP=%s dstIP=%s proto=%s payloadLen=%d releaseUnsafe=%t",
			srcMAC.String(), hdr.src.String(), hdr.dst.String(), hdr.protocol.String(), len(hdr.payload), releaseUnsafe)
	}

	if ns.validateChecksums.Load() {
		// A correct IPv4 header checksum results in 0 when computed over the
		// entire header, including the checksum field itself.
		headerLen := int(hdr.ihl) * 4
		if headerLen <= len(payload) {
			computed := ipv4Checksum(payload[:headerLen])
			if computed != 0 {
				tracef("netstack.handleIPv4 drop invalidHeaderChecksum", "computed=0x%04x", computed)
				return nil
			}
		}
	}

	// We act as an L3 gateway for the guest. Ethernet filtering already ensures
	// the frame is addressed to us; accept routed IPv4 packets regardless of
	// destination IP.
	//
	// We still only implement a small subset of L4 protocols; unsupported traffic
	// will be dropped below.
	if hdr.dst.To4() == nil {
		return nil
	}

	switch hdr.protocol {
	case udpProtocolNumber:
		return ns.handleUDPWithReuse(hdr, hdr.payload, releaseUnsafe)
	case tcpProtocolNumber:
		return ns.handleTCP(hdr, hdr.payload)
	case icmpProtocol:
		// slog.Info("raw: handling icmp packet", "srcIP", hdr.src.String(), "dstIP", hdr.dst.String())
		return ns.handleICMP(hdr, hdr.payload)
	default:
		tracef("netstack.handleIPv4 drop unsupported", "proto=%s", hdr.protocol.String())
		slog.Error("raw: drop unsupported ipv4 protocol", "proto", hdr.protocol)
		return nil
	}
}

////////////////////////////////////////////////////////////////////////////////
// ICMP (Echo request/reply) - minimal handling.
////////////////////////////////////////////////////////////////////////////////

func (ns *NetStack) handleICMP(h ipv4Header, payload []byte) error {
	if len(payload) < 8 {
		tracef("netstack.handleICMP drop tooShort", "len=%d", len(payload))
		return nil
	}
	typ := payload[0]
	if typ != 8 { // Echo Request
		tracef("netstack.handleICMP ignore", "type=%d", typ)
		return nil
	}
	tracef("netstack.handleICMP echoRequest", "src=%s dst=%s len=%d", h.src.String(), h.dst.String(), len(payload))

	if ns.validateChecksums.Load() {
		receivedChecksum := binary.BigEndian.Uint16(payload[2:4])
		binary.BigEndian.PutUint16(payload[2:4], 0)
		calculatedChecksum := checksum(payload)
		binary.BigEndian.PutUint16(payload[2:4], receivedChecksum)
		if receivedChecksum != calculatedChecksum {
			tracef("netstack.handleICMP drop invalidChecksum", "received=0x%04x calculated=0x%04x", receivedChecksum, calculatedChecksum)
			slog.Error("raw: drop icmp packet with invalid checksum",
				"received", fmt.Sprintf("0x%04x", receivedChecksum),
				"calculated", fmt.Sprintf("0x%04x", calculatedChecksum))
			return nil
		}
	}

	return ns.sendICMPEchoReply(h.dst, h.src, payload)
}

func (ns *NetStack) sendICMPEchoReply(src, dst net.IP, request []byte) error {
	tracef("netstack.sendICMP", "src=%s dst=%s payloadLen=%d", src.String(), dst.String(), len(request))
	frame := getEthernetFrameBuffer(ipv4HeaderLen + len(request))
	defer putEthernetFrameBuffer(frame)

	reply := frame[ethernetHeaderLen+ipv4HeaderLen : ethernetHeaderLen+ipv4HeaderLen+len(request)]
	clear(reply)
	reply[0] = 0
	reply[1] = request[1]
	copy(reply[4:], request[4:])
	check := checksum(reply)
	binary.BigEndian.PutUint16(reply[2:4], check)

	buildIPv4HeaderInto(frame[ethernetHeaderLen:ethernetHeaderLen+ipv4HeaderLen], src, dst, icmpProtocol, len(reply))

	dstMAC := ns.guestMACForTransmit()
	if !macIsSet(dstMAC) {
		return fmt.Errorf("guest mac unknown for icmp transmit")
	}
	buildEthernetHeaderInto(frame[:ethernetHeaderLen], dstMAC, macAddr(ns.hostMAC.Load()), etherTypeIPv4)

	return ns.sendFrame(frame)
}

////////////////////////////////////////////////////////////////////////////////
// UDP datapath (very small).
////////////////////////////////////////////////////////////////////////////////

type udpPacket struct {
	payload []byte
	addr    net.UDPAddr
}

type udpProxyKey struct {
	srcIP   [4]byte
	srcPort uint16
	dstPort uint16
}

type udpServiceProxyConn struct {
	stack    *NetStack
	key      udpProxyKey
	conn     *net.UDPConn
	done     chan struct{}
	lastUsed atomic.Int64
}

const udpServiceProxyIdleTimeout = 30 * time.Second

func (ns *NetStack) handleUDPWithReuse(h ipv4Header, payload []byte, releaseUnsafe bool) error {
	if len(payload) < 8 {
		tracef("netstack.udpEndpointConn drop tooShort", "len=%d", len(payload))
		return fmt.Errorf("udp packet too short: %d", len(payload))
	}

	srcPort := binary.BigEndian.Uint16(payload[0:2])
	dstPort := binary.BigEndian.Uint16(payload[2:4])

	length := binary.BigEndian.Uint16(payload[4:6])
	if length < udpHeaderLen {
		return fmt.Errorf("udp length shorter than header: %d", length)
	}

	if int(length) > len(payload) {
		return fmt.Errorf("udp length exceeds payload: %d > %d", length, len(payload))
	}

	if ns.validateChecksums.Load() && binary.BigEndian.Uint16(payload[6:8]) != 0 {
		computed := udpChecksum(h.src, h.dst, payload[:length])
		if computed != 0 {
			tracef("netstack.udpEndpointConn drop invalidChecksum", "src=%s:%d dst=%s:%d computed=0x%04x", h.src.String(), srcPort, h.dst.String(), dstPort, computed)
			return nil
		}
	}

	data := payload[8:length]
	if netstackTraceEnabled {
		tracef("netstack.udpEndpointConn", "src=%s:%d dst=%s:%d dataLen=%d releaseUnsafe=%t", h.src.String(), srcPort, h.dst.String(), dstPort, len(data), releaseUnsafe)
	}

	dstIP := h.dst.To4()
	if ns.shouldProxyService(dstIP) {
		if !ns.serviceProxyEnabled || !ns.serviceProxyAllowed(dstIP, dstPort) {
			tracef("netstack.handleUDP service proxy disabled", "src=%s:%d dst=%s:%d", h.src.String(), srcPort, h.dst.String(), dstPort)
			return nil
		}
		return ns.proxyServiceUDP(h.src.To4(), srcPort, dstPort, data)
	}

	ns.udpMu.RLock()
	ep, ok := ns.udpSockets[dstPort]
	ns.udpMu.RUnlock()
	if !ok {
		tracef("netstack.udpEndpointConn drop noSocket", "dstPort=%d", dstPort)
		return nil
	}

	addrIP := h.src.To4()
	if addrIP == nil {
		return fmt.Errorf("udp source ip is not ipv4: %v", h.src)
	}

	addr := net.UDPAddr{
		IP:   addrIP,
		Port: int(srcPort),
	}

	if err := ep.enqueue(data, addr); err != nil {
		tracef("netstack.udpEndpointConn enqueue error", "error=%v", err)
		return err
	}

	// BUG: This counter is incremented even when enqueue() drops due to
	// a full buffer (enqueue returns nil in that path). It should probably
	// reflect packets actually delivered to the application.
	ns.udpRxPackets.Add(1)
	return nil
}

func (ns *NetStack) proxyServiceUDP(srcIP net.IP, srcPort, dstPort uint16, payload []byte) error {
	if srcIP == nil {
		return fmt.Errorf("udp service proxy source ip is not ipv4")
	}
	var key udpProxyKey
	copy(key.srcIP[:], srcIP)
	key.srcPort = srcPort
	key.dstPort = dstPort

	proxy, err := ns.getUDPServiceProxy(key)
	if err != nil {
		return err
	}
	proxy.lastUsed.Store(time.Now().UnixNano())
	if _, err := proxy.conn.Write(payload); err != nil {
		return err
	}
	ns.udpRxPackets.Add(1)
	return nil
}

func (ns *NetStack) getUDPServiceProxy(key udpProxyKey) (*udpServiceProxyConn, error) {
	ns.udpProxyMu.Lock()
	if proxy, ok := ns.udpServiceProxies[key]; ok {
		ns.udpProxyMu.Unlock()
		return proxy, nil
	}
	ns.udpProxyMu.Unlock()

	addr := &net.UDPAddr{
		IP:   net.IPv4(127, 0, 0, 1),
		Port: int(key.dstPort),
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}
	proxy := &udpServiceProxyConn{
		stack: ns,
		key:   key,
		conn:  conn,
		done:  make(chan struct{}),
	}
	proxy.lastUsed.Store(time.Now().UnixNano())

	ns.udpProxyMu.Lock()
	if existing, ok := ns.udpServiceProxies[key]; ok {
		ns.udpProxyMu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	ns.udpServiceProxies[key] = proxy
	ns.udpProxyMu.Unlock()

	go proxy.readLoop()
	return proxy, nil
}

func (p *udpServiceProxyConn) readLoop() {
	defer func() {
		p.stack.udpProxyMu.Lock()
		if p.stack.udpServiceProxies[p.key] == p {
			delete(p.stack.udpServiceProxies, p.key)
		}
		p.stack.udpProxyMu.Unlock()
		_ = p.conn.Close()
		close(p.done)
	}()

	buf := make([]byte, maxEthernetFramePoolLen)
	for {
		if err := p.conn.SetReadDeadline(time.Now().Add(udpServiceProxyIdleTimeout)); err != nil {
			return
		}
		n, err := p.conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Since(time.Unix(0, p.lastUsed.Load())) < udpServiceProxyIdleTimeout {
					continue
				}
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Debug("raw: udp service proxy read failed", "port", p.key.dstPort, "err", err)
			return
		}

		frameLen := ethernetHeaderLen + ipv4HeaderLen + udpHeaderLen + n
		frame := getEthernetFrameBuffer(frameLen)
		copy(frame[ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen:], buf[:n])
		dstIP := net.IP(p.key.srcIP[:])
		err = p.stack.sendUDP(frame, p.key.dstPort, p.key.srcPort, net.IP(p.stack.serviceIPv4[:]), dstIP, n)
		putEthernetFrameBuffer(frame)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Debug("raw: udp service proxy send failed", "port", p.key.dstPort, "err", err)
			return
		}
	}
}

// sendUDP crafts and transmits a UDP packet to the guest.
func (ns *NetStack) sendUDP(
	buf []byte,
	srcPort, dstPort uint16,
	srcIP, dstIP net.IP,
	payloadLen int,
) error {
	if len(buf) < ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+payloadLen {
		tracef("netstack.sendUDPPacket bufferTooSmall", "len=%d need=%d", len(buf), ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+payloadLen)
		return fmt.Errorf("buffer too small for udp packet")
	}
	tracef("netstack.sendUDPPacket", "src=%s:%d dst=%s:%d payloadLen=%d", srcIP.String(), srcPort, dstIP.String(), dstPort, payloadLen)

	totalLen := 8 + payloadLen
	packet := buf[ethernetHeaderLen+ipv4HeaderLen : ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+payloadLen]
	binary.BigEndian.PutUint16(packet[0:2], srcPort)
	binary.BigEndian.PutUint16(packet[2:4], dstPort)
	binary.BigEndian.PutUint16(packet[4:6], uint16(totalLen))
	copy(packet[8:], buf[ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen:])

	zeroOut := packet[6:8]
	zeroOut[0] = 0
	zeroOut[1] = 0
	check := udpChecksum(srcIP, dstIP, packet)
	binary.BigEndian.PutUint16(zeroOut, check)

	buildIPv4HeaderInto(buf[ethernetHeaderLen:ethernetHeaderLen+ipv4HeaderLen], srcIP, dstIP, udpProtocolNumber, len(packet))

	dstMAC := ns.guestMACForTransmit()
	if !macIsSet(dstMAC) {
		return fmt.Errorf("guest mac unknown for udp transmit")
	}

	srcMAC := macAddr(ns.hostMAC.Load())
	if !macIsSet(srcMAC) {
		return fmt.Errorf("host mac unknown for udp transmit")
	}

	buildEthernetHeaderInto(buf[:ethernetHeaderLen], dstMAC, srcMAC, etherTypeIPv4)

	if err := ns.sendFrame(buf); err != nil {
		tracef("netstack.sendUDPPacket sendFrame failed", "error=%v", err)
		return err
	}

	ns.udpTxPackets.Add(1)
	return nil
}

// UDP checksums (with IPv4 pseudo-header).
func udpChecksum(src, dst net.IP, payload []byte) uint16 {
	ps := pseudoHeaderChecksum(src, dst, udpProtocolNumber, len(payload))
	return checksumWithInitial(payload, ps)
}

// udpTimeoutError conveys a timeout via net.Error.
type udpTimeoutError struct{}

func (udpTimeoutError) Error() string   { return "timeout" }
func (udpTimeoutError) Timeout() bool   { return true }
func (udpTimeoutError) Temporary() bool { return true }

type tcpTimeoutError struct{}

func (tcpTimeoutError) Error() string   { return "timeout" }
func (tcpTimeoutError) Timeout() bool   { return true }
func (tcpTimeoutError) Temporary() bool { return true }

// udpEndpointConn represents a bound UDP "socket" on a given port.
type udpEndpointConn struct {
	stack    *NetStack
	port     uint16
	incoming chan udpPacket
	closeCh  chan struct{}

	closed    atomic.Bool
	deadMu    sync.RWMutex
	readDead  time.Time
	writeDead time.Time
}

func newUDPEndpointConn(stack *NetStack, port uint16) *udpEndpointConn {
	return &udpEndpointConn{
		stack:    stack,
		port:     port,
		incoming: make(chan udpPacket, 32),
		closeCh:  make(chan struct{}),
	}
}

func (ep *udpEndpointConn) enqueue(data []byte, addr net.UDPAddr) error {
	if ep.closed.Load() {
		return net.ErrClosed
	}

	pkt := udpPacket{
		payload: retainPayload(data),
		addr:    addr,
	}
	select {
	case ep.incoming <- pkt:
		return nil
	case <-ep.closeCh:
		releasePayload(pkt.payload)
		return net.ErrClosed
	}

}

func (ep *udpEndpointConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if ep.closed.Load() {
		return 0, nil, net.ErrClosed
	}
	ep.deadMu.RLock()
	deadline := ep.readDead
	ep.deadMu.RUnlock()
	ch := ep.incoming

	var (
		timer   *time.Timer
		timeout <-chan time.Time
	)
	if !deadline.IsZero() {
		until := time.Until(deadline)
		if until <= 0 {
			return 0, nil, &net.OpError{Op: "read", Net: "udp", Err: udpTimeoutError{}}
		}
		timer = time.NewTimer(until)
		timeout = timer.C
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
	}

	select {
	case pkt, ok := <-ch:
		if !ok {
			return 0, nil, net.ErrClosed
		}
		n := copy(b, pkt.payload)
		releasePayload(pkt.payload)
		return n, &pkt.addr, nil
	case <-ep.closeCh:
		return 0, nil, net.ErrClosed
	case <-timeout:
		return 0, nil, &net.OpError{Op: "read", Net: "udp", Err: udpTimeoutError{}}
	}
}

func (ep *udpEndpointConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, &net.OpError{Op: "write", Net: "udp", Err: errors.New("unexpected addr type")}
	}
	if ep.closed.Load() {
		return 0, net.ErrClosed
	}
	ep.deadMu.RLock()
	dead := ep.writeDead
	ep.deadMu.RUnlock()

	if !dead.IsZero() && time.Now().After(dead) {
		return 0, &net.OpError{Op: "write", Net: "udp", Err: udpTimeoutError{}}
	}

	srcIP := net.IP(ep.stack.hostIPv4[:])
	dstIP := udpAddr.IP

	buf := make([]byte, ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+len(b))
	copy(buf[ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen:], b)

	err := ep.stack.sendUDP(buf, ep.port, uint16(udpAddr.Port), srcIP, dstIP, len(b))
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (ep *udpEndpointConn) Close() error {
	if !ep.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(ep.closeCh)
	for {
		select {
		case pkt := <-ep.incoming:
			releasePayload(pkt.payload)
		default:
			ep.stack.udpMu.Lock()
			delete(ep.stack.udpSockets, ep.port)
			ep.stack.udpMu.Unlock()
			return nil
		}
	}
}

func (ep *udpEndpointConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IP(ep.stack.hostIPv4[:]), Port: int(ep.port)}
}

func (ep *udpEndpointConn) SetDeadline(t time.Time) error {
	ep.SetReadDeadline(t)
	ep.SetWriteDeadline(t)
	return nil
}

func (ep *udpEndpointConn) SetReadDeadline(t time.Time) error {
	ep.deadMu.Lock()
	ep.readDead = t
	ep.deadMu.Unlock()
	return nil
}

func (ep *udpEndpointConn) SetWriteDeadline(t time.Time) error {
	ep.deadMu.Lock()
	ep.writeDead = t
	ep.deadMu.Unlock()
	return nil
}

var (
	_ udpEndpoint = (*udpEndpointConn)(nil)
)

////////////////////////////////////////////////////////////////////////////////
// TCP: tiny acceptor and connection state machine.
////////////////////////////////////////////////////////////////////////////////

const (
	tcpFlagFIN = 0x01
	tcpFlagSYN = 0x02
	tcpFlagRST = 0x04
	tcpFlagPSH = 0x08
	tcpFlagACK = 0x10
)

type tcpHeader struct {
	srcPort  uint16
	dstPort  uint16
	seq      uint32
	ack      uint32
	dataOff  uint8
	flags    uint16
	window   uint16
	checksum uint16
	urgent   uint16
	options  []byte
	payload  []byte
}

func parseTCPHeader(data []byte) (tcpHeader, error) {
	if len(data) < tcpHeaderLen {
		return tcpHeader{}, fmt.Errorf("tcp header too short: %d", len(data))
	}

	hdrLen := (data[12] >> 4) * 4
	if len(data) < int(hdrLen) {
		return tcpHeader{}, fmt.Errorf("tcp header length mismatch: %d", hdrLen)
	}

	h := tcpHeader{
		srcPort:  binary.BigEndian.Uint16(data[0:2]),
		dstPort:  binary.BigEndian.Uint16(data[2:4]),
		seq:      binary.BigEndian.Uint32(data[4:8]),
		ack:      binary.BigEndian.Uint32(data[8:12]),
		dataOff:  data[12],
		flags:    uint16(data[13]),
		window:   binary.BigEndian.Uint16(data[14:16]),
		checksum: binary.BigEndian.Uint16(data[16:18]),
		urgent:   binary.BigEndian.Uint16(data[18:20]),
		payload:  data[hdrLen:],
	}

	if hdrLen > tcpHeaderLen {
		h.options = data[tcpHeaderLen:hdrLen]
	}
	return h, nil
}

// Four-tuple uniquely identifies a TCP connection.
type tcpFourTuple struct {
	srcIP   [4]byte
	dstIP   [4]byte
	srcPort uint16
	dstPort uint16
}

type tcpState int

const (
	tcpStateSynRcvd tcpState = iota
	tcpStateSynSent
	tcpStateEstablished
	tcpStateFinWait
	tcpStateClosed
)

// tcpAddr implements net.Addr for our tiny TCP endpoints.
type tcpAddr struct {
	ip   net.IP
	port uint16
}

func (a *tcpAddr) Network() string { return "tcp" }
func (a *tcpAddr) String() string  { return net.JoinHostPort(a.ip.String(), itoa(int(a.port))) }

// tcpListener represents a bound port that receives inbound connections.
type tcpListener struct {
	stack *NetStack
	port  uint16

	// incoming is never closed. Use closeCh to signal shutdown.
	// This avoids panics from concurrent sends.
	incoming chan *tcpConn
	closeCh  chan struct{}

	mu     sync.Mutex
	closed bool
}

func newTCPListener(stack *NetStack, port uint16) *tcpListener {
	return &tcpListener{
		stack:    stack,
		port:     port,
		incoming: make(chan *tcpConn, 16),
		closeCh:  make(chan struct{}),
	}
}

func (l *tcpListener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.incoming:
		if !ok {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-l.closeCh:
		return nil, net.ErrClosed
	}
}

func (l *tcpListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.closeCh)
	l.mu.Unlock()

	l.stack.tcpMu.Lock()
	delete(l.stack.tcpListen, l.port)
	l.stack.tcpMu.Unlock()

	return nil
}

func (l *tcpListener) Addr() net.Addr {
	return &tcpAddr{ip: net.IP(l.stack.hostIPv4[:]), port: l.port}
}

// tcpConn is a TCP connection to the guest with full reliability support.
type tcpConn struct {
	stack         *NetStack
	listener      *tcpListener
	key           tcpFourTuple
	onEstablished func(*tcpConn)
	dialDone      chan error
	dialClosed    chan struct{}
	dialOnce      sync.Once
	localIPv4     [4]byte

	mu             sync.Mutex
	state          tcpState
	guestSeq       uint32
	hostSeq        uint32
	sendAcked      uint32
	peerWnd        uint16
	sendCond       *sync.Cond
	recvBuf        chan []byte
	readPending    []byte
	readPendingBuf []byte
	readDeadline   time.Time
	writeDeadline  time.Time
	closed         bool
	writeClosed    bool

	// Retransmission support
	sendBuf     *tcpSendBuffer   // segments awaiting ACK
	rttEst      *tcpRTTEstimator // RTT estimation for RTO
	retxTimer   *time.Timer      // retransmission timer
	retxTimerMu sync.Mutex       // protects retxTimer
	retxCount   int              // total retransmissions on this conn

	// Out-of-order receive buffering
	oooRecvBuf *tcpRecvBuffer // out-of-order segments

	// MSS and window scaling
	mss          uint16 // our MSS (send side)
	peerMSS      uint16 // peer's MSS
	peerWndScale uint8  // peer's window scale shift count
	ourWndScale  uint8  // our window scale shift count
	wndScaleOK   bool   // whether window scaling was negotiated

	// Congestion control
	congCtrl *tcpCongestionControl
	dupAcks  int // duplicate ACK counter for fast retransmit

	// Nagle algorithm
	nagleEnabled bool   // whether Nagle is active
	nagleBuf     []byte // pending small write
}

// Default MSS for Ethernet (1500 MTU - 20 IP - 20 TCP).
const defaultMSS = 1460

// Send buffer capacity (256 KB should handle most transfers).
const sendBufCapacity = 256 * 1024

// Max out-of-order segments to buffer.
const maxOOOSegments = 16

const activeOpenSynRTO = 100 * time.Millisecond

func newTCPConn(
	stack *NetStack,
	listener *tcpListener,
	key tcpFourTuple,
	guestSeq uint32,
	peerWnd uint16,
	localIPv4 [4]byte,
	onEstablished func(*tcpConn),
) *tcpConn {
	initialSeq := uint32(stack.randSource.Int31())
	c := &tcpConn{
		stack:         stack,
		listener:      listener,
		key:           key,
		localIPv4:     localIPv4,
		onEstablished: onEstablished,
		state:         tcpStateSynRcvd,
		guestSeq:      guestSeq + 1, // Expect data after SYN
		hostSeq:       initialSeq,
		sendAcked:     initialSeq,
		peerWnd:       peerWnd,
		recvBuf:       make(chan []byte, 512),

		// Retransmission
		sendBuf: newTCPSendBuffer(sendBufCapacity),
		rttEst:  newTCPRTTEstimator(),

		// OOO receive
		oooRecvBuf: newTCPRecvBuffer(maxOOOSegments),

		// MSS defaults (may be updated by options)
		mss:     defaultMSS,
		peerMSS: defaultMSS,

		// Congestion control
		congCtrl: newTCPCongestionControl(defaultMSS),

		// Nagle disabled by default (can cause deadlock with delayed ACK)
		nagleEnabled: false,
	}
	if c.peerWnd == 0 {
		c.peerWnd = 0xffff
	}
	c.sendCond = sync.NewCond(&c.mu)
	return c
}

func (c *tcpConn) completeDial(err error) {
	if c.dialDone == nil {
		return
	}
	c.dialOnce.Do(func() {
		c.dialDone <- err
		close(c.dialDone)
		close(c.dialClosed)
	})
}

// handleTCP demuxes by 4-tuple to an existing conn or establishes a new one.
func (ns *NetStack) handleTCP(h ipv4Header, payload []byte) error {
	if ns.validateChecksums.Load() {
		computed := tcpChecksum(h.src, h.dst, payload)
		if computed != 0 {
			tracef("netstack.handleTCP drop invalidChecksum", "src=%s dst=%s computed=0x%04x", h.src.String(), h.dst.String(), computed)
			return nil
		}
	}

	hdr, err := parseTCPHeader(payload)
	if err != nil {
		tracef("netstack.handleTCP parse error", "error=%v", err)
		return err
	}
	if netstackTraceEnabled {
		tracef("netstack.handleTCP segment", "src=%s:%d dst=%s:%d flags=0x%02x seq=%d ack=%d win=%d payloadLen=%d optLen=%d",
			h.src.String(), hdr.srcPort, h.dst.String(), hdr.dstPort, hdr.flags, hdr.seq, hdr.ack, hdr.window, len(hdr.payload), len(hdr.options))
	}

	key := tcpFourTuple{
		srcPort: hdr.srcPort,
		dstPort: hdr.dstPort,
	}
	copy(key.srcIP[:], h.src.To4())
	copy(key.dstIP[:], h.dst.To4())

	ns.tcpMu.Lock()
	conn, ok := ns.tcpConns[key]
	if !ok {
		// Only a SYN may open a new connection.
		if hdr.flags&tcpFlagSYN == 0 {
			tracef("netstack.handleTCP no connection drop nonSYN", "flags=0x%02x", hdr.flags)
			ns.tcpMu.Unlock()
			return nil
		}

		// Parse TCP options from SYN
		opts := parseTCPOptions(hdr.options)
		dstIP := h.dst.To4()
		allowServiceProxy := ns.serviceProxyAllowed(dstIP, hdr.dstPort)

		// Local listener present? Create a conn and complete handshake.
		if listener, ok := ns.tcpListen[hdr.dstPort]; ok {
			tracef("netstack.handleTCP new connection", "dstPort=%d", hdr.dstPort)
			conn = newTCPConn(ns, listener, key, hdr.seq, hdr.window, ns.hostIPv4, nil)
			conn.applyPeerOptions(opts)
			ns.tcpConns[key] = conn
			ns.tcpMu.Unlock()
			conn.sendSynAck()
			return nil
		}

		if !ns.hostAccessEnabled && isHostLocalIPv4(dstIP) && !allowServiceProxy {
			ns.tcpMu.Unlock()
			return ns.sendRST(h, hdr)
		}

		if netstackTraceEnabled {
			tracef("netstack.handleTCP connection attempt", "src=%s dst=%s", h.src.String(), h.dst.String())
		}

		// Proxy service connections to 127.0.0.1:dstPort if enabled.
		if ns.shouldProxyService(dstIP) {
			if !ns.serviceProxyEnabled || (!ns.hostAccessEnabled && !allowServiceProxy) {
				tracef("netstack.handleTCP proxy disabled -> reset", "dstIP=%s dstPort=%d", h.dst.String(), hdr.dstPort)
				ns.tcpMu.Unlock()
				return ns.sendRST(h, hdr)
			}
			tracef("netstack.handleTCP service proxy", "src=%s:%d dst=%s:%d", h.src.String(), hdr.srcPort, h.dst.String(), hdr.dstPort)
			onEstablished := func(c *tcpConn) {
				ns.startServiceProxy(c)
			}
			conn = newTCPConn(ns, nil, key, hdr.seq, hdr.window, ns.serviceIPv4, onEstablished)
			conn.applyPeerOptions(opts)
			ns.tcpConns[key] = conn
			ns.tcpMu.Unlock()
			conn.sendSynAck()
			return nil
		}

		// Deny internet if disabled (except to host IP).
		if !ns.allowInternet && !ipEqual(dstIP, ns.hostIPv4[:]) {
			ns.tcpMu.Unlock()
			return ns.sendRST(h, hdr)
		}

		// Transparent outbound TCP: for any non-local destination, establish a
		// synthetic TCP conn to the guest and bridge it to a real host TCP socket
		// connected to dstIP:dstPort.
		if !ipEqual(dstIP, ns.hostIPv4[:]) && !ipEqual(dstIP, ns.serviceIPv4[:]) {
			var localIPv4 [4]byte
			copy(localIPv4[:], dstIP)
			onEstablished := func(c *tcpConn) {
				ns.startOutboundTCPProxy(c)
			}
			conn = newTCPConn(ns, nil, key, hdr.seq, hdr.window, localIPv4, onEstablished)
			conn.applyPeerOptions(opts)
			ns.tcpConns[key] = conn
			ns.tcpMu.Unlock()
			conn.sendSynAck()
			return nil
		}

		// No listener and not proxyable; reset.
		ns.tcpMu.Unlock()
		return ns.sendRST(h, hdr)
	}
	ns.tcpMu.Unlock()

	if err := conn.handleSegment(h, hdr); err != nil {
		if errors.Is(err, net.ErrClosed) {
			tracef("netstack.handleTCP drop closed connection segment", "src=%s:%d dst=%s:%d", h.src.String(), hdr.srcPort, h.dst.String(), hdr.dstPort)
			return nil
		}
		return err
	}
	return nil
}

func (c *tcpConn) handleSegment(h ipv4Header, hdr tcpHeader) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return net.ErrClosed
	}
	if netstackTraceEnabled {
		tracef("netstack.tcpConn segment",
			"state=%d flags=0x%02x seq=%d ack=%d win=%d payloadLen=%d optLen=%d expectSeq=%d",
			c.state, hdr.flags, hdr.seq, hdr.ack, hdr.window, len(hdr.payload), len(hdr.options), c.guestSeq,
		)
	}

	// Track ack of our sent data.
	if hdr.flags&tcpFlagACK != 0 {
		// Record advertised receive window for flow control.
		// If window opens from zero/small, we need to wake blocked writers.
		oldWnd := c.peerWnd
		c.peerWnd = hdr.window
		windowOpened := oldWnd == 0 && hdr.window > 0
		if hdr.ack > c.sendAcked {
			// Calculate bytes acknowledged for congestion control
			bytesAcked := hdr.ack - c.sendAcked
			c.sendAcked = hdr.ack

			// Reset duplicate ACK counter on new ACK
			c.dupAcks = 0

			// Update congestion control
			if c.congCtrl != nil {
				c.congCtrl.onAck(int(bytesAcked))
			}

			// Free acknowledged segments from send buffer and get RTT sample
			if c.sendBuf != nil {
				_, rttSample, hasRTT := c.sendBuf.ack(hdr.ack)
				// Update RTT estimator with sample from non-retransmitted segment
				if hasRTT && c.rttEst != nil {
					c.rttEst.update(rttSample)
					c.rttEst.resetBackoff()
				}

				// Manage retransmit timer
				if c.sendBuf.len() > 0 {
					// Restart timer with updated RTO for remaining segments
					c.restartRetxTimer()
				} else {
					// All data acked, stop timer
					c.stopRetxTimer()

					// Flush Nagle buffer if all in-flight data is acknowledged
					if len(c.nagleBuf) > 0 {
						nagleData := c.nagleBuf
						c.nagleBuf = nil
						seq := c.hostSeq
						ack := c.guestSeq
						c.hostSeq += uint32(len(nagleData))
						c.mu.Unlock()
						// Send buffered Nagle data
						c.stack.sendTCPPacket(c.localIPv4, c.key, seq, ack, tcpFlagACK|tcpFlagPSH, nagleData)
						c.mu.Lock()
					}
				}
			}

			if c.sendCond != nil {
				c.sendCond.Broadcast()
			}
		} else if hdr.ack == c.sendAcked &&
			c.sendBuf != nil && c.sendBuf.len() > 0 &&
			len(hdr.payload) == 0 &&
			hdr.flags&(tcpFlagSYN|tcpFlagFIN|tcpFlagRST) == 0 &&
			hdr.window == oldWnd {
			// A duplicate ACK carries no data or control flags and does not
			// update the receive window. Counting request data or window updates
			// as loss signals repeatedly collapsed the congestion window during
			// pipelined HTTP transfers from Linux guests.
			c.dupAcks++
			if c.congCtrl != nil && c.congCtrl.onDupAck() {
				// 3 duplicate ACKs - trigger fast retransmit
				seg, ok := c.sendBuf.oldest()
				if ok {
					ack := c.guestSeq
					c.mu.Unlock()
					c.stack.sendTCPPacket(c.localIPv4, c.key, seg.seqStart, ack, tcpFlagACK, seg.payload)
					c.mu.Lock()
				}
			}
		}
		// If the peer ACKs beyond what we think we've sent, resync to avoid
		// stalling due to mismatched sequence tracking.
		if hdr.ack > c.hostSeq {
			c.hostSeq = hdr.ack
			c.sendAcked = hdr.ack
			if c.sendCond != nil {
				c.sendCond.Broadcast()
			}
		}

		// Wake blocked writers when window opens from zero.
		// This handles the case where the peer was flow-controlling us (win=0)
		// and now has buffer space available.
		if windowOpened && c.sendCond != nil {
			tracef("netstack.tcpConn window opened from zero", "win=%d key=%v", hdr.window, c.key)
			c.sendCond.Broadcast()
		}
	}

	switch c.state {
	case tcpStateSynSent:
		if hdr.flags&tcpFlagRST != 0 {
			c.state = tcpStateClosed
			c.mu.Unlock()
			c.completeDial(net.ErrClosed)
			c.Close()
			return nil
		}
		if hdr.flags&(tcpFlagSYN|tcpFlagACK) == (tcpFlagSYN|tcpFlagACK) && hdr.ack == c.hostSeq {
			opts := parseTCPOptions(hdr.options)
			c.applyPeerOptionsLocked(opts)
			c.guestSeq = hdr.seq + 1
			c.peerWnd = hdr.window
			c.state = tcpStateEstablished
			c.mu.Unlock()
			c.sendAck()
			c.completeDial(nil)
			return nil
		}
		c.mu.Unlock()
		return nil
	case tcpStateSynRcvd:
		if hdr.flags&tcpFlagACK != 0 {
			if hdr.ack != c.hostSeq {
				tracef("netstack.tcpConn synrcvd drop badAck", "ack=%d want=%d", hdr.ack, c.hostSeq)
				c.mu.Unlock()
				return nil
			}
			tracef("netstack.tcpConn established", "seq=%d ack=%d payloadLen=%d", hdr.seq, hdr.ack, len(hdr.payload))
			c.state = tcpStateEstablished
			cb := c.onEstablished
			listener := c.listener
			hasData := len(hdr.payload) > 0 ||
				hdr.flags&(tcpFlagFIN|tcpFlagRST) != 0
			c.mu.Unlock()
			if listener != nil {
				// Listener can be closed concurrently. Avoid blocking forever by
				// bailing out when closeCh is closed.
				select {
				case listener.incoming <- c:
				case <-listener.closeCh:
					c.Close()
				}
			} else if cb != nil {
				go cb(c)
			}
			if hasData {
				return c.handleSegment(h, hdr)
			}
			return nil
		}
	case tcpStateEstablished:
		if len(hdr.payload) > 0 {
			if hdr.seq != c.guestSeq {
				// Out-of-order segment: buffer it for later reassembly
				if seqGT(hdr.seq, c.guestSeq) && c.oooRecvBuf != nil {
					seg := tcpOOOSegment{
						seqStart: hdr.seq,
						seqEnd:   hdr.seq + uint32(len(hdr.payload)),
						payload:  retainPayload(hdr.payload),
					}
					c.oooRecvBuf.insert(seg)
				}
				c.mu.Unlock()
				// Always ACK our current receive position so the sender can
				// retransmit (duplicate ACK triggers fast retransmit).
				c.sendAck()
				return nil
			}

			// In-order segment: deliver it
			c.guestSeq += uint32(len(hdr.payload))
			data := retainPayload(hdr.payload)

			// Check OOO buffer for contiguous segments
			var contiguous [][]byte
			if c.oooRecvBuf != nil {
				contiguous = c.oooRecvBuf.collectContiguous(&c.guestSeq)
			}

			peerClosed := hdr.flags&tcpFlagFIN != 0
			writeClosed := c.writeClosed
			if peerClosed {
				// FIN consumes one sequence number after the segment payload. A
				// peer is allowed to combine its final bytes and FIN in one
				// segment, so process both before returning to the network loop.
				c.guestSeq++
				c.state = tcpStateFinWait
			}
			c.mu.Unlock()

			// Deliver all data to application
			c.enqueueData(data)
			for _, seg := range contiguous {
				c.enqueueData(seg)
			}
			if peerClosed {
				c.enqueueData(nil) // signal EOF after the final payload
				c.sendAckImmediate()
				if !writeClosed {
					c.sendFin()
				}
				return nil
			}
			// Send ACK immediately (delayed ACK can cause deadlock with Nagle)
			c.sendAck()
			return nil
		}
		if hdr.flags&tcpFlagFIN != 0 {
			c.guestSeq++
			c.state = tcpStateFinWait
			writeClosed := c.writeClosed
			c.mu.Unlock()
			c.enqueueData(nil) // signal EOF to readers
			// Send ACK immediately for FIN
			c.sendAckImmediate()
			if !writeClosed {
				c.sendFin()
			}
			return nil
		}
		c.mu.Unlock()
		if hdr.flags&tcpFlagRST != 0 {
			c.Close()
		}
		return nil
	case tcpStateFinWait:
		if hdr.flags&tcpFlagACK != 0 {
			c.state = tcpStateClosed
			c.mu.Unlock()
			c.Close()
			return nil
		}
		c.mu.Unlock()
		return nil
	default:
		c.mu.Unlock()
		return nil
	}

	c.mu.Unlock()
	return nil
}

func (c *tcpConn) enqueueData(data []byte) {
	tracef("netstack.tcpConn enqueueData", "len=%d", len(data))

	// Synchronize with Close() (which closes recvBuf) to avoid sending on a
	// closed channel.
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	// Avoid blocking the network path if readers are slow.
	select {
	case c.recvBuf <- data:
	default:
		releasePayload(data)
	}
	c.mu.Unlock()
}

// applyPeerOptions stores the peer's TCP options from the SYN segment.
func (c *tcpConn) applyPeerOptions(opts tcpOptions) {
	tracef("netstack.tcpConn applyPeerOptions", "opts=%+v", opts)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyPeerOptionsLocked(opts)
}

func (c *tcpConn) applyPeerOptionsLocked(opts tcpOptions) {
	if opts.hasMSS {
		c.peerMSS = opts.mss
	}
	if opts.hasWndScale {
		c.peerWndScale = opts.wndScale
		c.wndScaleOK = true
		// Use a moderate window scale for ourselves (7 = 128KB windows)
		c.ourWndScale = 7
	}
}

func (c *tcpConn) sendSyn() error {
	c.mu.Lock()
	seq := c.hostSeq
	c.hostSeq++
	c.mu.Unlock()

	options := buildSynAckOptions(defaultMSS, 7, true)
	return c.stack.sendTCPPacketWithOptions(c.localIPv4, c.key, seq, 0, tcpFlagSYN, nil, options)
}

func (c *tcpConn) retransmitSyn() error {
	c.mu.Lock()
	if c.closed || c.state != tcpStateSynSent {
		c.mu.Unlock()
		return net.ErrClosed
	}
	seq := c.hostSeq - 1
	c.mu.Unlock()

	options := buildSynAckOptions(defaultMSS, 7, true)
	return c.stack.sendTCPPacketWithOptions(c.localIPv4, c.key, seq, 0, tcpFlagSYN, nil, options)
}

func (c *tcpConn) sendSynAck() {
	c.mu.Lock()
	seq := c.hostSeq
	ack := c.guestSeq
	wndScaleOK := c.wndScaleOK
	ourWndScale := c.ourWndScale
	c.hostSeq++
	c.mu.Unlock()

	// Build TCP options for SYN-ACK (MSS and optionally Window Scale)
	options := buildSynAckOptions(defaultMSS, ourWndScale, wndScaleOK)
	c.stack.sendTCPPacketWithOptions(c.localIPv4, c.key, seq, ack, tcpFlagSYN|tcpFlagACK, nil, options)
}

func (c *tcpConn) sendAck() {
	c.mu.Lock()
	seq := c.hostSeq
	ack := c.guestSeq
	c.mu.Unlock()
	c.stack.sendTCPPacket(c.localIPv4, c.key, seq, ack, tcpFlagACK, nil)
}

func (c *tcpConn) sendFin() {
	c.mu.Lock()
	seq := c.hostSeq
	ack := c.guestSeq
	c.hostSeq++
	c.mu.Unlock()
	c.stack.sendTCPPacket(c.localIPv4, c.key, seq, ack, tcpFlagFIN|tcpFlagACK, nil)
}

// sendAckImmediate sends an ACK immediately.
func (c *tcpConn) sendAckImmediate() {
	c.sendAck()
}

// Read returns payload delivered by the guest. A nil buffer pushed into the
// queue signals EOF.
func (c *tcpConn) Read(b []byte) (int, error) {
	var timeout <-chan time.Time
	var timer *time.Timer
	c.mu.Lock()
	if len(c.readPending) > 0 {
		n := copy(b, c.readPending)
		if n == len(c.readPending) {
			releasePayload(c.readPendingBuf)
			c.readPending = nil
			c.readPendingBuf = nil
		} else {
			c.readPending = c.readPending[n:]
		}
		c.mu.Unlock()
		return n, nil
	}
	if !c.readDeadline.IsZero() {
		until := time.Until(c.readDeadline)
		if until <= 0 {
			c.mu.Unlock()
			return 0, &net.OpError{Op: "read", Net: "tcp", Err: tcpTimeoutError{}}
		}
		timer = time.NewTimer(until)
		timeout = timer.C
		defer func() {
			if !timer.Stop() {
				// Ensure the timer channel is drained to avoid leaking.
				select {
				case <-timer.C:
				default:
				}
			}
		}()
	}
	buf := c.recvBuf
	c.mu.Unlock()

	select {
	case data, ok := <-buf:
		if !ok {
			return 0, net.ErrClosed
		}
		if data == nil {
			return 0, io.EOF
		}
		n := copy(b, data)
		tracef("netstack.tcpConn Read", "requested=%d delivered=%d remaining=%d", len(b), n, len(data)-n)
		if n < len(data) {
			c.mu.Lock()
			if c.closed {
				releasePayload(data)
			} else {
				c.readPendingBuf = data
				c.readPending = data[n:]
			}
			c.mu.Unlock()
		} else {
			releasePayload(data)
		}
		return n, nil
	case <-timeout:
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: tcpTimeoutError{}}
	}
}

// WriteTo lets io.Copy stream queued guest payloads straight into the host
// socket. That avoids copying each TCP payload into io.Copy's intermediate
// buffer before the kernel write.
func (c *tcpConn) WriteTo(w io.Writer) (int64, error) {
	var written int64
	batch := getProxyCopyBuffer(64 * 1024)
	defer releaseProxyCopyBuffer(batch)

	for {
		data, owned, err := c.nextReadPayload()
		if err != nil {
			if err == io.EOF {
				return written, nil
			}
			return written, err
		}
		if len(data) >= cap(batch) {
			n, err := writeFull(w, data)
			written += n
			releasePayload(owned)
			if err != nil {
				return written, err
			}
			continue
		}

		batch = batch[:0]
		eofAfterBatch := false
		for {
			if len(data) > cap(batch)-len(batch) {
				n, err := writeFull(w, batch)
				written += n
				if err != nil {
					releasePayload(owned)
					return written, err
				}
				batch = batch[:0]
			}
			batch = append(batch, data...)
			releasePayload(owned)

			var ok bool
			data, owned, ok, err = c.tryNextReadPayload()
			if err != nil {
				if err == io.EOF {
					eofAfterBatch = true
					break
				}
				n, writeErr := writeFull(w, batch)
				written += n
				if writeErr != nil {
					return written, writeErr
				}
				return written, err
			}
			if !ok {
				break
			}
			if len(data) >= cap(batch) && len(batch) > 0 {
				n, err := writeFull(w, batch)
				written += n
				if err != nil {
					releasePayload(owned)
					return written, err
				}
				batch = batch[:0]
			}
			if len(data) >= cap(batch) {
				n, err := writeFull(w, data)
				written += n
				releasePayload(owned)
				if err != nil {
					return written, err
				}
				break
			}
		}

		n, err := writeFull(w, batch)
		written += n
		if err != nil {
			return written, err
		}
		if eofAfterBatch {
			return written, nil
		}
	}
}

func (c *tcpConn) nextReadPayload() ([]byte, []byte, error) {
	var timeout <-chan time.Time
	var timer *time.Timer
	c.mu.Lock()
	if len(c.readPending) > 0 {
		data := c.readPending
		owned := c.readPendingBuf
		c.readPending = nil
		c.readPendingBuf = nil
		c.mu.Unlock()
		return data, owned, nil
	}
	if !c.readDeadline.IsZero() {
		until := time.Until(c.readDeadline)
		if until <= 0 {
			c.mu.Unlock()
			return nil, nil, &net.OpError{Op: "read", Net: "tcp", Err: tcpTimeoutError{}}
		}
		timer = time.NewTimer(until)
		timeout = timer.C
		defer func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}()
	}
	buf := c.recvBuf
	c.mu.Unlock()

	select {
	case data, ok := <-buf:
		if !ok {
			return nil, nil, net.ErrClosed
		}
		if data == nil {
			return nil, nil, io.EOF
		}
		return data, data, nil
	case <-timeout:
		return nil, nil, &net.OpError{Op: "read", Net: "tcp", Err: tcpTimeoutError{}}
	}
}

func (c *tcpConn) tryNextReadPayload() ([]byte, []byte, bool, error) {
	c.mu.Lock()
	if len(c.readPending) > 0 {
		data := c.readPending
		owned := c.readPendingBuf
		c.readPending = nil
		c.readPendingBuf = nil
		c.mu.Unlock()
		return data, owned, true, nil
	}
	buf := c.recvBuf
	c.mu.Unlock()

	select {
	case data, ok := <-buf:
		if !ok {
			return nil, nil, false, net.ErrClosed
		}
		if data == nil {
			return nil, nil, false, io.EOF
		}
		return data, data, true, nil
	default:
		return nil, nil, false, nil
	}
}

func writeFull(w io.Writer, data []byte) (int64, error) {
	var written int64
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			written += int64(n)
			data = data[n:]
		}
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// Write transmits payload to the guest.
func (c *tcpConn) Write(b []byte) (int, error) {
	tracef("netstack.tcpConn Write", "len=%d", len(b))

	// Keep payloads below a typical Ethernet MTU. We don't implement IP
	// fragmentation, and some virtio backends will drop oversized frames.
	const maxPayload = 1460

	written := 0
	for written < len(b) {
		var (
			seq   uint32
			ack   uint32
			flags uint16
			chunk []byte
		)

		c.mu.Lock()
		for {
			if c.closed || c.writeClosed {
				c.mu.Unlock()
				return written, net.ErrClosed
			}
			// The host-side socket already performs congestion control for the
			// real network path. This connection is the lossless virtual hop from
			// that socket to the guest, so applying a second Reno window here can
			// throttle bulk transfers to a fraction of the host connection's
			// throughput. Respect the guest's advertised receive window; the
			// retransmit queue still handles any lost virtual frames.
			inFlight := c.hostSeq - c.sendAcked
			peerWnd := uint32(c.peerWnd)
			if c.wndScaleOK {
				peerWnd = peerWnd << c.peerWndScale
			}
			wnd := peerWnd
			// Debug: log when blocked on window
			if wnd == 0 || inFlight >= wnd {
				tracef("netstack.tcpConn Write blocked", "wnd=%d inFlight=%d peerWnd=%d key=%v", wnd, inFlight, peerWnd, c.key)
			}
			if wnd > 0 && inFlight < wnd {
				avail := wnd - inFlight
				maxChunk := maxPayload
				if int(avail) < maxChunk {
					maxChunk = int(avail)
				}
				chunk = b[written:]
				if len(chunk) > maxChunk {
					chunk = chunk[:maxChunk]
				}

				// Nagle's algorithm: buffer small writes if data is in flight
				if c.nagleEnabled && len(chunk) < maxPayload && inFlight > 0 {
					// Add to Nagle buffer instead of sending immediately
					c.nagleBuf = append(c.nagleBuf, chunk...)
					written += len(chunk)
					c.mu.Unlock()
					// Skip to next iteration without going through normal send path
					goto nextChunk
				}

				// If we have buffered Nagle data, prepend it
				if len(c.nagleBuf) > 0 {
					chunk = append(c.nagleBuf, chunk...)
					c.nagleBuf = nil
					if len(chunk) > maxChunk {
						// Send up to maxChunk, keep rest in nagleBuf
						c.nagleBuf = append([]byte(nil), chunk[maxChunk:]...)
						chunk = chunk[:maxChunk]
					}
				}

				seq = c.hostSeq
				ack = c.guestSeq
				c.hostSeq += uint32(len(chunk))
				flags = uint16(tcpFlagACK)
				if written+len(chunk) == len(b) && len(c.nagleBuf) == 0 {
					flags |= uint16(tcpFlagPSH)
				}
				break
			}
			if c.sendCond == nil {
				c.mu.Unlock()
				return written, errors.New("send window stalled")
			}
			tracef("netstack.tcpConn Write waiting on sendCond", "key=%v", c.key)
			if err := c.waitForSendWindowLocked(); err != nil {
				c.mu.Unlock()
				return written, err
			}
			tracef("netstack.tcpConn Write woke up", "peerWnd=%d key=%v", c.peerWnd, c.key)
		}
		c.mu.Unlock()

		tracef("netstack.tcpConn Write", "len=%d seq=%d ack=%d", len(chunk), seq, ack)

		if err := c.stack.sendTCPPacket(c.localIPv4, c.key, seq, ack, flags, chunk); err != nil {
			return written, err
		}

		// Add to send buffer for potential retransmission
		if c.sendBuf != nil {
			seg := tcpSendSegment{
				seqStart: seq,
				seqEnd:   seq + uint32(len(chunk)),
				payload:  retainPayload(chunk),
				sentAt:   time.Now(),
			}
			// Wait for space if buffer is full.
			for !c.sendBuf.append(seg) {
				c.mu.Lock()
				ackedThrough := c.sendAcked
				if c.closed {
					c.mu.Unlock()
					return written, net.ErrClosed
				}
				c.mu.Unlock()
				// An ACK can arrive synchronously from the virtio backend before
				// the segment is appended above. Reconcile the queue before
				// waiting so an already-acknowledged segment cannot consume send
				// buffer capacity forever.
				c.sendBuf.ack(ackedThrough)
				if c.sendBuf.append(seg) {
					break
				}
				c.mu.Lock()
				c.sendCond.Wait()
				c.mu.Unlock()
			}

			// sendTCPPacket may synchronously re-enter handleSegment and advance
			// sendAcked before append runs. Remove any such segment now; without
			// this reconciliation, pipelined protocols can eventually fill the
			// retransmit queue with data the guest has already acknowledged.
			c.mu.Lock()
			ackedThrough := c.sendAcked
			c.mu.Unlock()
			c.sendBuf.ack(ackedThrough)

			// Start retransmit timer if not already running
			if c.sendBuf.len() > 0 {
				c.startRetxTimer()
			}
		}

		written += len(chunk)
	nextChunk:
	}
	return len(b), nil
}

func (c *tcpConn) waitForSendWindowLocked() error {
	if c.writeDeadline.IsZero() {
		c.sendCond.Wait()
		return nil
	}
	until := time.Until(c.writeDeadline)
	if until <= 0 {
		return &net.OpError{Op: "write", Net: "tcp", Err: tcpTimeoutError{}}
	}
	timer := time.AfterFunc(until, func() {
		c.mu.Lock()
		c.sendCond.Broadcast()
		c.mu.Unlock()
	})
	c.sendCond.Wait()
	if !timer.Stop() && time.Now().After(c.writeDeadline) {
		return &net.OpError{Op: "write", Net: "tcp", Err: tcpTimeoutError{}}
	}
	return nil
}

// startRetxTimer starts the retransmission timer if not already running.
// This ensures the timer is based on the oldest unacked segment.
func (c *tcpConn) startRetxTimer() {
	c.retxTimerMu.Lock()
	defer c.retxTimerMu.Unlock()

	// Only start if no timer is running - timer is based on oldest segment
	if c.retxTimer != nil {
		return
	}

	rto := c.rttEst.getRTO()
	c.retxTimer = time.AfterFunc(rto, c.onRetxTimeout)
}

// restartRetxTimer forces a restart of the retransmission timer with updated RTO.
// Called when new ACKs arrive to reset the timer for remaining segments.
func (c *tcpConn) restartRetxTimer() {
	c.retxTimerMu.Lock()
	defer c.retxTimerMu.Unlock()

	if c.retxTimer != nil {
		c.retxTimer.Stop()
	}

	rto := c.rttEst.getRTO()
	c.retxTimer = time.AfterFunc(rto, c.onRetxTimeout)
}

// stopRetxTimer stops the retransmission timer.
func (c *tcpConn) stopRetxTimer() {
	c.retxTimerMu.Lock()
	defer c.retxTimerMu.Unlock()

	if c.retxTimer != nil {
		c.retxTimer.Stop()
		c.retxTimer = nil
	}
}

// onRetxTimeout handles retransmission timeout.
func (c *tcpConn) onRetxTimeout() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}

	// Determine MSS for coalescing
	mss := int(c.mss)
	if mss == 0 {
		mss = defaultMSS
	}

	// Get oldest unacked segments, coalesced up to MSS
	seg, coalescedCount, ok := c.sendBuf.oldestCoalesced(mss)
	if !ok {
		// No unacked data - clear timer and return
		c.retxTimerMu.Lock()
		c.retxTimer = nil
		c.retxTimerMu.Unlock()
		c.mu.Unlock()
		return
	}

	// Verify this is a valid timeout - the segment should have been sent
	// at least minRTO ago to avoid spurious timeouts
	age := time.Since(seg.sentAt)
	rto := c.rttEst.getRTO()
	if age < rto/2 {
		// Spurious timeout - segment was recently (re)sent, just reschedule
		c.mu.Unlock()
		c.retxTimerMu.Lock()
		c.retxTimer = time.AfterFunc(rto-age, c.onRetxTimeout)
		c.retxTimerMu.Unlock()
		return
	}

	// Check max retries
	const maxRetries = 10
	if seg.retxCount >= maxRetries {
		c.mu.Unlock()
		c.logStallSnapshot("max retries exceeded")
		c.Close()
		return
	}

	// Log stall info every 3 retries
	if seg.retxCount > 0 && seg.retxCount%3 == 0 {
		c.mu.Unlock()
		c.logStallSnapshot("repeated retransmissions")
		c.mu.Lock()
	}

	// Only notify congestion control after first retransmit
	// This avoids penalizing for transient timing issues
	if seg.retxCount > 0 && c.congCtrl != nil {
		c.congCtrl.onTimeout()
	}

	// Exponential backoff
	c.rttEst.backoff()

	ack := c.guestSeq
	c.retxCount++
	rto = c.rttEst.getRTO() // Get updated RTO after backoff
	c.mu.Unlock()

	// Mark coalesced segments as retransmitted
	c.sendBuf.markRetransmittedN(coalescedCount)

	tracef("netstack.tcpConn retransmitting segment", "seq=%d len=%d retxCount=%d coalesced=%d", seg.seqStart, len(seg.payload), seg.retxCount+1, coalescedCount)

	// Retransmit
	_ = c.stack.sendTCPPacket(c.localIPv4, c.key, seg.seqStart, ack, tcpFlagACK, seg.payload)

	// Schedule next timeout - use direct timer management to avoid races
	c.retxTimerMu.Lock()
	c.retxTimer = time.AfterFunc(rto, c.onRetxTimeout)
	c.retxTimerMu.Unlock()
}

func (c *tcpConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	needFin := c.state == tcpStateEstablished && !c.writeClosed
	tracef("netstack.tcpConn Close", "needFin=%t", needFin)
	c.state = tcpStateClosed
	c.closed = true
	c.completeDial(net.ErrClosed)
	if c.sendCond != nil {
		c.sendCond.Broadcast()
	}
	releasePayload(c.readPendingBuf)
	c.readPending = nil
	c.readPendingBuf = nil
	close(c.recvBuf)
	c.mu.Unlock()

	// Stop timers
	c.stopRetxTimer()

	// Clear buffers
	if c.sendBuf != nil {
		c.sendBuf.clear()
	}
	if c.oooRecvBuf != nil {
		c.oooRecvBuf.clear()
	}

	if needFin {
		c.sendFin()
	}

	c.stack.tcpMu.Lock()
	delete(c.stack.tcpConns, c.key)
	c.stack.tcpMu.Unlock()
	return nil
}

// CloseWrite sends FIN while leaving the read side available for the peer's
// response. This is used by bidirectional proxies to preserve TCP half-close.
func (c *tcpConn) CloseWrite() error {
	c.mu.Lock()
	if c.closed || c.writeClosed {
		c.mu.Unlock()
		return nil
	}
	if c.state != tcpStateEstablished {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.writeClosed = true
	c.mu.Unlock()
	c.sendFin()
	return nil
}

func (c *tcpConn) LocalAddr() net.Addr {
	return &tcpAddr{ip: net.IP(c.localIPv4[:]), port: c.key.dstPort}
}

func (c *tcpConn) RemoteAddr() net.Addr {
	return &tcpAddr{ip: net.IP(c.key.srcIP[:]), port: c.key.srcPort}
}

func (c *tcpConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

func (c *tcpConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	return nil
}

func (c *tcpConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return nil
}

// SetNoDelay controls whether Nagle's algorithm is disabled (TCP_NODELAY).
// When noDelay is true, small writes are sent immediately without coalescing.
func (c *tcpConn) SetNoDelay(noDelay bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nagleEnabled = !noDelay
	return nil
}

// Snapshot returns a debug snapshot of the connection state.
func (c *tcpConn) Snapshot() tcpConnSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	stateStr := "unknown"
	switch c.state {
	case tcpStateSynRcvd:
		stateStr = "SYN_RCVD"
	case tcpStateEstablished:
		stateStr = "ESTABLISHED"
	case tcpStateFinWait:
		stateStr = "FIN_WAIT"
	case tcpStateClosed:
		stateStr = "CLOSED"
	}

	peerWnd := uint32(c.peerWnd)
	if c.wndScaleOK {
		peerWnd = uint32(c.peerWnd) << c.peerWndScale
	}

	snap := tcpConnSnapshot{
		State:        stateStr,
		LocalAddr:    fmt.Sprintf("%s:%d", net.IP(c.localIPv4[:]).String(), c.key.dstPort),
		RemoteAddr:   fmt.Sprintf("%s:%d", net.IP(c.key.srcIP[:]).String(), c.key.srcPort),
		HostSeq:      c.hostSeq,
		GuestSeq:     c.guestSeq,
		SendAcked:    c.sendAcked,
		InFlight:     c.sendBuf.inFlight(),
		PeerWnd:      peerWnd,
		PeerWndScale: c.peerWndScale,
		RTO:          c.rttEst.getRTO().String(),
		SRTT:         c.rttEst.srtt.String(),
		RetxCount:    c.retxCount,
		MSS:          c.mss,
	}

	if c.congCtrl != nil {
		snap.Cwnd = c.congCtrl.getCwnd()
		snap.Ssthresh = c.congCtrl.ssthresh
		snap.DupAcks = c.congCtrl.dupAcks
	}

	if c.oooRecvBuf != nil {
		snap.OOOSegments = c.oooRecvBuf.len()
	}

	return snap
}

// logStallSnapshot logs the connection state when a stall is detected.
func (c *tcpConn) logStallSnapshot(reason string) {
	snap := c.Snapshot()
	c.stack.log.Warn("raw: tcp connection stall detected",
		"reason", reason,
		"state", snap.State,
		"local", snap.LocalAddr,
		"remote", snap.RemoteAddr,
		"inFlight", snap.InFlight,
		"peerWnd", snap.PeerWnd,
		"cwnd", snap.Cwnd,
		"rto", snap.RTO,
		"retxCount", snap.RetxCount,
		"oooSegs", snap.OOOSegments,
	)
}

// sendTCPPacket crafts and transmits a TCP segment to the guest.
// sendTCPPacket sends a TCP segment without options.
func (ns *NetStack) sendTCPPacket(
	localIPv4 [4]byte,
	key tcpFourTuple,
	seq, ack uint32,
	flags uint16,
	payload []byte,
) error {
	return ns.sendTCPPacketWithOptions(localIPv4, key, seq, ack, flags, payload, nil)
}

// sendTCPPacketWithOptions sends a TCP segment with optional TCP options.
func (ns *NetStack) sendTCPPacketWithOptions(
	localIPv4 [4]byte,
	key tcpFourTuple,
	seq, ack uint32,
	flags uint16,
	payload []byte,
	options []byte,
) error {
	if netstackTraceEnabled {
		tracef("netstack.sendTCPPacket", "src=%v:%d dst=%v:%d seq=%d ack=%d flags=0x%02x payloadLen=%d optLen=%d",
			net.IP(localIPv4[:]).String(), key.dstPort,
			net.IP(key.srcIP[:]).String(), key.srcPort,
			seq, ack, flags, len(payload), len(options))
	}
	srcIP := net.IP(localIPv4[:])
	dstIP := net.IP(key.srcIP[:])
	srcPort := key.dstPort
	dstPort := key.srcPort

	// Calculate header length including options (must be multiple of 4)
	optLen := len(options)
	if optLen%4 != 0 {
		// Pad options to 4-byte boundary
		padded := make([]byte, ((optLen+3)/4)*4)
		copy(padded, options)
		options = padded
		optLen = len(options)
	}
	headerLen := tcpHeaderLen + optLen
	tcpLen := headerLen + len(payload)
	frame := getEthernetFrameBuffer(ipv4HeaderLen + tcpLen)
	defer putEthernetFrameBuffer(frame)

	packet := frame[ethernetHeaderLen+ipv4HeaderLen : ethernetHeaderLen+ipv4HeaderLen+tcpLen]
	clear(packet[:headerLen])
	binary.BigEndian.PutUint16(packet[0:2], srcPort)
	binary.BigEndian.PutUint16(packet[2:4], dstPort)
	binary.BigEndian.PutUint32(packet[4:8], seq)
	binary.BigEndian.PutUint32(packet[8:12], ack)
	packet[12] = uint8(headerLen/4) << 4
	packet[13] = uint8(flags)
	binary.BigEndian.PutUint16(packet[14:16], 0xffff) // Window
	if optLen > 0 {
		copy(packet[tcpHeaderLen:], options)
	}
	copy(packet[headerLen:], payload)

	// Compute checksum over pseudo-header + TCP segment.
	binary.BigEndian.PutUint16(packet[16:18], 0)
	check := tcpChecksum(srcIP, dstIP, packet)
	binary.BigEndian.PutUint16(packet[16:18], check)

	buildIPv4HeaderInto(frame[ethernetHeaderLen:ethernetHeaderLen+ipv4HeaderLen], srcIP, dstIP, tcpProtocolNumber, len(packet))

	dstMAC := ns.guestMACForTransmit()
	if !macIsSet(dstMAC) {
		return fmt.Errorf("guest mac unknown for tcp transmit")
	}
	buildEthernetHeaderInto(frame[:ethernetHeaderLen], dstMAC, macAddr(ns.hostMAC.Load()), etherTypeIPv4)

	return ns.sendFrame(frame)
}

// sendRST constructs a reset segment in response to an unexpected inbound.
func (ns *NetStack) sendRST(h ipv4Header, hdr tcpHeader) error {
	key := tcpFourTuple{}
	copy(key.srcIP[:], h.src.To4())
	copy(key.dstIP[:], h.dst.To4())
	key.srcPort = hdr.srcPort
	key.dstPort = hdr.dstPort
	var localIPv4 [4]byte
	if dst := h.dst.To4(); dst != nil {
		copy(localIPv4[:], dst)
	} else {
		copy(localIPv4[:], ns.hostIPv4[:])
	}
	return ns.sendTCPPacket(
		localIPv4,
		key,
		hdr.ack,
		hdr.seq+1,
		tcpFlagRST|tcpFlagACK,
		nil,
	)
}

////////////////////////////////////////////////////////////////////////////////
// Service proxying (TCP only, to localhost).
////////////////////////////////////////////////////////////////////////////////

// shouldProxyService returns true when the destination is the serviceIPv4.
func (ns *NetStack) shouldProxyService(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.To4() != nil && ipEqual(ip.To4(), ns.serviceIPv4[:])
}

func (ns *NetStack) serviceProxyAllowed(ip net.IP, port uint16) bool {
	if !ns.shouldProxyService(ip) {
		return false
	}
	if ns.hostAccessEnabled {
		return true
	}
	ns.serviceProxyPortsMu.RLock()
	defer ns.serviceProxyPortsMu.RUnlock()
	_, ok := ns.serviceProxyPorts[port]
	return ok
}

func (ns *NetStack) serviceARPEnabled() bool {
	ns.serviceProxyPortsMu.RLock()
	defer ns.serviceProxyPortsMu.RUnlock()
	return ns.hostAccessEnabled || len(ns.serviceProxyPorts) > 0
}

// startServiceProxy bridges the TCP conn to 127.0.0.1:dstPort.
func (ns *NetStack) startServiceProxy(conn *tcpConn) {
	go func() {
		addr := &net.TCPAddr{
			IP:   net.IPv4(127, 0, 0, 1),
			Port: int(conn.key.dstPort),
		}
		outbound, err := net.DialTCP("tcp", nil, addr)
		if err != nil {
			slog.Debug(
				"raw: service proxy dial failed",
				"port", conn.key.dstPort,
				"err", err,
			)
			_ = conn.Close()
			return
		}
		tuneProxyTCPConn(outbound)
		defer outbound.Close()
		defer conn.Close()

		if err := proxyConns(outbound, conn, 64*1024); err != nil &&
			!errors.Is(err, io.EOF) &&
			!errors.Is(err, net.ErrClosed) {
			slog.Error("raw: service proxy", "err", err)
		}
	}()
}

// startOutboundTCPProxy bridges the guest-visible TCP conn to a real host TCP
// socket connected to the conn's destination IP:port (transparent proxying).
func (ns *NetStack) startOutboundTCPProxy(conn *tcpConn) {
	go func() {
		addr := &net.TCPAddr{
			IP:   net.IP(conn.key.dstIP[:]),
			Port: int(conn.key.dstPort),
		}

		outbound, err := ns.tcpDial(context.Background(), addr)
		if err != nil {
			slog.Debug(
				"raw: outbound proxy dial failed",
				"dst", addr.String(),
				"err", err,
			)
			_ = conn.Close()
			return
		}
		if tcpConn, ok := outbound.(*net.TCPConn); ok {
			tuneProxyTCPConn(tcpConn)
		}
		defer outbound.Close()
		defer conn.Close()

		if err := proxyConns(outbound, conn, 64*1024); err != nil &&
			!errors.Is(err, io.EOF) &&
			!errors.Is(err, net.ErrClosed) {
			slog.Error("raw: outbound proxy", "err", err)
		}
	}()
}

func tuneProxyTCPConn(conn *net.TCPConn) {
	if conn == nil {
		return
	}
	_ = conn.SetNoDelay(true)
	_ = conn.SetReadBuffer(4 * 1024 * 1024)
	_ = conn.SetWriteBuffer(4 * 1024 * 1024)
}

////////////////////////////////////////////////////////////////////////////////
// User-facing Listen/Dial for UDP/TCP (limited).
////////////////////////////////////////////////////////////////////////////////

// ListenPacketInternal binds a UDP endpoint on a given port.
func (ns *NetStack) ListenPacketInternal(
	network, address string,
) (net.PacketConn, error) {
	if network != "udp" && network != "udp4" {
		return nil, fmt.Errorf("network %q not supported", network)
	}

	addr, err := splitHostPort(address)
	if err != nil {
		return nil, err
	}

	ep := newUDPEndpointConn(ns, addr.Port)
	ns.udpMu.Lock()
	if _, ok := ns.udpSockets[addr.Port]; ok {
		ns.udpMu.Unlock()
		return nil, fmt.Errorf("udp port %d already in use", addr.Port)
	}
	ns.udpSockets[addr.Port] = ep
	ns.udpMu.Unlock()

	return ep, nil
}

// UDPCallback is a function type for handling UDP packets. The data and addr
// are borrowed for the duration of the callback; copy anything retained after
// the callback returns.
type UDPCallback func(ep *udpCallbackEndpoint, data []byte, addr net.UDPAddr)

type udpCallbackEndpoint struct {
	stack    *NetStack
	port     uint16
	callback UDPCallback

	closed atomic.Bool
	buf    []byte
}

func newUDPCallbackEndpoint(
	stack *NetStack,
	port uint16,
	callback UDPCallback,
) *udpCallbackEndpoint {
	return &udpCallbackEndpoint{
		stack:    stack,
		port:     port,
		callback: callback,
		buf:      make([]byte, 0, 1500),
	}
}

func (ep *udpCallbackEndpoint) enqueue(data []byte, addr net.UDPAddr) error {
	if ep.closed.Load() {
		return net.ErrClosed
	}

	ep.callback(ep, data, addr)

	return nil
}

func (ep *udpCallbackEndpoint) WriteTo(b []byte, addr net.UDPAddr) (int, error) {
	if ep.closed.Load() {
		return 0, net.ErrClosed
	}

	srcIP := net.IP(ep.stack.hostIPv4[:])
	dstIP := addr.IP

	if len(ep.buf) < ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+len(b) {
		ep.buf = make([]byte, ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen+len(b))
	}

	copy(ep.buf[ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen:], b)

	err := ep.stack.sendUDP(ep.buf, ep.port, uint16(addr.Port), srcIP, dstIP, len(b))
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (ep *udpCallbackEndpoint) Close() error {
	if ep.closed.Load() {
		return nil
	}
	ep.closed.Store(true)

	ep.stack.udpMu.Lock()
	delete(ep.stack.udpSockets, ep.port)
	ep.stack.udpMu.Unlock()
	return nil
}

var (
	_ udpEndpoint = (*udpCallbackEndpoint)(nil)
)

// BindUDPCallback binds a UDP port to a callback function.
func (ns *NetStack) BindUDPCallback(address string, callback UDPCallback) error {
	addr, err := splitHostPort(address)
	if err != nil {
		return err
	}

	ep := newUDPCallbackEndpoint(ns, addr.Port, callback)
	ns.udpMu.Lock()
	if _, ok := ns.udpSockets[addr.Port]; ok {
		ns.udpMu.Unlock()
		return fmt.Errorf("udp port %d already in use", addr.Port)
	}
	ns.udpSockets[addr.Port] = ep
	ns.udpMu.Unlock()

	return nil
}

// DialInternalContext opens a synthetic TCP connection from the host side of
// the stack into the guest. It is used by host port forwarding.
func (ns *NetStack) DialInternalContext(
	ctx context.Context,
	network, address string,
) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("network %q not supported", network)
	}
	addr, err := splitHostPort(address)
	if err != nil {
		return nil, err
	}
	if addr.Port == 0 {
		return nil, fmt.Errorf("tcp dial missing destination port")
	}

	var guestIP [4]byte
	if addr.Host == "" {
		guestIP = ns.guestIPv4
	} else {
		ip4 := net.ParseIP(addr.Host).To4()
		if ip4 == nil {
			return nil, fmt.Errorf("tcp dial host %q is not an ipv4 address", addr.Host)
		}
		copy(guestIP[:], ip4)
	}

	ns.tcpMu.Lock()
	var key tcpFourTuple
	copy(key.srcIP[:], guestIP[:])
	copy(key.dstIP[:], ns.hostIPv4[:])
	key.srcPort = addr.Port
	key.dstPort = ns.allocateTCPSourcePortLocked(guestIP, addr.Port)
	conn := newTCPConn(ns, nil, key, 0, 0xffff, ns.hostIPv4, nil)
	conn.state = tcpStateSynSent
	conn.guestSeq = 0
	conn.dialDone = make(chan error, 1)
	conn.dialClosed = make(chan struct{})
	ns.tcpConns[key] = conn
	ns.tcpMu.Unlock()

	if err := conn.sendSyn(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go conn.retransmitSynUntilDone(ctx)

	select {
	case err := <-conn.dialDone:
		if err != nil {
			return nil, err
		}
		return conn, nil
	case <-ctx.Done():
		_ = conn.Close()
		return nil, ctx.Err()
	}
}

func (c *tcpConn) retransmitSynUntilDone(ctx context.Context) {
	timer := time.NewTimer(activeOpenSynRTO)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.dialClosed:
			return
		case <-timer.C:
			if err := c.retransmitSyn(); err != nil {
				return
			}
			timer.Reset(activeOpenSynRTO)
		}
	}
}

func (ns *NetStack) allocateTCPSourcePortLocked(dstIP [4]byte, dstPort uint16) uint16 {
	const firstEphemeral = 49152
	const ephemeralCount = 65535 - firstEphemeral + 1
	start := uint16(firstEphemeral + ns.randSource.Intn(ephemeralCount))
	for i := 0; i < ephemeralCount; i++ {
		port := uint16(firstEphemeral + ((int(start) - firstEphemeral + i) % ephemeralCount))
		key := tcpFourTuple{
			srcIP:   dstIP,
			dstIP:   ns.hostIPv4,
			srcPort: dstPort,
			dstPort: port,
		}
		if _, ok := ns.tcpConns[key]; !ok {
			return port
		}
	}
	return start
}

// ListenInternal binds a TCP listener on a given port.
func (ns *NetStack) ListenInternal(
	network, address string,
) (net.Listener, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("network %q not supported", network)
	}

	addr, err := splitHostPort(address)
	if err != nil {
		return nil, err
	}

	ns.tcpMu.Lock()
	defer ns.tcpMu.Unlock()

	if _, ok := ns.tcpListen[addr.Port]; ok {
		return nil, fmt.Errorf("tcp port %d already in use", addr.Port)
	}

	l := newTCPListener(ns, addr.Port)
	ns.tcpListen[addr.Port] = l
	return l, nil
}

////////////////////////////////////////////////////////////////////////////////
// DNS server bridge.
////////////////////////////////////////////////////////////////////////////////

// StartDNSServer binds UDP:53 and serves using a tiny DNS responder.
//
// The server resolves a few internal hostnames, then optionally falls back
// to real DNS if allowInternet is true.
func (ns *NetStack) StartDNSServer() error {
	if ns.dnsServer != nil {
		return nil
	}

	packetConn, err := ns.ListenPacketInternal("udp", ":53")
	if err != nil {
		return fmt.Errorf("listen udp port 53: %w", err)
	}

	dnsSrv := newDNSServer(packetConn, ns.lookupDNSName, ns.resolveDNSQuestion)
	ns.dnsServer = dnsSrv
	return nil
}

func (ns *NetStack) lookupDNSName(name string) (string, error) {
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	switch n {
	case ns.hostDNSName, "host.internal":
		if !ns.hostAccessEnabled {
			return "", fmt.Errorf("host access disabled")
		}
		return net.IP(ns.hostIPv4[:]).String(), nil
	case "guest.internal":
		return net.IP(ns.guestIPv4[:]).String(), nil
	case "service.internal":
		if !ns.hostAccessEnabled {
			return "", fmt.Errorf("host access disabled")
		}
		return net.IP(ns.serviceIPv4[:]).String(), nil
	}
	return "", fmt.Errorf("not a synthetic DNS name")
}

func (ns *NetStack) resolveDNSQuestion(q dnsQuestion) ([]dnsResource, []dnsResource, error) {
	if !ns.allowInternet {
		return nil, nil, fmt.Errorf("internet access disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch q.qtype {
	case dnsTypeA:
		return resolveIPDNSQuestion(ctx, q, "ip4")
	case dnsTypeAAAA:
		return resolveIPDNSQuestion(ctx, q, "ip6")
	case dnsTypeSRV:
		return resolveSRVDNSQuestion(ctx, q)
	default:
		return nil, nil, nil
	}
}

func resolveIPDNSQuestion(ctx context.Context, q dnsQuestion, network string) ([]dnsResource, []dnsResource, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, network, strings.TrimSuffix(q.name, "."))
	if err != nil {
		return nil, nil, err
	}
	answers := make([]dnsResource, 0, len(ips))
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil && network == "ip4" {
			answers = append(answers, dnsResource{
				nameStart: q.nameStart,
				typ:       dnsTypeA,
				class:     dnsClassIN,
				ttl:       30,
				data:      append([]byte(nil), ip4...),
			})
			continue
		}
		if ip16 := ip.To16(); ip16 != nil && ip.To4() == nil && network == "ip6" {
			answers = append(answers, dnsResource{
				nameStart: q.nameStart,
				typ:       dnsTypeAAAA,
				class:     dnsClassIN,
				ttl:       30,
				data:      append([]byte(nil), ip16...),
			})
		}
	}
	return answers, nil, nil
}

func resolveSRVDNSQuestion(ctx context.Context, q dnsQuestion) ([]dnsResource, []dnsResource, error) {
	service, proto, name := splitSRVQueryName(q.name)
	_, records, err := net.DefaultResolver.LookupSRV(ctx, service, proto, strings.TrimSuffix(name, "."))
	if err != nil {
		return nil, nil, err
	}
	answers := make([]dnsResource, 0, len(records))
	additionals := make([]dnsResource, 0)
	addedAdditional := map[string]struct{}{}
	for _, record := range records {
		targetName := strings.TrimSuffix(strings.ToLower(record.Target), ".")
		target, err := encodeDNSName(targetName)
		if err != nil {
			continue
		}
		data := make([]byte, 6+len(target))
		binary.BigEndian.PutUint16(data[0:2], record.Priority)
		binary.BigEndian.PutUint16(data[2:4], record.Weight)
		binary.BigEndian.PutUint16(data[4:6], record.Port)
		copy(data[6:], target)
		answers = append(answers, dnsResource{
			nameStart: q.nameStart,
			typ:       dnsTypeSRV,
			class:     dnsClassIN,
			ttl:       30,
			data:      data,
		})
		if _, ok := addedAdditional[targetName]; ok {
			continue
		}
		addedAdditional[targetName] = struct{}{}
		additionals = append(additionals, resolveAdditionalAddressRecords(ctx, targetName)...)
	}
	return answers, additionals, nil
}

func splitSRVQueryName(name string) (string, string, string) {
	labels := strings.Split(strings.Trim(name, "."), ".")
	if len(labels) < 3 || !strings.HasPrefix(labels[0], "_") || !strings.HasPrefix(labels[1], "_") {
		return "", "", name
	}
	return strings.TrimPrefix(labels[0], "_"), strings.TrimPrefix(labels[1], "_"), strings.Join(labels[2:], ".")
}

func resolveAdditionalAddressRecords(ctx context.Context, name string) []dnsResource {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", strings.TrimSuffix(name, "."))
	if err != nil {
		return nil
	}
	resources := make([]dnsResource, 0, len(ips))
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			resources = append(resources, dnsResource{
				name:      name,
				nameStart: -1,
				typ:       dnsTypeA,
				class:     dnsClassIN,
				ttl:       30,
				data:      append([]byte(nil), ip4...),
			})
			continue
		}
		if ip16 := ip.To16(); ip16 != nil {
			resources = append(resources, dnsResource{
				name:      name,
				nameStart: -1,
				typ:       dnsTypeAAAA,
				class:     dnsClassIN,
				ttl:       30,
				data:      append([]byte(nil), ip16...),
			})
		}
	}
	return resources
}

////////////////////////////////////////////////////////////////////////////////
// Debug HTTP endpoint providing JSON status.
////////////////////////////////////////////////////////////////////////////////

// EnableDebugHTTP starts a small debug server exposing internal state at /status.
//
// BUG: The code uses sync.WaitGroup but calls debugWG.Go(...). WaitGroup
// does not have a Go method; this will not compile unless debugWG is some
// wrapper type elsewhere. Either change to Add/Done or use errgroup.Group.
func (ns *NetStack) EnableDebugHTTP(addr string) error {
	if addr == "" {
		return nil
	}

	ns.debugMu.Lock()
	defer ns.debugMu.Unlock()

	if ns.debugSrv != nil {
		return fmt.Errorf("debug http already enabled at %s", ns.debugAddr)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen debug http: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", ns.handleDebugStatus)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ns.debugSrv = srv
	ns.debugListener = ln
	ns.debugAddr = ln.Addr().String()

	ns.debugWG.Add(1)
	go func() {
		defer ns.debugWG.Done()
		if err := srv.Serve(ln); err != nil &&
			!errors.Is(err, http.ErrServerClosed) &&
			!errors.Is(err, net.ErrClosed) {
			ns.log.Warn("raw: debug http serve", "err", err)
		}
	}()

	tracef("netstack.EnableDebugHTTP", "addr=%s", addr)
	return nil
}

// DebugHTTPAddr returns the bound address of the debug HTTP server.
func (ns *NetStack) DebugHTTPAddr() string {
	ns.debugMu.Lock()
	defer ns.debugMu.Unlock()
	return ns.debugAddr
}

// handleDebugStatus writes a JSON dump of internal state.
func (ns *NetStack) handleDebugStatus(w http.ResponseWriter, r *http.Request) {
	status := ns.collectDebugStatus()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		ns.log.Warn("raw: debug status encode", "err", err)
	}
}

// debugStatus is the JSON structure exposed at /status.
type debugStatus struct {
	HostIPv4                 string   `json:"hostIPv4"`
	GuestIPv4                string   `json:"guestIPv4"`
	ServiceIPv4              string   `json:"serviceIPv4"`
	AllowInternet            bool     `json:"allowInternet"`
	HostAccess               bool     `json:"hostAccess"`
	ServiceProxy             bool     `json:"serviceProxy"`
	AllowedServiceProxyPorts []uint16 `json:"allowedServiceProxyPorts,omitempty"`
	Interfaces               int      `json:"interfaces"`
	TCPListeners             []uint16 `json:"tcpListeners"`
	TCPConnections           []string `json:"tcpConnections"`
	UDPSockets               []uint16 `json:"udpSockets"`
	DebugAddr                string   `json:"debugAddr"`
	UDPRxPackets             uint64   `json:"udpRxPackets"`
	UDPTxPackets             uint64   `json:"udpTxPackets"`
	HostMAC                  string   `json:"hostMAC"`
	ConfiguredMAC            string   `json:"configuredGuestMAC"`
	ObservedMAC              string   `json:"observedGuestMAC"`
	SourceMACViolations      uint64   `json:"sourceMACViolations"`
	SourceARPViolations      uint64   `json:"sourceARPViolations"`
	SourceIPv4Violations     uint64   `json:"sourceIPv4Violations"`
	MalformedSourceFrames    uint64   `json:"malformedSourceFrames"`
}

func (ns *NetStack) collectDebugStatus() debugStatus {
	status := debugStatus{
		HostIPv4:      net.IP(ns.hostIPv4[:]).String(),
		GuestIPv4:     net.IP(ns.guestIPv4[:]).String(),
		ServiceIPv4:   net.IP(ns.serviceIPv4[:]).String(),
		AllowInternet: ns.allowInternet,
		HostAccess:    ns.hostAccessEnabled,
		ServiceProxy:  ns.hostAccessEnabled && ns.serviceProxyEnabled,
		DebugAddr:     ns.DebugHTTPAddr(),
	}
	ns.serviceProxyPortsMu.RLock()
	if len(ns.serviceProxyPorts) > 0 {
		status.AllowedServiceProxyPorts = make([]uint16, 0, len(ns.serviceProxyPorts))
		for port := range ns.serviceProxyPorts {
			status.AllowedServiceProxyPorts = append(status.AllowedServiceProxyPorts, port)
		}
		sort.Slice(status.AllowedServiceProxyPorts, func(i, j int) bool {
			return status.AllowedServiceProxyPorts[i] < status.AllowedServiceProxyPorts[j]
		})
	}
	ns.serviceProxyPortsMu.RUnlock()

	ns.mu.RLock()
	if ns.iface != nil {
		status.Interfaces = 1
	}
	ns.mu.RUnlock()

	if mac := macFromUint64(macAddr(ns.hostMAC.Load())); len(mac) == 6 {
		status.HostMAC = mac.String()
	}
	if mac := macFromUint64(macAddr(ns.guestMAC.Load())); len(mac) == 6 {
		status.ConfiguredMAC = mac.String()
	}
	if mac := macFromUint64(macAddr(ns.observedGuestMAC.Load())); len(mac) == 6 {
		status.ObservedMAC = mac.String()
	}
	status.SourceMACViolations = ns.sourceViolations[SourceMACViolation].Load()
	status.SourceARPViolations = ns.sourceViolations[SourceARPViolation].Load()
	status.SourceIPv4Violations = ns.sourceViolations[SourceIPv4Violation].Load()
	status.MalformedSourceFrames = ns.sourceViolations[SourceMalformed].Load()

	ns.tcpMu.Lock()
	for port := range ns.tcpListen {
		status.TCPListeners = append(status.TCPListeners, port)
	}
	for key := range ns.tcpConns {
		connStr := fmt.Sprintf(
			"%s:%d -> %s:%d",
			net.IP(key.srcIP[:]).String(),
			key.srcPort,
			net.IP(key.dstIP[:]).String(),
			key.dstPort,
		)
		status.TCPConnections = append(status.TCPConnections, connStr)
	}
	ns.tcpMu.Unlock()

	ns.udpMu.RLock()
	for port := range ns.udpSockets {
		status.UDPSockets = append(status.UDPSockets, port)
	}
	ns.udpMu.RUnlock()

	sort.Slice(status.TCPListeners, func(i, j int) bool { return status.TCPListeners[i] < status.TCPListeners[j] })
	sort.Strings(status.TCPConnections)
	sort.Slice(status.UDPSockets, func(i, j int) bool { return status.UDPSockets[i] < status.UDPSockets[j] })

	status.UDPRxPackets = ns.udpRxPackets.Load()
	status.UDPTxPackets = ns.udpTxPackets.Load()

	return status
}

////////////////////////////////////////////////////////////////////////////////
// Helpers: DNS server stop, parsing, checksums, etc.
////////////////////////////////////////////////////////////////////////////////

// hostPort is a helper for parsing "host:port" strings.
type hostPort struct {
	Host string
	Port uint16
}

func splitHostPort(address string) (hostPort, error) {
	if address == "" {
		return hostPort{Host: "", Port: 0}, nil
	}
	if !strings.Contains(address, ":") {
		address = ":" + address
	}

	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return hostPort{}, err
	}
	port64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return hostPort{}, fmt.Errorf("parse port %q: %w", portStr, err)
	}
	return hostPort{Host: host, Port: uint16(port64)}, nil
}

func ipEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isHostLocalIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 0:
		return true
	case ip4[0] == 10:
		return true
	case ip4[0] == 100 && ip4[1]&0xc0 == 0x40:
		return true
	case ip4[0] == 127:
		return true
	case ip4[0] == 169 && ip4[1] == 254:
		return true
	case ip4[0] == 172 && ip4[1]&0xf0 == 16:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	case ip4[0] >= 224:
		return true
	default:
		return false
	}
}

func checksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for (sum >> 16) != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// pseudoHeaderChecksum computes the IPv4 pseudo-header checksum, which is then
// combined with the transport segment checksum.
//
// BUG: No validation is done if src/dst are not IPv4 addresses; callers must
// pass 4-byte addresses.
func pseudoHeaderChecksum(
	src, dst net.IP,
	protocol protocolNumber,
	length int,
) uint32 {
	sum := uint32(0)
	ip4 := src.To4()
	dst4 := dst.To4()
	sum += uint32(binary.BigEndian.Uint16(ip4[0:2]))
	sum += uint32(binary.BigEndian.Uint16(ip4[2:4]))
	sum += uint32(binary.BigEndian.Uint16(dst4[0:2]))
	sum += uint32(binary.BigEndian.Uint16(dst4[2:4]))
	sum += uint32(protocol)
	sum += uint32(length)
	return sum
}

func checksumWithInitial(data []byte, initial uint32) uint16 {
	sum := initial
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for (sum >> 16) != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func tcpChecksum(src, dst net.IP, payload []byte) uint16 {
	ps := pseudoHeaderChecksum(src, dst, tcpProtocolNumber, len(payload))
	return checksumWithInitial(payload, ps)
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

var netstackTraceEnabled = os.Getenv("CC_NETSTACK_TRACE") != ""

func tracef(event string, format string, args ...any) {
	if !netstackTraceEnabled {
		return
	}
	fmt.Fprintf(os.Stderr, event+": "+format+"\n", args...)
}

// proxyConns bridges bytes between a and b, preserving TCP half-close in both
// directions. In particular, an origin server closing its response stream must
// deliver FIN to the guest without discarding a request that is still in flight.
func proxyConns(a, b net.Conn, bufSize int) error {
	copyOne := func(dst, src net.Conn) error {
		buf := getProxyCopyBuffer(bufSize)
		defer releaseProxyCopyBuffer(buf)
		_, err := io.CopyBuffer(dst, src, buf)
		if err == nil {
			if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
				err = closeWriter.CloseWrite()
			} else {
				// Some net.Conn implementations do not support half-close. Close
				// them to ensure the opposite copy goroutine can terminate.
				err = dst.Close()
			}
		}
		return err
	}

	errCh := make(chan error, 2)
	go func() { errCh <- copyOne(a, b) }()
	go func() { errCh <- copyOne(b, a) }()

	// A clean EOF only closes that direction. Wait for the peer to finish the
	// other half so response bytes are not truncated. An actual copy error still
	// aborts both sides promptly.
	err := <-errCh
	if err != nil {
		_ = a.Close()
		_ = b.Close()
	}
	err2 := <-errCh
	_ = a.Close()
	_ = b.Close()
	if err == nil {
		err = err2
	}
	return err
}
