package guestinit

import (
	"bytes"
	"context"
	"testing"
)

func TestBuildForArchReturnsEmbeddedPayloadCopy(t *testing.T) {
	payload := embeddedPayload("arm64")
	got, err := BuildForArch(context.Background(), t.TempDir(), "arm64")
	if err != nil {
		t.Fatalf("load embedded guest init: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	got[0] = 0
	if payload[0] != 0x7f {
		t.Fatal("returned payload aliases embedded storage")
	}
}
