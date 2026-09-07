package vmruntime

import (
	"encoding/binary"
	"fmt"
	"io"
)

func WriteDisplaySize(w io.Writer, width, height uint32) error {
	if width == 0 || height == 0 || width > 8192 || height > 8192 {
		return fmt.Errorf("invalid display dimensions %dx%d", width, height)
	}
	raw := make([]byte, 8)
	binary.BigEndian.PutUint32(raw[0:4], width)
	binary.BigEndian.PutUint32(raw[4:8], height)
	_, err := w.Write(raw)
	return err
}
