package vm

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestDarwinDisplayAccelerationIsExplicitlyRequested(t *testing.T) {
	shareGroupCalls := 0
	backend := runtimeBackend{openGLShareGroup: func() (uintptr, uintptr) {
		shareGroupCalls++
		return 11, 22
	}}

	var ordinary vmruntime.RunRequest
	backend.configureDisplayRequest(&ordinary, &client.DisplayConfig{Width: 1440, Height: 900})
	if ordinary.Accelerated3D || ordinary.OpenGLShareContext != 0 || ordinary.OpenGLSharePixelFormat != 0 {
		t.Fatalf("ordinary display requested acceleration: %+v", ordinary)
	}
	if shareGroupCalls != 0 {
		t.Fatalf("ordinary display requested the OpenGL share group %d times", shareGroupCalls)
	}

	var accelerated vmruntime.RunRequest
	backend.configureDisplayRequest(&accelerated, &client.DisplayConfig{
		Width: 1440, Height: 900, Accelerated3D: true,
	})
	if !accelerated.Accelerated3D || accelerated.OpenGLShareContext != 11 || accelerated.OpenGLSharePixelFormat != 22 {
		t.Fatalf("accelerated display request = %+v", accelerated)
	}
	if shareGroupCalls != 1 {
		t.Fatalf("accelerated display requested the OpenGL share group %d times, want once", shareGroupCalls)
	}
}
