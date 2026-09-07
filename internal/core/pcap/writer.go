package pcap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Common link-layer (DLT) identifiers used in pcap global headers.
// The values match the tcpdump/libpcap definitions.
const (
	LinkTypeEthernet uint32 = 1
)

var (
	// ErrHeaderAlreadyWritten indicates the global header has already been
	// emitted for this writer instance.
	ErrHeaderAlreadyWritten = errors.New("pcap: file header already written")
	// ErrHeaderNotWritten indicates a packet was written before the global header.
	ErrHeaderNotWritten = errors.New("pcap: file header not written")
)

// CaptureInfo describes metadata associated with a captured packet.
// Timestamp uses microsecond resolution when serialized into the pcap record.
type CaptureInfo struct {
	Timestamp     time.Time
	CaptureLength int
	Length        int
}

// Writer emits classic libpcap-formatted streams.
type Writer struct {
	w             io.Writer
	headerWritten bool
	snapLen       uint32
}

// NewWriter wraps the supplied io.Writer. The caller must invoke WriteFileHeader
// once before any packets are written.
func NewWriter(out io.Writer) *Writer {
	return &Writer{w: out}
}

// WriteFileHeader writes the 24-byte global pcap header. It must be called
// exactly once per Writer instance before WritePacket is used.
func (w *Writer) WriteFileHeader(snapLen uint32, linkType uint32) error {
	if w.headerWritten {
		return ErrHeaderAlreadyWritten
	}

	var hdr [24]byte
	binary.LittleEndian.PutUint32(hdr[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(hdr[4:6], 2) // Major version
	binary.LittleEndian.PutUint16(hdr[6:8], 4) // Minor version
	binary.LittleEndian.PutUint32(hdr[8:12], 0)
	binary.LittleEndian.PutUint32(hdr[12:16], 0)
	binary.LittleEndian.PutUint32(hdr[16:20], snapLen)
	binary.LittleEndian.PutUint32(hdr[20:24], linkType)

	if _, err := w.w.Write(hdr[:]); err != nil {
		return fmt.Errorf("pcap: write header: %w", err)
	}

	w.snapLen = snapLen
	w.headerWritten = true
	return nil
}

// WritePacket appends a captured packet record to the stream.
func (w *Writer) WritePacket(ci CaptureInfo, data []byte) error {
	if !w.headerWritten {
		return ErrHeaderNotWritten
	}

	if ci.CaptureLength < 0 {
		return fmt.Errorf("pcap: negative capture length %d", ci.CaptureLength)
	}
	if ci.Length < 0 {
		return fmt.Errorf("pcap: negative original length %d", ci.Length)
	}
	if ci.CaptureLength > len(data) {
		return fmt.Errorf("pcap: capture length %d exceeds data buffer %d", ci.CaptureLength, len(data))
	}
	if ci.CaptureLength > math.MaxUint32 {
		return fmt.Errorf("pcap: capture length %d overflows uint32", ci.CaptureLength)
	}
	if ci.Length > math.MaxUint32 {
		return fmt.Errorf("pcap: original length %d overflows uint32", ci.Length)
	}
	if w.snapLen != 0 && uint32(ci.CaptureLength) > w.snapLen {
		return fmt.Errorf("pcap: capture length %d exceeds snap length %d", ci.CaptureLength, w.snapLen)
	}

	var tsSec uint32
	var tsUsec uint32
	if !ci.Timestamp.IsZero() {
		sec := ci.Timestamp.Unix()
		if sec < 0 || sec > math.MaxUint32 {
			return fmt.Errorf("pcap: timestamp seconds %d out of range", sec)
		}
		tsSec = uint32(sec)
		tsUsec = uint32(ci.Timestamp.Nanosecond() / 1_000)
	}

	var rec [16]byte
	binary.LittleEndian.PutUint32(rec[0:4], tsSec)
	binary.LittleEndian.PutUint32(rec[4:8], tsUsec)
	binary.LittleEndian.PutUint32(rec[8:12], uint32(ci.CaptureLength))
	binary.LittleEndian.PutUint32(rec[12:16], uint32(ci.Length))

	if _, err := w.w.Write(rec[:]); err != nil {
		return fmt.Errorf("pcap: write record header: %w", err)
	}
	if ci.CaptureLength == 0 {
		return nil
	}
	if _, err := w.w.Write(data[:ci.CaptureLength]); err != nil {
		return fmt.Errorf("pcap: write packet data: %w", err)
	}
	return nil
}
