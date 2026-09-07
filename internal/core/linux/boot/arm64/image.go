package arm64

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	imageHeaderSizeBytes = 64
	ImageLoadAlignment   = 2 * 1024 * 1024
	arm64ImageMagic      = 0x644d5241
	maxGzipScanBytes     = 1 << 20
	maxCachedImages      = 4
)

type imageCacheKey struct {
	offset int64
	size   int64
	sum    [sha256.Size]byte
}

var extractedImageCache = struct {
	sync.Mutex
	entries map[imageCacheKey][]byte
	order   []imageCacheKey
}{
	entries: map[imageCacheKey][]byte{},
}

type KernelHeader struct {
	Code0      uint32
	Code1      uint32
	TextOffset uint64
	ImageSize  uint64
	Flags      uint64
	Res2       uint64
	Res3       uint64
	Res4       uint64
	Magic      uint32
	Res5       uint32
}

func (h KernelHeader) EntryPoint(base uint64) (uint64, error) {
	if base&(ImageLoadAlignment-1) != 0 {
		return 0, fmt.Errorf("arm64 kernel base must be 2 MiB aligned (got %#x)", base)
	}
	return base + h.TextOffset, nil
}

type ImageProbe struct {
	Header             KernelHeader
	NeedsDecompression bool
	CompressedOffset   int64
}

func ProbeKernelImage(reader io.ReaderAt, size int64) (*ImageProbe, error) {
	if reader == nil {
		return nil, fmt.Errorf("arm64 probe requires a reader")
	}
	if size <= 0 {
		return nil, fmt.Errorf("arm64 kernel image size must be positive")
	}

	header, err := readKernelHeaderAt(reader, 0)
	if err == nil {
		return &ImageProbe{Header: header}, nil
	}
	rawErr := err

	offset, err := findGzipPayload(reader, size)
	if err != nil {
		return nil, fmt.Errorf("arm64 kernel header not found: %w", rawErr)
	}
	header, err = readGzipKernelHeader(reader, offset, size)
	if err != nil {
		return nil, err
	}
	return &ImageProbe{
		Header:             header,
		NeedsDecompression: true,
		CompressedOffset:   offset,
	}, nil
}

func (p ImageProbe) ExtractImage(reader io.ReaderAt, size int64) ([]byte, error) {
	if !p.NeedsDecompression {
		data := make([]byte, int(size))
		if _, err := reader.ReadAt(data, 0); err != nil {
			return nil, fmt.Errorf("read raw arm64 image: %w", err)
		}
		return data, nil
	}

	compressed, err := readCompressedPayload(reader, p.CompressedOffset, size)
	if err != nil {
		return nil, err
	}
	key := imageCacheKey{
		offset: p.CompressedOffset,
		size:   int64(len(compressed)),
		sum:    sha256.Sum256(compressed),
	}
	if data, ok := cachedExtractedImage(key); ok {
		return data, nil
	}

	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open gzip reader: %w", err)
	}
	gz.Multistream(false)
	defer gz.Close()

	data, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("decompress arm64 image: %w", err)
	}
	storeExtractedImage(key, data)
	return data, nil
}

func readCompressedPayload(reader io.ReaderAt, offset, size int64) ([]byte, error) {
	if offset < 0 || offset > size {
		return nil, fmt.Errorf("invalid gzip payload offset %d for kernel size %d", offset, size)
	}
	compressedSize := size - offset
	if compressedSize > int64(int(compressedSize)) {
		return nil, fmt.Errorf("gzip payload too large: %d bytes", compressedSize)
	}
	data := make([]byte, int(compressedSize))
	n, err := reader.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read gzip payload: %w", err)
	}
	if n != len(data) {
		return nil, fmt.Errorf("read gzip payload: %w", io.ErrUnexpectedEOF)
	}
	return data, nil
}

func cachedExtractedImage(key imageCacheKey) ([]byte, bool) {
	extractedImageCache.Lock()
	defer extractedImageCache.Unlock()
	data, ok := extractedImageCache.entries[key]
	return data, ok
}

func storeExtractedImage(key imageCacheKey, data []byte) {
	extractedImageCache.Lock()
	defer extractedImageCache.Unlock()
	if _, ok := extractedImageCache.entries[key]; ok {
		return
	}
	if len(extractedImageCache.order) >= maxCachedImages {
		evict := extractedImageCache.order[0]
		copy(extractedImageCache.order, extractedImageCache.order[1:])
		extractedImageCache.order = extractedImageCache.order[:len(extractedImageCache.order)-1]
		delete(extractedImageCache.entries, evict)
	}
	extractedImageCache.entries[key] = data
	extractedImageCache.order = append(extractedImageCache.order, key)
}

func parseKernelHeader(header []byte) (KernelHeader, error) {
	if len(header) < imageHeaderSizeBytes {
		return KernelHeader{}, fmt.Errorf("arm64 kernel header truncated")
	}
	h := KernelHeader{
		Code0:      binary.LittleEndian.Uint32(header[0:4]),
		Code1:      binary.LittleEndian.Uint32(header[4:8]),
		TextOffset: binary.LittleEndian.Uint64(header[8:16]),
		ImageSize:  binary.LittleEndian.Uint64(header[16:24]),
		Flags:      binary.LittleEndian.Uint64(header[24:32]),
		Res2:       binary.LittleEndian.Uint64(header[32:40]),
		Res3:       binary.LittleEndian.Uint64(header[40:48]),
		Res4:       binary.LittleEndian.Uint64(header[48:56]),
		Magic:      binary.LittleEndian.Uint32(header[56:60]),
		Res5:       binary.LittleEndian.Uint32(header[60:64]),
	}
	if h.Magic != arm64ImageMagic {
		return KernelHeader{}, fmt.Errorf("invalid arm64 kernel magic %#x", h.Magic)
	}
	return h, nil
}

func readKernelHeaderAt(reader io.ReaderAt, offset int64) (KernelHeader, error) {
	buf := make([]byte, imageHeaderSizeBytes)
	if _, err := reader.ReadAt(buf, offset); err != nil {
		return KernelHeader{}, err
	}
	return parseKernelHeader(buf)
}

func findGzipPayload(reader io.ReaderAt, size int64) (int64, error) {
	scan := size
	if scan > maxGzipScanBytes {
		scan = maxGzipScanBytes
	}
	buf := make([]byte, scan)
	n, err := reader.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read kernel prefix: %w", err)
	}
	buf = buf[:n]
	idx := bytes.Index(buf, []byte{0x1f, 0x8b})
	if idx < 0 {
		return 0, fmt.Errorf("gzip header not found")
	}
	return int64(idx), nil
}

func readGzipKernelHeader(reader io.ReaderAt, offset, size int64) (KernelHeader, error) {
	section := io.NewSectionReader(reader, offset, size-offset)
	gz, err := gzip.NewReader(section)
	if err != nil {
		return KernelHeader{}, fmt.Errorf("open gzip reader: %w", err)
	}
	gz.Multistream(false)
	defer gz.Close()

	buf := make([]byte, imageHeaderSizeBytes)
	if _, err := io.ReadFull(gz, buf); err != nil {
		return KernelHeader{}, fmt.Errorf("read gzip header: %w", err)
	}
	return parseKernelHeader(buf)
}
