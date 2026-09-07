//go:build darwin

package virgl

import (
	"errors"
	"testing"
)

func newDarwinTestHost(t *testing.T) *darwinHost {
	t.Helper()
	host, err := newDarwinHost()
	if errors.Is(err, errDarwinAcceleratedOpenGLUnavailable) {
		t.Skip("accelerated OpenGL 4.1 is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	return host
}
