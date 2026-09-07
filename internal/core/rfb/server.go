package rfb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

const protocolVersion = "RFB 003.008\n"
const maxClipboardBytes = 64 << 20

const (
	securityNone                = 1
	encodingRaw                 = 0
	encodingHextile             = 5
	encodingZRLE                = 16
	encodingRichCursor          = -239
	encodingDesktopSize         = -223
	encodingExtendedDesktopSize = -308
)

// Security performs the RFB security-type handshake after protocol version
// negotiation. Additional implementations can add authenticated VNC access
// without changing framebuffer or input handling.
type Security interface {
	Type() uint8
	Handshake(io.ReadWriter) error
}

type NoneSecurity struct{}

func (NoneSecurity) Type() uint8 {
	return securityNone
}

func (NoneSecurity) Handshake(conn io.ReadWriter) error {
	return binary.Write(conn, binary.BigEndian, uint32(0))
}

type PixelFormat struct {
	BitsPerPixel uint8
	Depth        uint8
	BigEndian    bool
	TrueColor    bool
	RedMax       uint16
	GreenMax     uint16
	BlueMax      uint16
	RedShift     uint8
	GreenShift   uint8
	BlueShift    uint8
}

var defaultPixelFormat = PixelFormat{
	BitsPerPixel: 32,
	Depth:        24,
	TrueColor:    true,
	RedMax:       255,
	GreenMax:     255,
	BlueMax:      255,
	RedShift:     16,
	GreenShift:   8,
	BlueShift:    0,
}

type Server struct {
	Desktop  *virtio.Desktop
	Name     string
	Security Security

	activeMu sync.Mutex
	active   bool
	closing  bool
	conn     net.Conn
}

type framebufferRequest struct {
	incremental     bool
	rect            image.Rectangle
	format          PixelFormat
	encoding        int32
	richCursor      bool
	extendedDesktop bool
	resize          *desktopResize
}

type desktopResize struct {
	width  int
	height int
	reason uint16
	status uint16
}

func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return fmt.Errorf("RFB listener is nil")
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			_ = s.ServeConn(context.Background(), conn)
		}()
	}
}

func (s *Server) ServeConn(ctx context.Context, conn net.Conn) error {
	if s == nil || s.Desktop == nil || s.Desktop.Framebuffer == nil {
		return fmt.Errorf("RFB desktop is not configured")
	}
	s.activeMu.Lock()
	if s.active || s.closing {
		s.activeMu.Unlock()
		_ = conn.Close()
		return fmt.Errorf("RFB desktop is unavailable")
	}
	s.active = true
	s.conn = conn
	s.activeMu.Unlock()
	defer func() {
		s.activeMu.Lock()
		s.active = false
		if s.conn == conn {
			s.conn = nil
		}
		s.activeMu.Unlock()
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	defer conn.Close()

	security := s.Security
	if security == nil {
		security = NoneSecurity{}
	}
	if err := serverHandshake(conn, security); err != nil {
		return err
	}
	width, height := s.Desktop.Framebuffer.Size()
	if width > 0xffff || height > 0xffff {
		return fmt.Errorf("RFB framebuffer %dx%d exceeds protocol dimensions", width, height)
	}
	name := s.Name
	if name == "" {
		name = "cc glass"
	}
	if err := writeServerInit(conn, width, height, defaultPixelFormat, name); err != nil {
		return err
	}

	requests := make(chan framebufferRequest, 1)
	done := make(chan struct{})
	var writerErr error
	var writerMu sync.Mutex
	go func() {
		err := s.writeUpdates(ctx, conn, requests)
		writerMu.Lock()
		writerErr = err
		writerMu.Unlock()
		close(done)
		_ = conn.Close()
	}()

	err := s.readClientMessages(ctx, conn, requests)
	close(requests)
	<-done
	writerMu.Lock()
	defer writerMu.Unlock()
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if writerErr != nil && !errors.Is(writerErr, io.EOF) && !errors.Is(writerErr, net.ErrClosed) &&
		!errors.Is(writerErr, context.Canceled) {
		return writerErr
	}
	return nil
}

// Close disconnects the current viewer and prevents another connection from
// attaching. The listener is owned by the caller and should be closed too.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.activeMu.Lock()
	s.closing = true
	conn := s.conn
	s.activeMu.Unlock()
	if conn == nil {
		return nil
	}
	err := conn.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func serverHandshake(conn io.ReadWriter, security Security) error {
	if security == nil || security.Type() == 0 {
		return fmt.Errorf("RFB security is not configured")
	}
	if _, err := io.WriteString(conn, protocolVersion); err != nil {
		return err
	}
	version := make([]byte, len(protocolVersion))
	if _, err := io.ReadFull(conn, version); err != nil {
		return err
	}
	if string(version) != protocolVersion {
		return fmt.Errorf("unsupported RFB version %q", version)
	}
	if _, err := conn.Write([]byte{1, security.Type()}); err != nil {
		return err
	}
	selected := []byte{0}
	if _, err := io.ReadFull(conn, selected); err != nil {
		return err
	}
	if selected[0] != security.Type() {
		return fmt.Errorf("unsupported RFB security type %d", selected[0])
	}
	if err := security.Handshake(conn); err != nil {
		return err
	}
	clientInit := []byte{0}
	_, err := io.ReadFull(conn, clientInit)
	return err
}

func writeServerInit(w io.Writer, width, height int, format PixelFormat, name string) error {
	header := make([]byte, 24)
	binary.BigEndian.PutUint16(header[0:2], uint16(width))
	binary.BigEndian.PutUint16(header[2:4], uint16(height))
	encodePixelFormat(header[4:20], format)
	binary.BigEndian.PutUint32(header[20:24], uint32(len(name)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := io.WriteString(w, name)
	return err
}

func (s *Server) readClientMessages(ctx context.Context, conn io.Reader, requests chan framebufferRequest) error {
	previousButtons := uint8(0)
	format := defaultPixelFormat
	framebufferEncoding := int32(encodingRaw)
	richCursor := false
	extendedDesktop := false
	desktopLayoutPending := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		messageType := []byte{0}
		if _, err := io.ReadFull(conn, messageType); err != nil {
			return err
		}
		switch messageType[0] {
		case 0:
			raw := make([]byte, 19)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return err
			}
			nextFormat, err := decodePixelFormat(raw[3:])
			if err != nil {
				return err
			}
			format = nextFormat
		case 2:
			header := make([]byte, 3)
			if _, err := io.ReadFull(conn, header); err != nil {
				return err
			}
			count := binary.BigEndian.Uint16(header[1:3])
			raw := make([]byte, int(count)*4)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return err
			}
			encodings := make([]int32, count)
			for index := range encodings {
				encodings[index] = int32(binary.BigEndian.Uint32(raw[index*4 : index*4+4]))
			}
			framebufferEncoding = chooseFramebufferEncoding(encodings)
			richCursor = hasEncoding(encodings, encodingRichCursor)
			nextExtendedDesktop := hasEncoding(encodings, encodingExtendedDesktopSize)
			if nextExtendedDesktop && !extendedDesktop {
				desktopLayoutPending = true
			}
			extendedDesktop = nextExtendedDesktop
		case 3:
			raw := make([]byte, 9)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return err
			}
			x := int(binary.BigEndian.Uint16(raw[1:3]))
			y := int(binary.BigEndian.Uint16(raw[3:5]))
			width := int(binary.BigEndian.Uint16(raw[5:7]))
			height := int(binary.BigEndian.Uint16(raw[7:9]))
			request := framebufferRequest{
				incremental:     raw[0] != 0,
				rect:            image.Rect(x, y, x+width, y+height),
				format:          format,
				encoding:        framebufferEncoding,
				richCursor:      richCursor,
				extendedDesktop: extendedDesktop,
			}
			if desktopLayoutPending {
				currentWidth, currentHeight := s.Desktop.Framebuffer.Size()
				request.resize = &desktopResize{
					width:  currentWidth,
					height: currentHeight,
				}
				desktopLayoutPending = false
			}
			sendLatestRequest(requests, request)
		case 4:
			raw := make([]byte, 7)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return err
			}
			if s.Desktop.Keyboard == nil {
				continue
			}
			keysym := binary.BigEndian.Uint32(raw[3:7])
			if code, ok := LinuxKeycode(keysym); ok {
				if err := s.Desktop.Keyboard.Key(code, raw[0] != 0); err != nil {
					return err
				}
			}
		case 5:
			raw := make([]byte, 5)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return err
			}
			if s.Desktop.Pointer == nil {
				continue
			}
			x := uint32(binary.BigEndian.Uint16(raw[1:3]))
			y := uint32(binary.BigEndian.Uint16(raw[3:5]))
			width, height := s.Desktop.Framebuffer.Size()
			if width > 0 && x >= uint32(width) {
				x = uint32(width - 1)
			}
			if height > 0 && y >= uint32(height) {
				y = uint32(height - 1)
			}
			if err := s.Desktop.Pointer.PointerEvent(x, y, raw[0], previousButtons); err != nil {
				return err
			}
			previousButtons = raw[0]
		case 6:
			raw := make([]byte, 7)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return err
			}
			length := binary.BigEndian.Uint32(raw[3:7])
			if length > maxClipboardBytes {
				return fmt.Errorf("RFB clipboard text is too large: %d bytes", length)
			}
			text := make([]byte, int(length))
			if _, err := io.ReadFull(conn, text); err != nil {
				return err
			}
			if s.Desktop.Clipboard != nil {
				s.Desktop.Clipboard.SetFromFrontend(string(text))
			}
		case 251:
			raw := make([]byte, 7)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return err
			}
			width := int(binary.BigEndian.Uint16(raw[1:3]))
			height := int(binary.BigEndian.Uint16(raw[3:5]))
			screenCount := int(raw[5])
			screens := make([]byte, screenCount*16)
			if _, err := io.ReadFull(conn, screens); err != nil {
				return err
			}
			status := uint16(0)
			if !extendedDesktop || screenCount != 1 ||
				binary.BigEndian.Uint16(screens[4:6]) != 0 ||
				binary.BigEndian.Uint16(screens[6:8]) != 0 ||
				int(binary.BigEndian.Uint16(screens[8:10])) != width ||
				int(binary.BigEndian.Uint16(screens[10:12])) != height {
				status = 3
			} else {
				width, height = supportedDesktopSize(width, height)
				if err := s.Desktop.Resize(width, height); err != nil {
					status = 3
				}
			}
			currentWidth, currentHeight := s.Desktop.Framebuffer.Size()
			request := framebufferRequest{
				format:          format,
				encoding:        framebufferEncoding,
				richCursor:      richCursor,
				extendedDesktop: extendedDesktop,
				resize: &desktopResize{
					width:  currentWidth,
					height: currentHeight,
					reason: 1,
					status: status,
				},
			}
			sendLatestRequest(requests, request)
		default:
			return fmt.Errorf("unsupported RFB client message %d", messageType[0])
		}
	}
}

func supportedDesktopSize(width, height int) (int, int) {
	// Linux's virtio-gpu DRM driver constructs hotplug modes using CVT's
	// eight-pixel horizontal character cells. Keep the host framebuffer and
	// the mode visible to guest userspace identical for arbitrary VNC window
	// sizes instead of accepting a size the guest silently rounds down.
	if aligned := width &^ 7; aligned > 0 {
		width = aligned
	}
	return width, height
}

func (s *Server) writeUpdates(ctx context.Context, conn io.Writer, requests <-chan framebufferRequest) error {
	zrle, err := newZRLEEncoder()
	if err != nil {
		return err
	}
	defer zrle.Close()

	generation := uint64(0)
	cursorSent := false
	cursorFormat := PixelFormat{}
	clipboardChanged := (<-chan struct{})(nil)
	if s.Desktop.Clipboard != nil {
		if text, generation := s.Desktop.Clipboard.GuestSnapshot(); generation != 0 {
			if err := writeServerCutText(conn, text); err != nil {
				return err
			}
		}
		clipboardChanged = s.Desktop.Clipboard.GuestChanged()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clipboardChanged:
			text, _ := s.Desktop.Clipboard.GuestSnapshot()
			if err := writeServerCutText(conn, text); err != nil {
				return err
			}
			clipboardChanged = s.Desktop.Clipboard.GuestChanged()
		case request, ok := <-requests:
			if !ok {
				return nil
			}
			if request.resize != nil && request.rect.Empty() {
				if err := writeFramebufferUpdate(conn, virtio.FramebufferUpdate{}, request.format, request.encoding, false, request.resize, zrle); err != nil {
					return err
				}
				continue
			}
			if request.incremental {
				for {
					update, err := s.Desktop.Snapshot(request.rect, generation, true)
					if err != nil {
						return err
					}
					sendCursor := request.richCursor && (!cursorSent || cursorFormat != request.format)
					if !update.Rect.Empty() || sendCursor || request.resize != nil {
						if err := writeFramebufferUpdate(conn, update, request.format, request.encoding, sendCursor, request.resize, zrle); err != nil {
							return err
						}
						generation = update.Generation
						if sendCursor {
							cursorSent = true
							cursorFormat = request.format
						}
						break
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case next, ok := <-requests:
						if !ok {
							return nil
						}
						request = next
						if request.resize != nil && request.rect.Empty() {
							if err := writeFramebufferUpdate(conn, virtio.FramebufferUpdate{}, request.format, request.encoding, false, request.resize, zrle); err != nil {
								return err
							}
							goto nextRequest
						}
						if !request.incremental {
							update, err = s.Desktop.Snapshot(request.rect, generation, false)
							if err != nil {
								return err
							}
							sendCursor = request.richCursor && (!cursorSent || cursorFormat != request.format)
							if err := writeFramebufferUpdate(conn, update, request.format, request.encoding, sendCursor, request.resize, zrle); err != nil {
								return err
							}
							generation = update.Generation
							if sendCursor {
								cursorSent = true
								cursorFormat = request.format
							}
							goto nextRequest
						}
					case <-s.Desktop.Framebuffer.Changed():
					case <-clipboardChanged:
						text, _ := s.Desktop.Clipboard.GuestSnapshot()
						if err := writeServerCutText(conn, text); err != nil {
							return err
						}
						clipboardChanged = s.Desktop.Clipboard.GuestChanged()
					}
				}
			} else {
				update, err := s.Desktop.Snapshot(request.rect, generation, false)
				if err != nil {
					return err
				}
				sendCursor := request.richCursor && (!cursorSent || cursorFormat != request.format)
				if err := writeFramebufferUpdate(conn, update, request.format, request.encoding, sendCursor, request.resize, zrle); err != nil {
					return err
				}
				generation = update.Generation
				if sendCursor {
					cursorSent = true
					cursorFormat = request.format
				}
			}
		nextRequest:
		}
	}
}

func writeFramebufferUpdate(w io.Writer, update virtio.FramebufferUpdate, format PixelFormat, encoding int32, sendCursor bool, resize *desktopResize, zrle *zrleEncoder) error {
	header := []byte{0, 0, 0, 0}
	count := uint16(0)
	if !update.Rect.Empty() {
		count++
	}
	if sendCursor {
		count++
	}
	if resize != nil {
		count++
	}
	binary.BigEndian.PutUint16(header[2:4], count)
	if _, err := w.Write(header); err != nil {
		return err
	}
	if !update.Rect.Empty() {
		if err := writeFramebufferRectangle(w, update, format, encoding, zrle); err != nil {
			return err
		}
	}
	if sendCursor {
		if err := writeDefaultRichCursor(w, format); err != nil {
			return err
		}
	}
	if resize != nil {
		return writeExtendedDesktopSize(w, *resize)
	}
	return nil
}

func writeExtendedDesktopSize(w io.Writer, resize desktopResize) error {
	raw := make([]byte, 32)
	binary.BigEndian.PutUint16(raw[0:2], resize.reason)
	binary.BigEndian.PutUint16(raw[2:4], resize.status)
	binary.BigEndian.PutUint16(raw[4:6], uint16(resize.width))
	binary.BigEndian.PutUint16(raw[6:8], uint16(resize.height))
	encoding := int32(encodingExtendedDesktopSize)
	binary.BigEndian.PutUint32(raw[8:12], uint32(encoding))
	raw[12] = 1
	binary.BigEndian.PutUint32(raw[16:20], 1)
	binary.BigEndian.PutUint16(raw[24:26], uint16(resize.width))
	binary.BigEndian.PutUint16(raw[26:28], uint16(resize.height))
	_, err := w.Write(raw)
	return err
}

func writeServerCutText(w io.Writer, text string) error {
	if len(text) > maxClipboardBytes {
		return fmt.Errorf("RFB clipboard text is too large: %d bytes", len(text))
	}
	raw := make([]byte, 8)
	raw[0] = 3
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(text)))
	if _, err := w.Write(raw); err != nil {
		return err
	}
	_, err := io.WriteString(w, text)
	return err
}

func writeFramebufferRectangle(w io.Writer, update virtio.FramebufferUpdate, format PixelFormat, encoding int32, zrle *zrleEncoder) error {
	rect := make([]byte, 12)
	binary.BigEndian.PutUint16(rect[0:2], uint16(update.Rect.Min.X))
	binary.BigEndian.PutUint16(rect[2:4], uint16(update.Rect.Min.Y))
	binary.BigEndian.PutUint16(rect[4:6], uint16(update.Rect.Dx()))
	binary.BigEndian.PutUint16(rect[6:8], uint16(update.Rect.Dy()))
	binary.BigEndian.PutUint32(rect[8:12], uint32(encoding))
	if _, err := w.Write(rect); err != nil {
		return err
	}
	switch encoding {
	case encodingZRLE:
		if zrle == nil {
			return fmt.Errorf("ZRLE encoder is unavailable")
		}
		return zrle.WriteRectangle(w, update.Pixels, update.Rect.Dx(), update.Rect.Dy(), format)
	case encodingHextile:
		return writeHextile(w, update.Pixels, update.Rect.Dx(), update.Rect.Dy(), format)
	default:
		pixels, err := encodePixels(update.Pixels, format)
		if err != nil {
			return err
		}
		_, err = w.Write(pixels)
		return err
	}
}

func chooseFramebufferEncoding(encodings []int32) int32 {
	for _, encoding := range encodings {
		switch encoding {
		case encodingZRLE, encodingHextile, encodingRaw:
			return encoding
		}
	}
	return encodingRaw
}

func hasEncoding(encodings []int32, target int32) bool {
	for _, encoding := range encodings {
		if encoding == target {
			return true
		}
	}
	return false
}

func sendLatestRequest(requests chan framebufferRequest, request framebufferRequest) {
	select {
	case requests <- request:
		return
	default:
	}
	var previous framebufferRequest
	select {
	case previous = <-requests:
	default:
	}
	if previous.resize != nil && request.resize == nil {
		request = previous
	}
	select {
	case requests <- request:
	default:
	}
}

func encodeHextile(src []byte, width, height int, format PixelFormat) ([]byte, error) {
	var out bytes.Buffer
	if err := writeHextile(&out, src, width, height, format); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeHextile(w io.Writer, src []byte, width, height int, format PixelFormat) error {
	bytesPerPixel, err := encodedPixelSize(format)
	if err != nil {
		return err
	}
	const maxInt = int(^uint(0) >> 1)
	if width <= 0 || height <= 0 || width > maxInt/4 || height > maxInt/(width*4) ||
		len(src) != width*height*4 {
		return fmt.Errorf("invalid Hextile framebuffer %dx%d with %d bytes", width, height, len(src))
	}

	out := bufio.NewWriterSize(w, 64<<10)
	var background [4]byte
	backgroundSet := false
	var pixel [4]byte
	var colorKeys [512]uint32
	var colorCounts [512]uint16
	var subrectPayload [1 + 4 + 1 + 255*(4+2)]byte
	for tileY := 0; tileY < height; tileY += 16 {
		tileHeight := min(16, height-tileY)
		for tileX := 0; tileX < width; tileX += 16 {
			tileWidth := min(16, width-tileX)
			firstOffset := (tileY*width + tileX) * 4
			first := src[firstOffset : firstOffset+4]
			solid := true
			for y := 0; y < tileHeight && solid; y++ {
				row := ((tileY+y)*width + tileX) * 4
				for x := 0; x < tileWidth; x++ {
					offset := row + x*4
					if src[offset] != first[0] || src[offset+1] != first[1] || src[offset+2] != first[2] {
						solid = false
						break
					}
				}
			}
			if solid {
				encodePixel(pixel[:bytesPerPixel], first, format)
				if backgroundSet && bytes.Equal(background[:bytesPerPixel], pixel[:bytesPerPixel]) {
					if err := out.WriteByte(0); err != nil {
						return err
					}
					continue
				}
				if err := out.WriteByte(2); err != nil { // BackgroundSpecified
					return err
				}
				if _, err := out.Write(pixel[:bytesPerPixel]); err != nil {
					return err
				}
				copy(background[:], pixel[:bytesPerPixel])
				backgroundSet = true
				continue
			}

			clear(colorCounts[:])
			var backgroundColor uint32
			var backgroundCount uint16
			for y := 0; y < tileHeight; y++ {
				row := ((tileY+y)*width + tileX) * 4
				for x := 0; x < tileWidth; x++ {
					offset := row + x*4
					color := uint32(src[offset]) | uint32(src[offset+1])<<8 | uint32(src[offset+2])<<16
					slot := int((color * 2654435761) >> 23)
					for colorCounts[slot] != 0 && colorKeys[slot] != color {
						slot = (slot + 1) & (len(colorKeys) - 1)
					}
					colorKeys[slot] = color
					colorCounts[slot]++
					if colorCounts[slot] > backgroundCount {
						backgroundColor = color
						backgroundCount = colorCounts[slot]
					}
				}
			}

			backgroundSource := [4]byte{
				byte(backgroundColor),
				byte(backgroundColor >> 8),
				byte(backgroundColor >> 16),
			}
			encodePixel(pixel[:bytesPerPixel], backgroundSource[:], format)
			payload := subrectPayload[:1]
			payload[0] = 8 | 16 // AnySubrects | SubrectsColoured
			if !backgroundSet || !bytes.Equal(background[:bytesPerPixel], pixel[:bytesPerPixel]) {
				payload[0] |= 2 // BackgroundSpecified
				payload = append(payload, pixel[:bytesPerPixel]...)
			}
			countOffset := len(payload)
			payload = append(payload, 0)
			subrectCount := 0
			for y := 0; y < tileHeight && subrectCount <= 255; y++ {
				row := ((tileY+y)*width + tileX) * 4
				for x := 0; x < tileWidth; {
					offset := row + x*4
					color := uint32(src[offset]) | uint32(src[offset+1])<<8 | uint32(src[offset+2])<<16
					if color == backgroundColor {
						x++
						continue
					}
					start := x
					for x++; x < tileWidth; x++ {
						next := row + x*4
						nextColor := uint32(src[next]) | uint32(src[next+1])<<8 | uint32(src[next+2])<<16
						if nextColor != color {
							break
						}
					}
					subrectCount++
					if subrectCount > 255 {
						break
					}
					target := len(payload)
					payload = payload[:target+bytesPerPixel]
					encodePixel(payload[target:], src[offset:offset+4], format)
					payload = append(payload,
						byte(start<<4|y),
						byte((x-start-1)<<4),
					)
				}
			}
			rawSize := 1 + tileWidth*tileHeight*bytesPerPixel
			if subrectCount <= 255 && len(payload) < rawSize {
				payload[countOffset] = byte(subrectCount)
				if _, err := out.Write(payload); err != nil {
					return err
				}
				copy(background[:], pixel[:bytesPerPixel])
				backgroundSet = true
				continue
			}

			if err := out.WriteByte(1); err != nil { // Raw
				return err
			}
			if format == defaultPixelFormat {
				for y := 0; y < tileHeight; y++ {
					offset := ((tileY+y)*width + tileX) * 4
					if _, err := out.Write(src[offset : offset+tileWidth*4]); err != nil {
						return err
					}
				}
				continue
			}
			for y := 0; y < tileHeight; y++ {
				offset := ((tileY+y)*width + tileX) * 4
				for x := 0; x < tileWidth; x++ {
					source := src[offset+x*4 : offset+x*4+4]
					encodePixel(pixel[:bytesPerPixel], source, format)
					if _, err := out.Write(pixel[:bytesPerPixel]); err != nil {
						return err
					}
				}
			}
		}
	}
	return out.Flush()
}

func writeDefaultRichCursor(w io.Writer, format PixelFormat) error {
	const (
		width   = 15
		height  = 15
		hotspot = 7
	)
	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[0:2], hotspot)
	binary.BigEndian.PutUint16(header[2:4], hotspot)
	binary.BigEndian.PutUint16(header[4:6], width)
	binary.BigEndian.PutUint16(header[6:8], height)
	encoding := int32(encodingRichCursor)
	binary.BigEndian.PutUint32(header[8:12], uint32(encoding))
	if _, err := w.Write(header); err != nil {
		return err
	}

	pixels := make([]byte, width*height*4)
	mask := make([]byte, ((width+7)/8)*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			visible := (x >= hotspot-1 && x <= hotspot+1) || (y >= hotspot-1 && y <= hotspot+1)
			if !visible {
				continue
			}
			offset := (y*width + x) * 4
			if x == hotspot || y == hotspot {
				pixels[offset], pixels[offset+1], pixels[offset+2] = 0xff, 0xff, 0xff
			}
			mask[y*((width+7)/8)+x/8] |= 1 << (7 - uint(x%8))
		}
	}
	encoded, err := encodePixels(pixels, format)
	if err != nil {
		return err
	}
	if _, err := w.Write(encoded); err != nil {
		return err
	}
	_, err = w.Write(mask)
	return err
}

func encodePixels(src []byte, format PixelFormat) ([]byte, error) {
	bytesPerPixel, err := encodedPixelSize(format)
	if err != nil {
		return nil, err
	}
	if len(src)%4 != 0 {
		return nil, fmt.Errorf("invalid XRGB framebuffer length %d", len(src))
	}
	out := make([]byte, len(src)/4*bytesPerPixel)
	for source, target := 0, 0; source+4 <= len(src); source, target = source+4, target+bytesPerPixel {
		encodePixel(out[target:target+bytesPerPixel], src[source:source+4], format)
	}
	return out, nil
}

func encodedPixelSize(format PixelFormat) (int, error) {
	if !format.TrueColor || format.BitsPerPixel != 8 && format.BitsPerPixel != 16 && format.BitsPerPixel != 32 {
		return 0, fmt.Errorf("unsupported RFB pixel format: %+v", format)
	}
	return int(format.BitsPerPixel / 8), nil
}

func encodePixel(dst, src []byte, format PixelFormat) {
	if format == defaultPixelFormat {
		copy(dst, src)
		return
	}
	blue := uint32(src[0])
	green := uint32(src[1])
	red := uint32(src[2])
	value := (red*uint32(format.RedMax)/255)<<format.RedShift |
		(green*uint32(format.GreenMax)/255)<<format.GreenShift |
		(blue*uint32(format.BlueMax)/255)<<format.BlueShift
	if format.BigEndian {
		for index := range dst {
			dst[index] = byte(value >> ((len(dst) - index - 1) * 8))
		}
		return
	}
	for index := range dst {
		dst[index] = byte(value >> (index * 8))
	}
}

func encodePixelFormat(dst []byte, format PixelFormat) {
	dst[0] = format.BitsPerPixel
	dst[1] = format.Depth
	if format.BigEndian {
		dst[2] = 1
	}
	if format.TrueColor {
		dst[3] = 1
	}
	binary.BigEndian.PutUint16(dst[4:6], format.RedMax)
	binary.BigEndian.PutUint16(dst[6:8], format.GreenMax)
	binary.BigEndian.PutUint16(dst[8:10], format.BlueMax)
	dst[10] = format.RedShift
	dst[11] = format.GreenShift
	dst[12] = format.BlueShift
}

func decodePixelFormat(src []byte) (PixelFormat, error) {
	if len(src) < 16 {
		return PixelFormat{}, fmt.Errorf("short RFB pixel format")
	}
	format := PixelFormat{
		BitsPerPixel: src[0],
		Depth:        src[1],
		BigEndian:    src[2] != 0,
		TrueColor:    src[3] != 0,
		RedMax:       binary.BigEndian.Uint16(src[4:6]),
		GreenMax:     binary.BigEndian.Uint16(src[6:8]),
		BlueMax:      binary.BigEndian.Uint16(src[8:10]),
		RedShift:     src[10],
		GreenShift:   src[11],
		BlueShift:    src[12],
	}
	if !format.TrueColor || format.BitsPerPixel != 8 && format.BitsPerPixel != 16 && format.BitsPerPixel != 32 {
		return PixelFormat{}, fmt.Errorf("unsupported RFB pixel format: %+v", format)
	}
	return format, nil
}

type readerOnly struct {
	io.Reader
}
