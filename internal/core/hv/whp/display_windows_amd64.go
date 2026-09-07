//go:build windows && amd64

package whp

import (
	"context"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

func serveDisplayConnections(ctx context.Context, listener virtio.VsockListener, desktop *virtio.Desktop) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = serveDisplayConnection(ctx, conn, desktop)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
	}
}

func serveDisplayConnection(ctx context.Context, conn virtio.VsockConn, desktop *virtio.Desktop) error {
	width, height := desktop.Framebuffer.Size()
	if err := vmruntime.WriteDisplaySize(conn, uint32(width), uint32(height)); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case size := <-desktop.ResizeRequests():
			if err := vmruntime.WriteDisplaySize(conn, size.Width, size.Height); err != nil {
				return err
			}
		}
	}
}
