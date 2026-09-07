package rfb

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"os"
	"time"
)

type Client struct {
	conn        io.ReadWriteCloser
	reader      io.Reader
	width       int
	height      int
	name        string
	format      PixelFormat
	pixels      []byte
	buttons     uint8
	clipboard   string
	resize      bool
	readTimeout time.Duration
	password    *string
}

func Dial(ctx context.Context, address string) (*Client, error) {
	return dial(ctx, address, nil)
}

// DialPassword connects to an RFB server using standard VNC password
// authentication. The protocol accepts at most eight password bytes.
func DialPassword(ctx context.Context, address, password string) (*Client, error) {
	return dial(ctx, address, &password)
}

func dial(ctx context.Context, address string, password *string) (*Client, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	client, err := newClient(conn, password)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

func NewClient(conn io.ReadWriteCloser) (*Client, error) {
	return newClient(conn, nil)
}

func newClient(conn io.ReadWriteCloser, password *string) (*Client, error) {
	if conn == nil {
		return nil, fmt.Errorf("RFB connection is nil")
	}
	client := &Client{
		conn:        conn,
		reader:      bufio.NewReaderSize(conn, 64<<10),
		readTimeout: 30 * time.Second,
		password:    password,
	}
	if err := client.handshake(); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) input() io.Reader {
	if c.reader != nil {
		return c.reader
	}
	return c.conn
}

func (c *Client) Size() (int, int) {
	return c.width, c.height
}

func (c *Client) Name() string {
	return c.name
}

func (c *Client) Clipboard() string {
	return c.clipboard
}

func (c *Client) SupportsDesktopResize() bool {
	return c.resize
}

func (c *Client) Pixel(x, y int) (red, green, blue byte, ok bool) {
	if x < 0 || y < 0 || x >= c.width || y >= c.height {
		return 0, 0, 0, false
	}
	offset := (y*c.width + x) * 4
	return c.pixels[offset+2], c.pixels[offset+1], c.pixels[offset], true
}

func (c *Client) handshake() error {
	version := make([]byte, len(protocolVersion))
	if _, err := io.ReadFull(c.input(), version); err != nil {
		return err
	}
	if string(version) != protocolVersion {
		return fmt.Errorf("unsupported RFB version %q", version)
	}
	if _, err := io.WriteString(c.conn, protocolVersion); err != nil {
		return err
	}
	count := []byte{0}
	if _, err := io.ReadFull(c.input(), count); err != nil {
		return err
	}
	if count[0] == 0 {
		return fmt.Errorf("RFB server offered no security types")
	}
	types := make([]byte, int(count[0]))
	if _, err := io.ReadFull(c.input(), types); err != nil {
		return err
	}
	selected := byte(securityNone)
	if c.password != nil {
		selected = securityVNCAuthentication
	}
	found := false
	for _, securityType := range types {
		found = found || securityType == selected
	}
	if !found {
		return fmt.Errorf("RFB server does not offer requested security type %d", selected)
	}
	if _, err := c.conn.Write([]byte{selected}); err != nil {
		return err
	}
	if selected == securityVNCAuthentication {
		challenge := make([]byte, 16)
		if _, err := io.ReadFull(c.input(), challenge); err != nil {
			return err
		}
		response, err := passwordChallengeResponse(*c.password, challenge)
		if err != nil {
			return err
		}
		if _, err := c.conn.Write(response); err != nil {
			return err
		}
	}
	var securityResult uint32
	if err := binary.Read(c.input(), binary.BigEndian, &securityResult); err != nil {
		return err
	}
	if securityResult != 0 {
		return fmt.Errorf("RFB security failed with status %d", securityResult)
	}
	if _, err := c.conn.Write([]byte{1}); err != nil {
		return err
	}
	header := make([]byte, 24)
	if _, err := io.ReadFull(c.input(), header); err != nil {
		return err
	}
	c.width = int(binary.BigEndian.Uint16(header[0:2]))
	c.height = int(binary.BigEndian.Uint16(header[2:4]))
	format, err := decodePixelFormat(header[4:20])
	if err != nil {
		return err
	}
	c.format = format
	nameLength := binary.BigEndian.Uint32(header[20:24])
	if nameLength > 1<<20 {
		return fmt.Errorf("RFB desktop name is too large: %d", nameLength)
	}
	name := make([]byte, nameLength)
	if _, err := io.ReadFull(c.input(), name); err != nil {
		return err
	}
	c.name = string(name)
	c.pixels = make([]byte, c.width*c.height*4)
	return c.SetEncodings(encodingHextile, encodingRichCursor, encodingExtendedDesktopSize, encodingRaw)
}

func (c *Client) SetEncodings(encodings ...int32) error {
	raw := make([]byte, 4+len(encodings)*4)
	raw[0] = 2
	binary.BigEndian.PutUint16(raw[2:4], uint16(len(encodings)))
	for index, encoding := range encodings {
		binary.BigEndian.PutUint32(raw[4+index*4:8+index*4], uint32(encoding))
	}
	_, err := c.conn.Write(raw)
	return err
}

func (c *Client) RequestUpdate(rect image.Rectangle, incremental bool) error {
	rect = rect.Intersect(image.Rect(0, 0, c.width, c.height))
	raw := make([]byte, 10)
	raw[0] = 3
	if incremental {
		raw[1] = 1
	}
	binary.BigEndian.PutUint16(raw[2:4], uint16(rect.Min.X))
	binary.BigEndian.PutUint16(raw[4:6], uint16(rect.Min.Y))
	binary.BigEndian.PutUint16(raw[6:8], uint16(rect.Dx()))
	binary.BigEndian.PutUint16(raw[8:10], uint16(rect.Dy()))
	_, err := c.conn.Write(raw)
	return err
}

func (c *Client) Resize(width, height int) error {
	if width <= 0 || height <= 0 || width > 8192 || height > 8192 {
		return fmt.Errorf("invalid desktop dimensions %dx%d", width, height)
	}
	raw := make([]byte, 24)
	raw[0] = 251
	binary.BigEndian.PutUint16(raw[2:4], uint16(width))
	binary.BigEndian.PutUint16(raw[4:6], uint16(height))
	raw[6] = 1
	binary.BigEndian.PutUint32(raw[8:12], 1)
	binary.BigEndian.PutUint16(raw[16:18], uint16(width))
	binary.BigEndian.PutUint16(raw[18:20], uint16(height))
	_, err := c.conn.Write(raw)
	return err
}

func (c *Client) SetClipboard(text string) error {
	if len(text) > maxClipboardBytes {
		return fmt.Errorf("RFB clipboard text is too large: %d bytes", len(text))
	}
	raw := make([]byte, 8)
	raw[0] = 6
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(text)))
	if _, err := c.conn.Write(raw); err != nil {
		return err
	}
	_, err := io.WriteString(c.conn, text)
	return err
}

func (c *Client) ReadUpdate(ctx context.Context) (image.Rectangle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if conn, ok := c.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		deadline := time.Now().Add(c.readTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return image.Rectangle{}, err
		}
		defer conn.SetReadDeadline(time.Time{})
	}
	header := make([]byte, 4)
	for {
		if _, err := io.ReadFull(c.input(), header[:1]); err != nil {
			return image.Rectangle{}, err
		}
		switch header[0] {
		case 0:
			if _, err := io.ReadFull(c.input(), header[1:]); err != nil {
				return image.Rectangle{}, err
			}
			goto framebufferUpdate
		case 3:
			if _, err := io.ReadFull(c.input(), header[1:]); err != nil {
				return image.Rectangle{}, err
			}
			lengthRaw := make([]byte, 4)
			if _, err := io.ReadFull(c.input(), lengthRaw); err != nil {
				return image.Rectangle{}, err
			}
			length := binary.BigEndian.Uint32(lengthRaw)
			if length > maxClipboardBytes {
				return image.Rectangle{}, fmt.Errorf("RFB clipboard text is too large: %d bytes", length)
			}
			text := make([]byte, int(length))
			if _, err := io.ReadFull(c.input(), text); err != nil {
				return image.Rectangle{}, err
			}
			c.clipboard = string(text)
		default:
			return image.Rectangle{}, fmt.Errorf("unsupported RFB server message %d", header[0])
		}
	}

framebufferUpdate:
	count := binary.BigEndian.Uint16(header[2:4])
	dirty := image.Rectangle{}
	for index := uint16(0); index < count; index++ {
		rectHeader := make([]byte, 12)
		if _, err := io.ReadFull(c.input(), rectHeader); err != nil {
			return image.Rectangle{}, err
		}
		x := int(binary.BigEndian.Uint16(rectHeader[0:2]))
		y := int(binary.BigEndian.Uint16(rectHeader[2:4]))
		width := int(binary.BigEndian.Uint16(rectHeader[4:6]))
		height := int(binary.BigEndian.Uint16(rectHeader[6:8]))
		encoding := int32(binary.BigEndian.Uint32(rectHeader[8:12]))
		if encoding == encodingExtendedDesktopSize {
			c.resize = true
			screenHeader := make([]byte, 4)
			if _, err := io.ReadFull(c.input(), screenHeader); err != nil {
				return image.Rectangle{}, err
			}
			screenCount := int(screenHeader[0])
			screens := make([]byte, screenCount*16)
			if _, err := io.ReadFull(c.input(), screens); err != nil {
				return image.Rectangle{}, err
			}
			if y != 0 {
				return image.Rectangle{}, fmt.Errorf("RFB desktop resize failed with status %d", y)
			}
			if screenCount != 1 || width <= 0 || height <= 0 {
				return image.Rectangle{}, fmt.Errorf("invalid RFB desktop layout %dx%d with %d screens", width, height, screenCount)
			}
			c.resizeFramebuffer(width, height)
			continue
		}
		rect := image.Rect(x, y, x+width, y+height)
		if encoding == encodingRichCursor {
			bytesPerPixel := int(c.format.BitsPerPixel / 8)
			payload := int64(width*height*bytesPerPixel + ((width+7)/8)*height)
			if _, err := io.CopyN(io.Discard, readerOnly{c.input()}, payload); err != nil {
				return image.Rectangle{}, err
			}
			continue
		}
		if !rect.In(image.Rect(0, 0, c.width, c.height)) {
			return image.Rectangle{}, fmt.Errorf("RFB update rectangle %v is outside framebuffer", rect)
		}
		switch encoding {
		case encodingRaw:
			bytesPerPixel := int(c.format.BitsPerPixel / 8)
			raw := make([]byte, width*height*bytesPerPixel)
			if _, err := io.ReadFull(c.input(), raw); err != nil {
				return image.Rectangle{}, err
			}
			if err := c.applyRawRectangle(rect, raw); err != nil {
				return image.Rectangle{}, err
			}
		case encodingHextile:
			if err := c.readHextileRectangle(rect); err != nil {
				return image.Rectangle{}, err
			}
		default:
			return image.Rectangle{}, fmt.Errorf("unsupported RFB encoding %d", encoding)
		}
		if dirty.Empty() {
			dirty = rect
		} else {
			dirty = dirty.Union(rect)
		}
	}
	return dirty, nil
}

func (c *Client) resizeFramebuffer(width, height int) {
	pixels := make([]byte, width*height*4)
	copyWidth := min(width, c.width)
	copyHeight := min(height, c.height)
	for y := 0; y < copyHeight; y++ {
		copy(pixels[y*width*4:y*width*4+copyWidth*4], c.pixels[y*c.width*4:y*c.width*4+copyWidth*4])
	}
	c.width = width
	c.height = height
	c.pixels = pixels
}

func (c *Client) readHextileRectangle(rect image.Rectangle) error {
	bytesPerPixel := int(c.format.BitsPerPixel / 8)
	background := make([]byte, bytesPerPixel)
	foreground := make([]byte, bytesPerPixel)
	for tileY := rect.Min.Y; tileY < rect.Max.Y; tileY += 16 {
		tileHeight := min(16, rect.Max.Y-tileY)
		for tileX := rect.Min.X; tileX < rect.Max.X; tileX += 16 {
			tileWidth := min(16, rect.Max.X-tileX)
			subencoding := []byte{0}
			if _, err := io.ReadFull(c.input(), subencoding); err != nil {
				return err
			}
			tile := image.Rect(tileX, tileY, tileX+tileWidth, tileY+tileHeight)
			if subencoding[0]&1 != 0 {
				raw := make([]byte, tileWidth*tileHeight*bytesPerPixel)
				if _, err := io.ReadFull(c.input(), raw); err != nil {
					return err
				}
				if err := c.applyRawRectangle(tile, raw); err != nil {
					return err
				}
				continue
			}
			if subencoding[0]&2 != 0 {
				if _, err := io.ReadFull(c.input(), background); err != nil {
					return err
				}
			}
			if subencoding[0]&4 != 0 {
				if _, err := io.ReadFull(c.input(), foreground); err != nil {
					return err
				}
			}
			if err := c.fillEncodedRectangle(tile, background); err != nil {
				return err
			}
			if subencoding[0]&8 == 0 {
				continue
			}
			count := []byte{0}
			if _, err := io.ReadFull(c.input(), count); err != nil {
				return err
			}
			for range int(count[0]) {
				color := foreground
				if subencoding[0]&16 != 0 {
					color = make([]byte, bytesPerPixel)
					if _, err := io.ReadFull(c.input(), color); err != nil {
						return err
					}
				}
				geometry := []byte{0, 0}
				if _, err := io.ReadFull(c.input(), geometry); err != nil {
					return err
				}
				x := tileX + int(geometry[0]>>4)
				y := tileY + int(geometry[0]&0x0f)
				width := int(geometry[1]>>4) + 1
				height := int(geometry[1]&0x0f) + 1
				subrect := image.Rect(x, y, x+width, y+height)
				if !subrect.In(tile) {
					return fmt.Errorf("RFB Hextile subrectangle %v is outside tile %v", subrect, tile)
				}
				if err := c.fillEncodedRectangle(subrect, color); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *Client) fillEncodedRectangle(rect image.Rectangle, pixel []byte) error {
	bytesPerPixel := int(c.format.BitsPerPixel / 8)
	if len(pixel) != bytesPerPixel {
		return fmt.Errorf("encoded RFB pixel has %d bytes, want %d", len(pixel), bytesPerPixel)
	}
	if rect.Empty() || !rect.In(image.Rect(0, 0, c.width, c.height)) {
		return fmt.Errorf("encoded RFB rectangle %v is outside framebuffer", rect)
	}
	row := make([]byte, rect.Dx()*bytesPerPixel)
	for offset := 0; offset < len(row); offset += bytesPerPixel {
		copy(row[offset:], pixel)
	}
	raw := make([]byte, len(row)*rect.Dy())
	for y := 0; y < rect.Dy(); y++ {
		copy(raw[y*len(row):], row)
	}
	return c.applyRawRectangle(rect, raw)
}

func (c *Client) Capture(ctx context.Context) (*image.RGBA, error) {
	if err := c.RequestUpdate(image.Rect(0, 0, c.width, c.height), false); err != nil {
		return nil, err
	}
	if _, err := c.ReadUpdate(ctx); err != nil {
		return nil, err
	}
	out := image.NewRGBA(image.Rect(0, 0, c.width, c.height))
	for offset := 0; offset < c.width*c.height; offset++ {
		out.Pix[offset*4] = c.pixels[offset*4+2]
		out.Pix[offset*4+1] = c.pixels[offset*4+1]
		out.Pix[offset*4+2] = c.pixels[offset*4]
		out.Pix[offset*4+3] = 0xff
	}
	return out, nil
}

func (c *Client) CapturePNG(ctx context.Context, path string) error {
	frame, err := c.Capture(ctx)
	if err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, frame); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (c *Client) Key(keysym uint32, down bool) error {
	raw := make([]byte, 8)
	raw[0] = 4
	if down {
		raw[1] = 1
	}
	binary.BigEndian.PutUint32(raw[4:8], keysym)
	_, err := c.conn.Write(raw)
	return err
}

func (c *Client) Press(keysym uint32) error {
	if err := c.Key(keysym, true); err != nil {
		return err
	}
	return c.Key(keysym, false)
}

func (c *Client) Type(text string) error {
	for _, character := range text {
		keysym := uint32(character)
		shift := character >= 'A' && character <= 'Z' || shiftedASCII(character)
		if shift {
			if err := c.Key(0xffe1, true); err != nil {
				return err
			}
		}
		if err := c.Press(keysym); err != nil {
			return err
		}
		if shift {
			if err := c.Key(0xffe1, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) Pointer(x, y uint16, buttons uint8) error {
	raw := make([]byte, 6)
	raw[0] = 5
	raw[1] = buttons
	binary.BigEndian.PutUint16(raw[2:4], x)
	binary.BigEndian.PutUint16(raw[4:6], y)
	if _, err := c.conn.Write(raw); err != nil {
		return err
	}
	c.buttons = buttons
	return nil
}

func (c *Client) Click(x, y uint16, button uint8) error {
	if err := c.Pointer(x, y, c.buttons|button); err != nil {
		return err
	}
	return c.Pointer(x, y, c.buttons&^button)
}

func (c *Client) applyRawRectangle(rect image.Rectangle, raw []byte) error {
	bytesPerPixel := int(c.format.BitsPerPixel / 8)
	for y := 0; y < rect.Dy(); y++ {
		for x := 0; x < rect.Dx(); x++ {
			source := (y*rect.Dx() + x) * bytesPerPixel
			var value uint32
			if c.format.BigEndian {
				for index := 0; index < bytesPerPixel; index++ {
					value = value<<8 | uint32(raw[source+index])
				}
			} else {
				for index := 0; index < bytesPerPixel; index++ {
					value |= uint32(raw[source+index]) << (index * 8)
				}
			}
			red := componentToByte(value>>c.format.RedShift, c.format.RedMax)
			green := componentToByte(value>>c.format.GreenShift, c.format.GreenMax)
			blue := componentToByte(value>>c.format.BlueShift, c.format.BlueMax)
			target := ((rect.Min.Y+y)*c.width + rect.Min.X + x) * 4
			c.pixels[target] = blue
			c.pixels[target+1] = green
			c.pixels[target+2] = red
		}
	}
	return nil
}

func componentToByte(value uint32, max uint16) byte {
	if max == 0 {
		return 0
	}
	value &= uint32(max)
	return byte(value * 255 / uint32(max))
}

func shiftedASCII(character rune) bool {
	const shifted = "~!@#$%^&*()_+{}|:\"<>?"
	for _, candidate := range shifted {
		if character == candidate {
			return true
		}
	}
	return false
}
