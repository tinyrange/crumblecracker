package virtio

import (
	"io"
	"testing"
	"time"
)

func TestSimpleVsockPeerCloseUnblocksRead(t *testing.T) {
	backend := NewSimpleVsockBackend()
	listener, err := backend.Listen(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	clientConn, err := backend.Connect(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := clientConn.Read(make([]byte, 1))
		readDone <- err
	}()
	if err := serverConn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != io.EOF {
			t.Fatalf("read after peer close = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("peer close did not unblock read")
	}
}

func TestVsockBackendDeliveryWaitsForPeerCredit(t *testing.T) {
	backend := NewSimpleVsockBackend()
	listener, err := backend.Listen(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	clientConn, err := backend.Connect(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer serverConn.Close()

	device := NewVsock(0, 0, 0, 3, backend)
	defer device.Close()
	key := vsockConnKey{localPort: 1024, remotePort: 2048}
	device.connections[key] = &vsockConnection{
		key: key, state: vsockConnStateConnected, peerAlloc: 4, backend: serverConn,
	}
	device.wg.Add(1)
	go device.readFromBackend(serverConn, key)
	if _, err := clientConn.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	waitForVsockTxCount(t, device, key, 4)
	time.Sleep(20 * time.Millisecond)
	device.mu.Lock()
	if got := device.connections[key].txCnt; got != 4 {
		device.mu.Unlock()
		t.Fatalf("sent %d bytes through a 4-byte peer window", got)
	}
	device.connections[key].peerCnt = 4
	device.connections[key].creditRequestPending = false
	device.creditCond.Broadcast()
	device.mu.Unlock()
	waitForVsockTxCount(t, device, key, 8)
}

func TestVsockDetachesBeforeGuestMemoryIsReleased(t *testing.T) {
	memory := &lifecycleTestNetMemory{testGuestMemory: make(testGuestMemory, 4096)}
	device := NewVsock(0, 0x1000, 11, 3, nil)
	device.Attach(memory, &testIRQ{})
	if memory.detach == nil {
		t.Fatal("vsock did not register a guest-memory detach callback")
	}

	device.pendingRx = [][]byte{{1, 2, 3}}
	memory.detach()
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.mem != nil || device.irq != nil || len(device.pendingRx) != 0 {
		t.Fatal("vsock retained guest-memory state after detach")
	}
	if err := device.processRXLocked(); err != nil {
		t.Fatalf("detached RX processing returned an error: %v", err)
	}
}

func TestVsockResetClosesDiscardedConnections(t *testing.T) {
	backend := NewSimpleVsockBackend()
	listener, err := backend.Listen(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	clientConn, err := backend.Connect(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}

	device := NewVsock(0, 0, 0, 3, backend)
	key := vsockConnKey{localPort: 1024, remotePort: 2048}
	device.connections[key] = &vsockConnection{
		key: key, state: vsockConnStateConnected, backend: serverConn,
	}
	device.wg.Add(1)
	readDone := make(chan struct{})
	go func() {
		device.readFromBackend(serverConn, key)
		close(readDone)
	}()

	device.mu.Lock()
	device.resetLocked()
	device.mu.Unlock()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("vsock reset left a discarded connection reader running")
	}
}

func waitForVsockTxCount(t *testing.T, device *Vsock, key vsockConnKey, want uint32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		device.mu.Lock()
		got := device.connections[key].txCnt
		device.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("vsock tx count did not reach %d", want)
}
