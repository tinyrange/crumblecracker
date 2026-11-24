package desktopapp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

// Glass uses framebuffer readback when the renderer owns an offscreen context.
func headlessGPUSupported() bool { return platformExperimentalGPUAccelerationAvailable() }

func (d *headlessRuntimeDriver) Jupyter(id string) *headlessJupyterResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	value, ok := d.jupyter[id]
	if !ok {
		return nil
	}
	return &value
}

func (d *headlessRuntimeDriver) startJupyter(ctx context.Context, id string, port int) error {
	api := d.machine(id)
	response, err := api.RunInContext(ctx, id, client.RunRequest{User: "root", Command: []string{"systemctl", "start", "neurodesktop-jupyter.service"}, TimeoutSeconds: 300})
	if err != nil {
		return err
	}
	if response.ExitCode != 0 {
		return headlessFailure("jupyter_start_failed", "Jupyter service failed; the image must support Jupyter-first preparation")
	}
	response, err = api.RunInContext(ctx, id, client.RunRequest{User: "jovyan", Command: []string{"cat", "/run/user/1000/neurodesktop/jupyter.token"}, TimeoutSeconds: 10})
	if err != nil {
		return err
	}
	token := strings.TrimSpace(response.Output)
	if response.ExitCode != 0 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		return headlessFailure("jupyter_start_failed", "Jupyter did not create a valid private token")
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	probe := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		request, err := http.NewRequestWithContext(ctx, "GET", base+"/api/status", nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "token "+token)
		res, err := probe.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	d.mu.Lock()
	if d.jupyter == nil {
		d.jupyter = make(map[string]headlessJupyterResult)
	}
	d.jupyter[id] = headlessJupyterResult{URL: base + "/lab", Token: token}
	d.mu.Unlock()
	return nil
}
