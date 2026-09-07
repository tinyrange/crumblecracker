package vm

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

type queuedListener struct{ conns []net.Conn }

func (l *queuedListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, io.EOF
	}
	conn := l.conns[0]
	l.conns = l.conns[1:]
	return conn, nil
}
func (*queuedListener) Close() error   { return nil }
func (*queuedListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestPortForwardFloodIsRejectedBeforeConnectionWorkers(t *testing.T) {
	listener := &queuedListener{}
	var peers []net.Conn
	for i := 0; i < 3; i++ {
		server, peer := net.Pipe()
		listener.conns = append(listener.conns, server)
		peers = append(peers, peer)
		defer peer.Close()
	}
	runtime := &networkRuntime{forwardSlots: make(chan struct{}, 1)}
	runtime.forwardSlots <- struct{}{}
	runtime.wg.Add(1)
	runtime.acceptPortForward(listener, "10.42.0.2:80", make(chan struct{}, 2))
	stats := runtime.PortForwardStats()
	if stats.Active != 0 || stats.Rejected != 3 || stats.Limit != 1 {
		t.Fatalf("forward stats = %+v", stats)
	}
	for i, peer := range peers {
		buf := make([]byte, 1)
		if _, err := peer.Read(buf); err == nil {
			t.Fatalf("rejected connection %d remained open", i)
		}
	}
}

func TestPeerFramesRequireCompletedTXChecksums(t *testing.T) {
	withSwitch := &netstackVirtioBackend{runtime: &networkRuntime{txHook: func([]byte) bool { return true }}}
	peerFrame := make([]byte, 14)
	copy(peerFrame[:6], []byte{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x03})
	if !withSwitch.NeedsTXChecksum(peerFrame) {
		t.Fatal("peer-bound frame was allowed onto the virtual switch with an unfinished checksum")
	}
	gatewayFrame := make([]byte, 14)
	copy(gatewayFrame[:6], defaultGatewayMACBytes)
	if withSwitch.NeedsTXChecksum(gatewayFrame) {
		t.Fatal("netstack-bound frame unnecessarily requested checksum completion")
	}
}

func TestPeerFramesConsumedBeforeNetstackDelivery(t *testing.T) {
	backend := &netstackVirtioBackend{runtime: &networkRuntime{
		txHook: func([]byte) bool { return true },
	}}
	if err := backend.HandleTxPacket(make([]byte, 14)); err != nil {
		t.Fatalf("consumed peer frame reached detached netstack: %v", err)
	}
}
func TestNetworkRuntimeCloseTerminatesIdleActiveForward(t *testing.T) {
	runtimeConn, peerConn := net.Pipe()
	defer peerConn.Close()
	runtime := &networkRuntime{}
	runtime.ctx, runtime.cancel = context.WithCancel(context.Background())
	if !runtime.registerPortForwardConn(runtimeConn) {
		t.Fatal("active connection was not registered")
	}
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		defer runtime.wg.Done()
		defer runtime.unregisterPortForwardConn(runtimeConn)
		_, _ = io.Copy(io.Discard, runtimeConn)
	}()

	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("network shutdown did not terminate idle active forward")
	}
	select {
	case <-handlerDone:
	default:
		t.Fatal("forward copy goroutine survived network shutdown")
	}
	if _, err := peerConn.Write([]byte("still open")); err == nil {
		t.Fatal("forward peer remained writable after network shutdown")
	}
}

func TestProxyPortForwardPreservesHalfCloseResponse(t *testing.T) {
	hostProxy, hostClient := tcpConnectionPair(t)
	guestProxy, guestServer := tcpConnectionPair(t)
	defer hostClient.Close()
	defer guestServer.Close()

	proxyDone := make(chan struct{})
	go func() {
		proxyPortForward(hostProxy, guestProxy)
		_ = hostProxy.Close()
		_ = guestProxy.Close()
		close(proxyDone)
	}()

	request := []byte("request body")
	if _, err := hostClient.Write(request); err != nil {
		t.Fatal(err)
	}
	if err := hostClient.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	gotRequest, err := io.ReadAll(guestServer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRequest, request) {
		t.Fatalf("forwarded request = %q, want %q", gotRequest, request)
	}

	response := []byte("response after request EOF")
	if _, err := guestServer.Write(response); err != nil {
		t.Fatal(err)
	}
	if err := guestServer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	gotResponse, err := io.ReadAll(hostClient)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("forwarded response = %q, want %q", gotResponse, response)
	}
	select {
	case <-proxyDone:
	case <-time.After(time.Second):
		t.Fatal("forward did not finish after both peers closed their write sides")
	}
}

func tcpConnectionPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server := <-accepted:
		return server, client
	case err := <-acceptErr:
		_ = client.Close()
		t.Fatal(err)
	case <-time.After(time.Second):
		_ = client.Close()
		t.Fatal("timed out accepting TCP test connection")
	}
	return nil, nil
}
