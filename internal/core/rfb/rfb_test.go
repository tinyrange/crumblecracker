package rfb

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"image"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestServerClientCaptureAndInput(t *testing.T) {
	framebuffer, err := virtio.NewFramebuffer(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := framebuffer.Update(image.Rect(0, 0, 2, 1), []byte{
		0x30, 0x20, 0x10, 0,
		0x60, 0x50, 0x40, 0,
	}, 8); err != nil {
		t.Fatal(err)
	}
	keyboard := virtio.NewKeyboardInput(0, 0x1000, 1)
	pointer := virtio.NewAbsolutePointerInput(0, 0x1000, 2, 2, 1)
	gpu := virtio.NewGPU(0, 0x1000, 3, framebuffer)
	clipboard := virtio.NewClipboard()
	clipboard.SetFromGuest("initial guest clipboard")
	serverSide, clientSide := net.Pipe()
	serverDone := make(chan error, 1)
	security := &recordingSecurity{called: make(chan struct{})}
	go func() {
		serverDone <- (&Server{
			Desktop: &virtio.Desktop{
				Framebuffer: framebuffer,
				GPU:         gpu,
				Keyboard:    keyboard,
				Pointer:     pointer,
				Clipboard:   clipboard,
			},
			Name:     "glass-test",
			Security: security,
		}).ServeConn(context.Background(), serverSide)
	}()

	client, err := NewClient(clientSide)
	if err != nil {
		t.Fatal(err)
	}
	if client.Name() != "glass-test" {
		t.Fatalf("desktop name = %q", client.Name())
	}
	select {
	case <-security.called:
	default:
		t.Fatal("configured RFB security handshake was not used")
	}
	if err := client.SetEncodings(encodingHextile, encodingRichCursor, encodingExtendedDesktopSize, encodingRaw); err != nil {
		t.Fatal(err)
	}
	frame, err := client.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := frame.RGBAAt(0, 0); got.R != 0x10 || got.G != 0x20 || got.B != 0x30 || got.A != 0xff {
		t.Fatalf("first pixel = %#v", got)
	}
	if client.Clipboard() != "initial guest clipboard" {
		t.Fatalf("initial guest clipboard = %q", client.Clipboard())
	}
	if !client.SupportsDesktopResize() {
		t.Fatal("server did not advertise desktop resize support")
	}
	if err := client.Resize(1100, 760); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if width, height := client.Size(); width != 1096 || height != 760 {
		t.Fatalf("resized client = %dx%d, want 1096x760", width, height)
	}
	if width, height := framebuffer.Size(); width != 1096 || height != 760 {
		t.Fatalf("resized framebuffer = %dx%d, want 1096x760", width, height)
	}

	if err := client.SetClipboard("viewer clipboard"); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-clipboard.ToGuest():
		if text != "viewer clipboard" {
			t.Fatalf("viewer clipboard = %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("viewer clipboard did not reach desktop")
	}
	clipboard.SetFromGuest("guest clipboard")
	clipboardContext, cancelClipboard := context.WithTimeout(context.Background(), time.Second)
	defer cancelClipboard()
	for client.Clipboard() != "guest clipboard" {
		width, height := client.Size()
		if err := client.RequestUpdate(image.Rect(0, 0, width, height), false); err != nil {
			t.Fatal(err)
		}
		if _, err := client.ReadUpdate(clipboardContext); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Type("A"); err != nil {
		t.Fatal(err)
	}
	if err := client.Click(1, 0, 1); err != nil {
		t.Fatal(err)
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
		t.Fatal("RFB server did not finish after disconnect")
	}
}

func TestHextileCompressesSolidFramebuffer(t *testing.T) {
	const (
		width  = 128
		height = 64
	)
	source := make([]byte, width*height*4)
	for offset := 0; offset < len(source); offset += 4 {
		source[offset] = 0x30
		source[offset+1] = 0x20
		source[offset+2] = 0x10
	}
	hextile, err := encodeHextile(source, width, height, defaultPixelFormat)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodePixels(source, defaultPixelFormat)
	if err != nil {
		t.Fatal(err)
	}
	if len(hextile) >= len(raw)/10 {
		t.Fatalf("solid Hextile payload is %d bytes, raw payload is %d", len(hextile), len(raw))
	}
}

func TestFramebufferEncodingUsesClientPreference(t *testing.T) {
	if got := chooseFramebufferEncoding([]int32{7, encodingZRLE, encodingHextile, encodingRaw}); got != encodingZRLE {
		t.Fatalf("encoding = %d, want ZRLE", got)
	}
	if got := chooseFramebufferEncoding([]int32{7, encodingHextile, encodingRaw}); got != encodingHextile {
		t.Fatalf("encoding = %d, want Hextile fallback", got)
	}
}

func TestHextileCompressesSparseTile(t *testing.T) {
	const width = 16
	source := make([]byte, width*width*4)
	for offset := 0; offset < len(source); offset += 4 {
		source[offset], source[offset+1], source[offset+2] = 0x30, 0x20, 0x10
	}
	for x := 4; x < 12; x++ {
		offset := (8*width + x) * 4
		source[offset], source[offset+1], source[offset+2] = 0xff, 0xff, 0xff
	}
	hextile, err := encodeHextile(source, width, width, defaultPixelFormat)
	if err != nil {
		t.Fatal(err)
	}
	if hextile[0]&8 == 0 {
		t.Fatalf("sparse tile used subencoding %#x", hextile[0])
	}
	if len(hextile) >= len(source)/10 {
		t.Fatalf("sparse Hextile payload is %d bytes, raw payload is %d", len(hextile), len(source))
	}
	serverSide, clientSide := net.Pipe()
	go func() {
		_, _ = serverSide.Write(hextile)
		_ = serverSide.Close()
	}()
	client := &Client{
		conn:   clientSide,
		width:  width,
		height: width,
		format: defaultPixelFormat,
		pixels: make([]byte, len(source)),
	}
	if err := client.readHextileRectangle(image.Rect(0, 0, width, width)); err != nil {
		t.Fatal(err)
	}
	background := (2*width + 2) * 4
	foreground := (8*width + 8) * 4
	if client.pixels[background] != 0x30 || client.pixels[foreground] != 0xff {
		t.Fatalf("decoded sparse tile background=%#x foreground=%#x", client.pixels[background], client.pixels[foreground])
	}
}

func TestZRLECompressesDesktopAndKeepsOneStream(t *testing.T) {
	const (
		width  = 128
		height = 96
	)
	source := make([]byte, width*height*4)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			offset := (y*width + x) * 4
			source[offset] = byte(x / 8)
			source[offset+1] = byte(y / 8)
			source[offset+2] = 0x40
		}
	}

	encoder, err := newZRLEEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	var first bytes.Buffer
	if err := encoder.WriteRectangle(&first, source, width, height, defaultPixelFormat); err != nil {
		t.Fatal(err)
	}
	if first.Len() >= len(source)/8 {
		t.Fatalf("ZRLE payload is %d bytes, raw framebuffer is %d", first.Len(), len(source))
	}

	firstPayload := first.Bytes()
	compressedLength := int(binary.BigEndian.Uint32(firstPayload[:4]))
	if compressedLength != len(firstPayload)-4 {
		t.Fatalf("ZRLE length = %d, payload is %d", compressedLength, len(firstPayload)-4)
	}
	tileCount := ((width + 63) / 64) * ((height + 63) / 64)
	var second bytes.Buffer
	if err := encoder.WriteRectangle(&second, source, width, height, defaultPixelFormat); err != nil {
		t.Fatal(err)
	}
	secondPayload := second.Bytes()
	secondLength := int(binary.BigEndian.Uint32(secondPayload[:4]))
	if secondLength != len(secondPayload)-4 {
		t.Fatalf("second ZRLE length = %d, payload is %d", secondLength, len(secondPayload)-4)
	}

	var stream bytes.Buffer
	stream.Write(firstPayload[4:])
	stream.Write(secondPayload[4:])
	reader, err := zlib.NewReader(&stream)
	if err != nil {
		t.Fatal(err)
	}
	uncompressedLength := width*height*3 + tileCount
	uncompressed := make([]byte, uncompressedLength*2)
	if _, err := io.ReadFull(reader, uncompressed); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, uncompressedLength} {
		if uncompressed[offset] != 0 {
			t.Fatalf("ZRLE first tile subencoding = %d, want raw", uncompressed[offset])
		}
		if got := uncompressed[offset+1 : offset+4]; !bytes.Equal(got, []byte{0, 0, 0x40}) {
			t.Fatalf("ZRLE first compressed pixel = %v", got)
		}
	}
}

func BenchmarkZRLEDesktopFrame(b *testing.B) {
	const (
		width  = 1920
		height = 1080
	)
	source := make([]byte, width*height*4)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			offset := (y*width + x) * 4
			source[offset] = byte(x / 16)
			source[offset+1] = byte(y / 16)
			source[offset+2] = byte((x + y) / 32)
		}
	}
	b.SetBytes(int64(len(source)))
	b.ResetTimer()
	wireBytes := 0
	for range b.N {
		encoder, err := newZRLEEncoder()
		if err != nil {
			b.Fatal(err)
		}
		var payload bytes.Buffer
		if err := encoder.WriteRectangle(&payload, source, width, height, defaultPixelFormat); err != nil {
			b.Fatal(err)
		}
		if err := encoder.Close(); err != nil {
			b.Fatal(err)
		}
		wireBytes = payload.Len()
	}
	b.ReportMetric(float64(wireBytes), "wire-B/frame")
}

func TestRichCursorHasVisiblePixels(t *testing.T) {
	var payload bytes.Buffer
	if err := writeDefaultRichCursor(&payload, defaultPixelFormat); err != nil {
		t.Fatal(err)
	}
	header := payload.Bytes()[:12]
	width := int(binary.BigEndian.Uint16(header[4:6]))
	height := int(binary.BigEndian.Uint16(header[6:8]))
	encoding := int32(binary.BigEndian.Uint32(header[8:12]))
	if encoding != encodingRichCursor {
		t.Fatalf("cursor encoding = %d", encoding)
	}
	bytesPerPixel := int(defaultPixelFormat.BitsPerPixel / 8)
	mask := payload.Bytes()[12+width*height*bytesPerPixel:]
	visible := false
	for _, value := range mask {
		visible = visible || value != 0
	}
	if !visible {
		t.Fatal("cursor visibility mask is empty")
	}
}

type recordingSecurity struct {
	called chan struct{}
}

func (*recordingSecurity) Type() uint8 {
	return securityNone
}

func (s *recordingSecurity) Handshake(conn io.ReadWriter) error {
	close(s.called)
	return binary.Write(conn, binary.BigEndian, uint32(0))
}

func TestPixelFormatsRoundTrip(t *testing.T) {
	source := []byte{0x33, 0x22, 0x11, 0}
	for _, format := range []PixelFormat{
		defaultPixelFormat,
		{BitsPerPixel: 16, Depth: 16, TrueColor: true, RedMax: 31, GreenMax: 63, BlueMax: 31, RedShift: 11, GreenShift: 5},
		{BitsPerPixel: 16, Depth: 16, BigEndian: true, TrueColor: true, RedMax: 31, GreenMax: 63, BlueMax: 31, RedShift: 11, GreenShift: 5},
	} {
		encoded, err := encodePixels(source, format)
		if err != nil {
			t.Fatal(err)
		}
		client := &Client{width: 1, height: 1, format: format, pixels: make([]byte, 4)}
		if err := client.applyRawRectangle(image.Rect(0, 0, 1, 1), encoded); err != nil {
			t.Fatal(err)
		}
		if client.pixels[2] < 0x08 || client.pixels[2] > 0x18 {
			t.Fatalf("format %+v red = %#x", format, client.pixels[2])
		}
	}
}

func TestLinuxKeycode(t *testing.T) {
	for _, test := range []struct {
		keysym uint32
		code   uint16
	}{
		{'a', 30},
		{'A', 30},
		{'!', 2},
		{0xff0d, 28},
		{0xff52, 103},
	} {
		code, ok := LinuxKeycode(test.keysym)
		if !ok || code != test.code {
			t.Fatalf("keysym %#x = (%d, %v), want %d", test.keysym, code, ok, test.code)
		}
	}
}
