package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/netstack"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const defaultGatewayMAC = "02:42:0a:2a:00:01"
const defaultForwardConnections = 32
const defaultTotalForwardConnections = 128

var defaultGatewayMACBytes = []byte{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x01}

type PortForwardRuntimeStats struct {
	Active   int64
	Rejected uint64
	Limit    int
}

type networkRuntime struct {
	id              string
	ip              net.IP
	mac             net.HardwareAddr
	stack           *netstack.NetStack
	iface           *netstack.NetworkInterface
	dev             *virtio.Net
	txHook          func([]byte) bool
	mu              sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	closing         bool
	listeners       []net.Listener
	active          map[net.Conn]struct{}
	forwards        map[string]client.PortForward
	wg              sync.WaitGroup
	closeOnce       sync.Once
	closeErr        error
	forwardSlots    chan struct{}
	forwardActive   atomic.Int64
	forwardRejected atomic.Uint64
}

type netstackVirtioBackend struct {
	runtime *networkRuntime
}

type networkDeviceConfig struct {
	ID      string
	Config  *client.NetworkConfig
	IP      net.IP
	MAC     net.HardwareAddr
	Base    uint64
	Size    uint64
	IRQ     uint32
	TXHook  func([]byte) bool
	RXHook  func([]byte) error
	Cleanup func()
}

func newNetworkRuntime(cfg networkDeviceConfig) (_ *networkRuntime, retErr error) {
	if cfg.Config == nil || !cfg.Config.Enabled {
		return nil, nil
	}
	totalForwardLimit := cfg.Config.MaxForwardConnections
	if totalForwardLimit <= 0 {
		totalForwardLimit = defaultTotalForwardConnections
	}
	if cfg.IP == nil {
		if ip := net.ParseIP(strings.TrimSpace(cfg.Config.GuestIPv4)).To4(); ip != nil {
			cfg.IP = ip
		}
	}
	if cfg.IP == nil {
		cfg.IP = net.IPv4(10, 42, 0, 2)
	}
	if cfg.MAC == nil {
		if macText := strings.TrimSpace(cfg.Config.GuestMAC); macText != "" {
			mac, err := net.ParseMAC(macText)
			if err != nil {
				return nil, fmt.Errorf("parse guest network MAC %q: %w", macText, err)
			}
			cfg.MAC = mac
		}
	}
	if cfg.MAC == nil {
		cfg.MAC = net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}
	}
	cleanup := func() {
		if cfg.Cleanup != nil {
			cfg.Cleanup()
		}
	}
	defer func() {
		if retErr != nil {
			cleanup()
		}
	}()

	stack := netstack.New(slog.Default())
	stack.SetInternetAccessEnabled(cfg.Config.AllowInternet)
	stack.SetHostAccessEnabled(!cfg.Config.BlockHostAccess)
	stack.SetAllowedServiceProxyPorts(cfg.Config.AllowedServiceProxyPorts)
	stack.SetHostDNSName(cfg.Config.HostDNSName)
	if err := stack.SetGuestMAC(cfg.MAC); err != nil {
		_ = stack.Close()
		return nil, err
	}
	if err := stack.SetGuestIPv4(cfg.IP); err != nil {
		_ = stack.Close()
		return nil, err
	}
	gatewayMAC, err := net.ParseMAC(defaultGatewayMAC)
	if err != nil {
		_ = stack.Close()
		return nil, err
	}
	if err := stack.SetHostMAC(gatewayMAC); err != nil {
		_ = stack.Close()
		return nil, err
	}
	iface, err := stack.AttachNetworkInterface()
	if err != nil {
		_ = stack.Close()
		return nil, err
	}
	if err := stack.StartDNSServer(); err != nil {
		_ = stack.Close()
		return nil, fmt.Errorf("start guest dns server: %w", err)
	}

	runtime := &networkRuntime{
		id:           cfg.ID,
		ip:           cfg.IP,
		mac:          cfg.MAC,
		stack:        stack,
		iface:        iface,
		txHook:       cfg.TXHook,
		forwardSlots: make(chan struct{}, totalForwardLimit),
	}
	runtime.ctx, runtime.cancel = context.WithCancel(context.Background())
	dev := virtio.NewNet(cfg.Base, cfg.Size, cfg.IRQ, cfg.MAC, &netstackVirtioBackend{runtime: runtime})
	dev.CompleteTXChecksum = false
	runtime.dev = dev
	rxHook := cfg.RXHook
	if rxHook == nil {
		rxHook = func(frame []byte) error {
			return dev.EnqueueRxPacket(frame)
		}
	}
	iface.AttachVirtioBackend(func(frame []byte) error {
		return rxHook(frame)
	})

	for _, forward := range cfg.Config.PortForwards {
		if err := runtime.AddPortForward(forward); err != nil {
			_ = runtime.Close()
			return nil, err
		}
	}
	return runtime, nil
}

func (b *netstackVirtioBackend) HandleTxPacket(packet []byte) error {
	if b == nil || b.runtime == nil {
		return fmt.Errorf("network runtime is not attached")
	}
	if b.runtime.txHook != nil {
		if b.runtime.txHook(packet) {
			return nil
		}
	}
	if b.runtime.stack == nil {
		return fmt.Errorf("network runtime is not attached")
	}
	return b.runtime.ifaceDeliver(packet)
}

func (b *netstackVirtioBackend) NeedsTXChecksum(packet []byte) bool {
	return b != nil && b.runtime != nil && b.runtime.txHook != nil && !frameTargetsDefaultGateway(packet)
}

func frameTargetsDefaultGateway(frame []byte) bool {
	return len(frame) >= 6 && bytes.Equal(frame[:6], defaultGatewayMACBytes)
}

func (n *networkRuntime) ifaceDeliver(packet []byte) error {
	if n == nil || n.iface == nil {
		return fmt.Errorf("network interface detached")
	}
	return n.iface.DeliverGuestPacket(packet, true)
}

func (n *networkRuntime) Close() error {
	if n == nil {
		return nil
	}
	n.closeOnce.Do(func() {
		n.closeErr = n.close()
	})
	return n.closeErr
}

func (n *networkRuntime) close() error {
	var err error
	n.mu.Lock()
	n.closing = true
	listeners := append([]net.Listener(nil), n.listeners...)
	n.listeners = nil
	connections := make([]net.Conn, 0, len(n.active))
	for conn := range n.active {
		connections = append(connections, conn)
	}
	cancel := n.cancel
	n.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, ln := range listeners {
		if closeErr := ln.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	for _, conn := range connections {
		if closeErr := conn.Close(); closeErr != nil && err == nil && !errors.Is(closeErr, net.ErrClosed) {
			err = closeErr
		}
	}
	n.wg.Wait()
	if n.stack != nil {
		if stackErr := n.stack.Close(); stackErr != nil && err == nil {
			err = stackErr
		}
	}
	return err
}

func (n *networkRuntime) Device() *virtio.Net {
	if n == nil {
		return nil
	}
	return n.dev
}

func (n *networkRuntime) GuestAddress() string {
	if n == nil || n.ip == nil {
		return "10.42.0.2"
	}
	return n.ip.String()
}

func (n *networkRuntime) GuestCIDR() string {
	return n.GuestAddress() + "/24"
}

func (n *networkRuntime) GuestInitConfig() *vmruntime.GuestNetworkConfig {
	if n == nil {
		return nil
	}
	return &vmruntime.GuestNetworkConfig{
		Interface: "eth0",
		Address:   n.GuestCIDR(),
		Gateway:   "10.42.0.1",
		DNS:       "10.42.0.1",
	}
}

func (n *networkRuntime) AddPortForward(forward client.PortForward) error {
	if n == nil || n.stack == nil {
		return fmt.Errorf("network is not enabled")
	}
	protocol := strings.ToLower(strings.TrimSpace(forward.Protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "tcp4" {
		return fmt.Errorf("port forward protocol %q is not supported", forward.Protocol)
	}
	if forward.HostPort <= 0 || forward.HostPort > 65535 {
		return fmt.Errorf("host port %d out of range", forward.HostPort)
	}
	if forward.GuestPort <= 0 || forward.GuestPort > 65535 {
		return fmt.Errorf("guest port %d out of range", forward.GuestPort)
	}
	hostAddr := strings.TrimSpace(forward.HostAddr)
	if hostAddr == "" {
		hostAddr = "127.0.0.1"
	}
	guestAddr := strings.TrimSpace(forward.GuestAddr)
	if guestAddr == "" {
		guestAddr = n.GuestAddress()
	}
	forward.Protocol = protocol
	forward.HostAddr = hostAddr
	forward.GuestAddr = guestAddr
	key := strings.Join([]string{protocol, hostAddr, strconv.Itoa(forward.HostPort), guestAddr, strconv.Itoa(forward.GuestPort)}, "\x00")

	n.mu.Lock()
	if n.closing {
		n.mu.Unlock()
		return net.ErrClosed
	}
	if existing, ok := n.forwards[key]; ok {
		n.mu.Unlock()
		if existing == forward {
			return nil
		}
		return fmt.Errorf("port forward already exists")
	}
	n.mu.Unlock()

	ln, err := net.Listen("tcp", net.JoinHostPort(hostAddr, strconv.Itoa(forward.HostPort)))
	if err != nil {
		return fmt.Errorf("listen port forward %s:%d: %w", hostAddr, forward.HostPort, err)
	}
	n.mu.Lock()
	if n.closing {
		n.mu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	if n.forwards == nil {
		n.forwards = make(map[string]client.PortForward)
	}
	n.forwards[key] = forward
	n.listeners = append(n.listeners, ln)
	n.wg.Add(1)
	n.mu.Unlock()
	limit := forward.MaxConnections
	if limit <= 0 {
		limit = defaultForwardConnections
	}
	go n.acceptPortForward(ln, net.JoinHostPort(guestAddr, strconv.Itoa(forward.GuestPort)), make(chan struct{}, limit))
	return nil
}

func (n *networkRuntime) AllowServiceProxyPort(port int) error {
	if n == nil || n.stack == nil {
		return fmt.Errorf("network is not enabled")
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("service proxy port %d out of range", port)
	}
	n.stack.AllowServiceProxyPort(port)
	return nil
}

func (n *networkRuntime) acceptPortForward(ln net.Listener, guestAddress string, slots chan struct{}) {
	defer n.wg.Done()
	for {
		hostConn, err := ln.Accept()
		if err != nil {
			return
		}
		if !tryAcquireForwardSlot(slots) {
			n.forwardRejected.Add(1)
			_ = hostConn.Close()
			continue
		}
		if !tryAcquireForwardSlot(n.forwardSlots) {
			releaseForwardSlot(slots)
			n.forwardRejected.Add(1)
			_ = hostConn.Close()
			continue
		}
		if !n.registerPortForwardConn(hostConn) {
			releaseForwardSlot(slots)
			releaseForwardSlot(n.forwardSlots)
			_ = hostConn.Close()
			return
		}
		n.forwardActive.Add(1)
		go func() {
			defer releaseForwardSlot(slots)
			defer releaseForwardSlot(n.forwardSlots)
			defer n.forwardActive.Add(-1)
			n.handlePortForwardConn(hostConn, guestAddress)
		}()
	}
}

func tryAcquireForwardSlot(slots chan struct{}) bool {
	if slots == nil {
		return false
	}
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}
func releaseForwardSlot(slots chan struct{}) {
	if slots == nil {
		return
	}
	select {
	case <-slots:
	default:
	}
}

func (n *networkRuntime) PortForwardStats() PortForwardRuntimeStats {
	if n == nil {
		return PortForwardRuntimeStats{}
	}
	return PortForwardRuntimeStats{Active: n.forwardActive.Load(), Rejected: n.forwardRejected.Load(), Limit: cap(n.forwardSlots)}
}

func (n *networkRuntime) handlePortForwardConn(hostConn net.Conn, guestAddress string) {
	defer n.wg.Done()
	defer n.unregisterPortForwardConn(hostConn)
	defer hostConn.Close()

	baseCtx := n.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
	defer cancel()
	guestConn, err := n.stack.DialInternalContext(ctx, "tcp", guestAddress)
	if err != nil {
		return
	}
	if !n.registerActiveConn(guestConn) {
		_ = guestConn.Close()
		return
	}
	defer n.unregisterPortForwardConn(guestConn)
	defer guestConn.Close()

	proxyPortForward(hostConn, guestConn)
}

func (n *networkRuntime) registerPortForwardConn(conn net.Conn) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closing {
		return false
	}
	if n.active == nil {
		n.active = make(map[net.Conn]struct{})
	}
	n.active[conn] = struct{}{}
	n.wg.Add(1)
	return true
}

func (n *networkRuntime) registerActiveConn(conn net.Conn) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closing {
		return false
	}
	if n.active == nil {
		n.active = make(map[net.Conn]struct{})
	}
	n.active[conn] = struct{}{}
	return true
}

func (n *networkRuntime) unregisterPortForwardConn(conn net.Conn) {
	n.mu.Lock()
	delete(n.active, conn)
	n.mu.Unlock()
}

func proxyPortForward(hostConn, guestConn net.Conn) {
	errCh := make(chan error, 2)
	copyDirection := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err == nil {
			if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
				err = closeWriter.CloseWrite()
			} else {
				err = dst.Close()
			}
		}
		errCh <- err
	}
	go copyDirection(guestConn, hostConn)
	go copyDirection(hostConn, guestConn)
	if err := <-errCh; err != nil {
		_ = hostConn.Close()
		_ = guestConn.Close()
	}
	<-errCh
}
