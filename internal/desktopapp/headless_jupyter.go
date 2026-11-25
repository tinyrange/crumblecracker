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
	response, err := api.RunInContext(ctx, id, client.RunRequest{User: "root", Command: []string{"/bin/sh", "-eu", "-c", startNeurodesktopJupyter}, TimeoutSeconds: 300})
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

// The published Glass image starts this unit from its desktop launcher.
// Supply the private state that the launcher normally prepares when running
// Jupyter before Glass.
const startNeurodesktopJupyter = `
if [ "$(systemctl show --property=LoadState --value neurodesktop-prepare.service)" = not-found ] &&
   [ "$(systemctl show --property=LoadState --value neurodesktop-jupyter.service)" = loaded ]; then
	group=$(id -g jovyan)
	runtime=/run/user/1000/neurodesktop
	state=/home/jovyan/.local/state/Neurodesktop
	install -d -m 0700 -o jovyan -g "$group" /run/user/1000 "$runtime" "$runtime/jupyter"
	for dir in "$state" /home/jovyan/.local/share/jupyter /home/jovyan/.local/share/code-server/extensions; do
		if [ ! -d "$dir" ]; then
			runuser -u jovyan -- mkdir -m 0700 -p "$dir"
		fi
	done
	if [ ! -e "$state/jupyter.log" ]; then
		install -m 0600 -o jovyan -g "$group" /dev/null "$state/jupyter.log"
	fi
	install -d -m 0700 /run/ccx3-jupyter
	dropin=/run/systemd/system/neurodesktop-jupyter.service.d
	install -d -m 0755 "$dropin"
	umask 077
	token=$(od -An -v -tx1 -N32 /dev/urandom | tr -d ' \n')
	test "${#token}" -eq 64
	printf '%s\n' "$token" > /run/ccx3-jupyter/token
	install -m 0600 -o jovyan -g "$group" /run/ccx3-jupyter/token "$runtime/jupyter.token"
	printf 'JUPYTER_TOKEN=%s\nJUPYTER_RUNTIME_DIR=%s/jupyter\n' "$token" "$runtime" > /run/ccx3-jupyter/environment
	printf '[Service]\nEnvironmentFile=/run/ccx3-jupyter/environment\n' > "$dropin/95-ccx3-auth.conf"
	systemctl daemon-reload
fi
systemctl start neurodesktop-jupyter.service
`
