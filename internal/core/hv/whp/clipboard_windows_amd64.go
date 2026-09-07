//go:build windows && amd64

package whp

import (
	"context"
	"errors"
	"io"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

func serveClipboardConnections(ctx context.Context, listener virtio.VsockListener, clipboard *virtio.Clipboard) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = serveClipboardConnection(ctx, conn, clipboard)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
	}
}

func serveClipboardConnection(ctx context.Context, conn virtio.VsockConn, clipboard *virtio.Clipboard) error {
	if text, generation := clipboard.FrontendSnapshot(); generation != 0 {
		if err := vmruntime.WriteClipboardText(conn, text); err != nil {
			return err
		}
	}
	readErr := make(chan error, 1)
	go func() {
		for {
			text, err := vmruntime.ReadClipboardText(conn)
			if err != nil {
				readErr <- err
				return
			}
			clipboard.SetFromGuest(text)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case text := <-clipboard.ToGuest():
			if err := vmruntime.WriteClipboardText(conn, text); err != nil {
				return err
			}
		}
	}
}
