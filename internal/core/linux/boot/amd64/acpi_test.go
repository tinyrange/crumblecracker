package amd64

import (
	"bytes"
	"testing"
)

func TestBootDSDTAdvertisesS5Poweroff(t *testing.T) {
	want := []byte{0x08, '_', 'S', '5', '_', 0x12, 0x08, 0x04, 0x0a, 0x05, 0x0a, 0x05, 0x00, 0x00}
	if got := buildBootDSDT(); !bytes.Equal(got, want) {
		t.Fatalf("DSDT = %x, want %x", got, want)
	}
}
