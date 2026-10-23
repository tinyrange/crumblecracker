package desktopapp

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"strings"
	"time"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

type headlessDesktopInspector interface {
	Screenshot(context.Context, string) ([]byte, error)
	Diagnostics(context.Context, string) (map[string]any, error)
}

func (s *headlessServer) desktopRead(w http.ResponseWriter, r *http.Request, route string) {
	id := strings.Split(r.URL.Path, "/")[3]
	s.mu.Lock()
	vm := s.vms[id]
	ready := vm != nil && vm.State == "running" && !s.closing
	presented := ready && vm.Glass != nil && vm.Glass.State == "open"
	s.mu.Unlock()
	if !ready || (route == "screenshot" && !presented) {
		headlessHTTPError(w, 409, "invalid_state", "VM or presented Glass window is unavailable")
		return
	}
	inspector, ok := s.driver.(headlessDesktopInspector)
	if !ok {
		headlessHTTPError(w, 501, "unsupported_configuration", "Desktop inspection unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if route == "screenshot" {
		data, err := inspector.Screenshot(ctx, id)
		if err != nil {
			headlessHTTPError(w, 409, "capture_failed", "Desktop capture failed")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	} else {
		report, err := inspector.Diagnostics(ctx, id)
		if err != nil {
			headlessHTTPError(w, 409, "inspection_failed", "Guest inspection failed")
			return
		}
		headlessJSON(w, 200, report)
	}
}

func (d *headlessRuntimeDriver) Screenshot(ctx context.Context, id string) ([]byte, error) {
	api := d.machine(id)
	if api == nil {
		return nil, fmt.Errorf("VM unavailable")
	}
	session, ok := api.DisplaySession(id)
	if !ok {
		return nil, fmt.Errorf("display unavailable")
	}
	width, height := session.Size()
	frame := session.Snapshot(image.Rect(0, 0, width, height), 0, false)
	if frame.Width < 1 || frame.Height < 1 || len(frame.Pixels) != frame.Width*frame.Height*4 {
		return nil, fmt.Errorf("incomplete frame")
	}
	pixels := image.NewRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	for i := 0; i < len(frame.Pixels); i += 4 {
		pixels.Pix[i], pixels.Pix[i+1], pixels.Pix[i+2], pixels.Pix[i+3] = frame.Pixels[i+2], frame.Pixels[i+1], frame.Pixels[i], 255
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, pixels); err != nil {
		return nil, err
	}
	return buffer.Bytes(), ctx.Err()
}

func (d *headlessRuntimeDriver) Diagnostics(ctx context.Context, id string) (map[string]any, error) {
	api := d.machine(id)
	if api == nil {
		return nil, fmt.Errorf("VM unavailable")
	}
	services, err := api.RunInContext(ctx, id, client.RunRequest{User: "root", TimeoutSeconds: 5, Command: []string{"sh", "-c", "systemctl is-active neurodesktop-jupyter.service; systemctl is-active neurodesktop-glass.service; test -S /tmp/.X11-unix/X0 && echo x11-ready; true"}})
	if err != nil {
		return nil, err
	}
	graphics, err := api.RunInContext(ctx, id, client.RunRequest{User: "jovyan", TimeoutSeconds: 10, Command: []string{"env", "DISPLAY=:0", "XAUTHORITY=/run/user/1000/neurodesktop/Xauthority", "glxinfo", "-B"}})
	if err != nil {
		return nil, err
	}
	return map[string]any{"services": services.Output, "graphics": graphics.Output, "graphics_exit_code": graphics.ExitCode}, nil
}
