package rfb

import (
	"context"
	"crypto/des"
	"encoding/binary"
	"image"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestPasswordSecurityAuthenticatesVNCResponse(t *testing.T) {
	server, viewer := net.Pipe()
	defer viewer.Close()
	done := make(chan error, 1)
	go func() {
		done <- PasswordSecurity("password").Handshake(server)
		_ = server.Close()
	}()

	challenge := make([]byte, 16)
	if _, err := io.ReadFull(viewer, challenge); err != nil {
		t.Fatal(err)
	}
	// VNC DES keys reverse the bit order of each password byte.
	cipher, err := des.NewCipher([]byte{0x0e, 0x86, 0xce, 0xce, 0xee, 0xf6, 0x4e, 0x26})
	if err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 16)
	cipher.Encrypt(response[:8], challenge[:8])
	cipher.Encrypt(response[8:], challenge[8:])
	if _, err := viewer.Write(response); err != nil {
		t.Fatal(err)
	}
	var status uint32
	if err := binary.Read(viewer, binary.BigEndian, &status); err != nil {
		t.Fatal(err)
	}
	if status != 0 {
		t.Fatalf("VNC authentication status = %d", status)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPasswordClientAuthenticatesAndCaptures(t *testing.T) {
	framebuffer, err := virtio.NewFramebuffer(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := framebuffer.Update(image.Rect(0, 0, 1, 1), []byte{0x30, 0x20, 0x10, 0}, 4); err != nil {
		t.Fatal(err)
	}
	serverSide, clientSide := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- (&Server{
			Desktop: &virtio.Desktop{
				Framebuffer: framebuffer,
				GPU:         virtio.NewGPU(0, 0x1000, 3, framebuffer),
			},
			Name:     "password-client-test",
			Security: PasswordSecurity("password"),
		}).ServeConn(context.Background(), serverSide)
	}()

	password := "password"
	client, err := newClient(clientSide, &password)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := client.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := frame.RGBAAt(0, 0); got.R != 0x10 || got.G != 0x20 || got.B != 0x30 || got.A != 0xff {
		t.Fatalf("captured pixel = %#v", got)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("password-protected RFB server did not finish after disconnect")
	}
}
