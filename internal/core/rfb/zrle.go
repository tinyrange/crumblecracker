package rfb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

// zrleEncoder owns the connection-wide zlib stream required by RFC 6143.
// Resetting compressed only discards bytes already sent; the zlib writer
// retains its dictionary across framebuffer rectangles.
type zrleEncoder struct {
	compressed bytes.Buffer
	stream     *zlib.Writer
}

func newZRLEEncoder() (*zrleEncoder, error) {
	encoder := &zrleEncoder{}
	stream, err := zlib.NewWriterLevel(&encoder.compressed, zlib.BestSpeed)
	if err != nil {
		return nil, fmt.Errorf("create ZRLE stream: %w", err)
	}
	encoder.stream = stream
	return encoder, nil
}

func (z *zrleEncoder) Close() error {
	if z == nil || z.stream == nil {
		return nil
	}
	return z.stream.Close()
}

func (z *zrleEncoder) WriteRectangle(w io.Writer, src []byte, width, height int, format PixelFormat) error {
	if z == nil || z.stream == nil {
		return fmt.Errorf("ZRLE encoder is unavailable")
	}
	const maxInt = int(^uint(0) >> 1)
	if width <= 0 || height <= 0 || width > maxInt/4 || height > maxInt/(width*4) ||
		len(src) != width*height*4 {
		return fmt.Errorf("invalid ZRLE framebuffer %dx%d with %d bytes", width, height, len(src))
	}

	var row [64 * 4]byte
	for tileY := 0; tileY < height; tileY += 64 {
		tileHeight := min(64, height-tileY)
		for tileX := 0; tileX < width; tileX += 64 {
			tileWidth := min(64, width-tileX)
			if _, err := z.stream.Write([]byte{0}); err != nil { // raw TRLE tile
				return fmt.Errorf("encode ZRLE tile: %w", err)
			}
			if err := writeCPixels(z.stream, src, width, tileX, tileY, tileWidth, tileHeight, format, row[:]); err != nil {
				return err
			}
		}
	}
	if err := z.stream.Flush(); err != nil {
		return fmt.Errorf("flush ZRLE stream: %w", err)
	}
	if z.compressed.Len() > int(^uint32(0)) {
		return fmt.Errorf("ZRLE rectangle is too large: %d bytes", z.compressed.Len())
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(z.compressed.Len()))
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	if _, err := z.compressed.WriteTo(w); err != nil {
		return err
	}
	z.compressed.Reset()
	return nil
}

func writeCPixels(w io.Writer, src []byte, width, tileX, tileY, tileWidth, tileHeight int, format PixelFormat, row []byte) error {
	bytesPerPixel, compactOffset, err := compressedPixelLayout(format)
	if err != nil {
		return err
	}
	if len(row) < tileWidth*bytesPerPixel {
		return fmt.Errorf("ZRLE row buffer has %d bytes, needs %d", len(row), tileWidth*bytesPerPixel)
	}
	if bytesPerPixel == 3 && format == defaultPixelFormat {
		for y := 0; y < tileHeight; y++ {
			offset := ((tileY+y)*width + tileX) * 4
			target := 0
			for x := 0; x < tileWidth; x++ {
				copy(row[target:target+3], src[offset+x*4:offset+x*4+3])
				target += 3
			}
			if _, err := w.Write(row[:target]); err != nil {
				return err
			}
		}
		return nil
	}

	encodedBytes := int(format.BitsPerPixel / 8)
	var pixel [4]byte
	for y := 0; y < tileHeight; y++ {
		offset := ((tileY+y)*width + tileX) * 4
		target := 0
		for x := 0; x < tileWidth; x++ {
			encodePixel(pixel[:encodedBytes], src[offset+x*4:offset+x*4+4], format)
			copy(row[target:target+bytesPerPixel], pixel[compactOffset:compactOffset+bytesPerPixel])
			target += bytesPerPixel
		}
		if _, err := w.Write(row[:target]); err != nil {
			return err
		}
	}
	return nil
}

func compressedPixelLayout(format PixelFormat) (size int, offset int, err error) {
	size, err = encodedPixelSize(format)
	if err != nil {
		return 0, 0, err
	}
	if format.BitsPerPixel != 32 || format.Depth > 24 {
		return size, 0, nil
	}
	highest := max(
		int(format.RedShift)+bitWidth(format.RedMax),
		int(format.GreenShift)+bitWidth(format.GreenMax),
		int(format.BlueShift)+bitWidth(format.BlueMax),
	)
	lowest := min(int(format.RedShift), int(format.GreenShift), int(format.BlueShift))
	if highest <= 24 {
		if format.BigEndian {
			return 3, 1, nil
		}
		return 3, 0, nil
	}
	if lowest >= 8 {
		if format.BigEndian {
			return 3, 0, nil
		}
		return 3, 1, nil
	}
	return size, 0, nil
}

func bitWidth(value uint16) int {
	width := 0
	for value != 0 {
		width++
		value >>= 1
	}
	return width
}
