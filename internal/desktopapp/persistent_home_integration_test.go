package desktopapp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
	appruntime "github.com/tinyrange/crumblecracker/internal/runtime"
)

// This boots real VMs with an isolated cache. On macOS the test binary must be
// signed with tools/entitlements.xml. Keep it out of ordinary commit CI.
func TestNeurodeskHomeUpgradeBoot(t *testing.T) {
	if testing.Short() || os.Getenv("CRUMBLECRACKER_UPGRADE_SMOKE") != "1" {
		t.Skip("set CRUMBLECRACKER_UPGRADE_SMOKE=1 for the manual VM upgrade check")
	}
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })
	appConfig = Config{DefaultVMName: "neurodesk", LegacyDefaultHome: "ndappx"}
	cache := t.TempDir()
	api, err := appruntime.New(appruntime.Options{CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := api.ShutdownContext(ctx); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fixture := "alpine.simg"
	if runtime.GOARCH == "arm64" {
		fixture = "alpine-arm64.simg"
	}
	source, err := filepath.Abs(filepath.Join("..", "..", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := api.PullImageStreamContext(ctx, "upgrade", client.PullImageRequest{Source: source}, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ndappx", "neurodesk"} {
		mounts, _, err := configuredPersistentHomeMount(cache, name, "", false)
		if err != nil {
			t.Fatal(err)
		}
		mounts[0].Mount = "/root"
		request := client.CreateInstanceRequest{Image: "upgrade", MemoryMB: 1024, CPUs: 1, PersistentMounts: mounts}
		if _, err := api.CreateInstanceStreamWithIDContext(ctx, name, request, nil); err != nil {
			t.Fatal(err)
		}
		script := "printf 'saved before upgrade' > /root/upgrade-data"
		if name == "neurodesk" {
			script = "cat /root/upgrade-data"
		}
		result, err := api.RunInContext(ctx, name, client.RunRequest{Command: []string{"/bin/sh", "-c", script}, TimeoutSeconds: 10})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("guest exit %d: %s", result.ExitCode, result.Output)
		}
		if name == "neurodesk" && result.Output != "saved before upgrade" {
			t.Fatalf("home data after upgrade = %q", result.Output)
		}
		if err := api.ShutdownInstanceWithIDContext(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
}
