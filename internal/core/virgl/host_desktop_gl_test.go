//go:build (linux || windows) && amd64

package virgl

import (
	"os"
	"testing"
)

func newReplayTestHost(t *testing.T) *openGLHost {
	t.Helper()
	if os.Getenv("VMSH_VIRGL_TEST") != "1" {
		t.Skip("set VMSH_VIRGL_TEST=1 to run the native OpenGL test")
	}
	host, err := newReplayHost()
	if err != nil {
		t.Fatal(err)
	}
	return host
}
