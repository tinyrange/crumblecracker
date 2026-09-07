package vmruntime

import (
	"encoding/binary"
	"fmt"
	"io"
)

const ClipboardMaxBytes = 64 << 20

func WriteClipboardText(w io.Writer, text string) error {
	if len(text) > ClipboardMaxBytes {
		return fmt.Errorf("clipboard text is too large: %d bytes", len(text))
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(text)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := io.WriteString(w, text)
	return err
}

func ReadClipboardText(r io.Reader) (string, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return "", err
	}
	length := binary.BigEndian.Uint32(header)
	if length > ClipboardMaxBytes {
		return "", fmt.Errorf("clipboard text is too large: %d bytes", length)
	}
	text := make([]byte, int(length))
	if _, err := io.ReadFull(r, text); err != nil {
		return "", err
	}
	return string(text), nil
}
