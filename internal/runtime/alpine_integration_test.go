package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

// Opt-in: downloads an Alpine image and kernel into a private temporary cache.
// No host directories are shared with the guest.
func TestAlpineGuestIntegration(t *testing.T) {
	if testing.Short() || os.Getenv("CRUMBLECRACKER_ALPINE_SMOKE") != "1" {
		t.Skip("set CRUMBLECRACKER_ALPINE_SMOKE=1 to download and boot an isolated Alpine VM")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, err := New(Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.ShutdownContext(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	t.Log("stage: virtualization support")
	support, err := r.VMSupportedContext(ctx)
	if err != nil || !support.Supported {
		t.Fatalf("virtualization unavailable: %+v; %v", support, err)
	}
	report := func(e client.ProgressEvent) {
		if e.Error != "" {
			t.Logf("progress error: %s", e.Error)
		}
	}
	t.Log("stage: pull Alpine image")
	if err := r.PullImageStreamContext(ctx, "alpine-smoke", client.PullImageRequest{Source: "alpine:3.23", Prefetch: true}, func(e client.ProgressEvent) error { report(e); return nil }); err != nil {
		t.Fatalf("image pull: %v", err)
	}
	t.Log("stage: prepare kernel")
	if _, err := r.PrepareKernel(ctx, report); err != nil {
		t.Fatalf("kernel: %v", err)
	}
	t.Log("stage: boot")
	state, err := r.CreateInstanceStreamWithIDContext(ctx, "alpine-smoke", client.CreateInstanceRequest{Image: "alpine-smoke", DefaultUser: "root", MemoryMB: 512, CPUs: 1, TimeoutSeconds: 45}, func(e client.BootEvent) error {
		if e.Kind != "serial" {
			t.Logf("boot: %s %s %s", e.Kind, e.Message, e.Error)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Logf("stage: guest execution (state=%s)", state.Status)
	run := func(command string) client.ExecResponse {
		t.Helper()
		response, err := r.RunInContext(ctx, state.ID, client.RunRequest{Command: []string{"/bin/sh", "-c", command}, User: "root", TimeoutSeconds: 10})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		return response
	}
	first := run("test $(id -u) = 0 && test $(uname -s) = Linux && printf vm-smoke > /tmp/vm-smoke && cat /tmp/vm-smoke")
	if first.ExitCode != 0 || first.Output != "vm-smoke" {
		t.Fatalf("first exec: %+v", first)
	}
	second := run("cat /tmp/vm-smoke; exit 7")
	if second.ExitCode != 7 || second.Output != "vm-smoke" {
		t.Fatalf("second exec: %+v", second)
	}
	t.Log("guest Linux/root verified; filesystem state survives separate execs; exit code 7 preserved")
}
