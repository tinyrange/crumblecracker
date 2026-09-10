package desktopapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

// Opt-in and requires an explicitly isolated cache containing a prepared image
// and kernel. Never discover or operate on a user's live runtime/cache here.
func TestHeadlessGuestIntegration(t *testing.T) {
	cache := os.Getenv("NDAPPX_HEADLESS_TEST_CACHE")
	image := os.Getenv("NDAPPX_HEADLESS_TEST_IMAGE")
	if testing.Short() || cache == "" || image == "" {
		t.Skip("set isolated NDAPPX_HEADLESS_TEST_CACHE and NDAPPX_HEADLESS_TEST_IMAGE")
	}
	config, err := normalizeConfig(Config{ProductName: "Headless integration", Kind: "ndappx", DefaultVMName: "headless-integration", DefaultImage: image, DefaultMemoryMB: 1024, DefaultCPUs: 1, DefaultUser: "jovyan", GuestStorageMount: "/vmsh-neurodesktop-storage", PersistentHomeOwner: &GuestOwner{1000, 100}})
	if err != nil {
		t.Fatal(err)
	}
	config.AMD64Emulation = true
	desktop := os.Getenv("NDAPPX_HEADLESS_TEST_DESKTOP") == "1"
	if desktop {
		config.CVMFSHostMount = &CVMFSHostMountConfig{Mount: "/cvmfs/neurodesk.ardc.edu.au", Mirror: "http://cvmfs.neurodesk.org", Mirrors: []string{"http://cvmfs.neurodesk.org"}, Repo: "neurodesk.ardc.edu.au", Path: "/", CacheLimitBytes: 1 << 30}
	}
	previous := appConfig
	appConfig = config
	t.Cleanup(func() { appConfig = previous })
	driver, err := newHeadlessRuntimeDriver(cache, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	state, err := driver.pullAPI.ImageState(image)
	if err != nil {
		t.Fatal(err)
	}
	kernel := driver.pullAPI.KernelStatus()
	driver.prepared["img_test"] = headlessPrepared{image: headlessImage{ID: "img_test", Kernel: headlessKernel{Version: kernel.Version}}, name: state.Name}
	storage := t.TempDir()
	additional := t.TempDir()
	req := headlessDefaults(config, storage)
	req.ImageID = "img_test"
	req.Env["HEADLESS_TEST_VALUE"] = "space ' \" $PATH\nsecond line"
	req.Shares = []headlessShare{{HostPath: additional, GuestPath: "/data"}}
	req.Home.ID = "headless-integration"
	ephemeral := os.Getenv("NDAPPX_HEADLESS_TEST_EPHEMERAL") == "1"
	if ephemeral {
		req.Home = headlessHome{Mode: "ephemeral"}
	}
	if err := req.validate(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := driver.Shutdown(cleanup); err != nil {
			t.Error(err)
		}
	})
	for pass := 0; pass < 2; pass++ {
		if err := driver.Start(ctx, "vm_integration", req); err != nil {
			t.Fatal(err)
		}
		api := driver.machine("vm_integration")
		run := func(command ...string) string {
			t.Helper()
			response, err := api.RunInContext(ctx, "vm_integration", client.RunRequest{Command: command, User: "root", TimeoutSeconds: 15})
			if err != nil {
				t.Fatal(err)
			}
			if response.ExitCode != 0 {
				t.Fatalf("guest command failed: %d %s", response.ExitCode, response.Output)
			}
			return response.Output
		}
		run("/bin/sh", "-c", "test ! -S /tmp/.X11-unix/X0; ! systemctl is-active --quiet neurodesktop-glass.service; test -f /run/systemd/system/ccx3-headless.target")
		value := run("/usr/bin/printenv", "HEADLESS_TEST_VALUE")
		if strings.TrimSuffix(value, "\n") != req.Env["HEADLESS_TEST_VALUE"] {
			t.Fatalf("guest env bytes changed: %q", value)
		}
		if pass == 0 {
			run("/bin/sh", "-c", "printf 'shared-data' > /data/headless-check; printf 'persistent-home' > /home/jovyan/headless-check")
		} else if ephemeral {
			run("/bin/sh", "-c", "test ! -e /home/jovyan/headless-check")
		} else {
			if got := run("cat", "/home/jovyan/headless-check"); got != "persistent-home" {
				t.Fatalf("home lost: %q", got)
			}
		}
		if data, err := os.ReadFile(filepath.Join(additional, "headless-check")); err != nil || string(data) != "shared-data" {
			t.Fatalf("additional directory write-through: %q %v", data, err)
		}
		if desktop {
			run("systemctl", "start", "neurodesktop-glass.service")
			if err := waitForDesktop(ctx, api, "vm_integration"); err != nil {
				t.Fatal(err)
			}
			run("python3", "-c", "import pathlib,subprocess,sys; pid=subprocess.check_output(['systemctl','show','--property=MainPID','--value','neurodesktop-glass.service']).strip().decode(); env=pathlib.Path('/proc/'+pid+'/environ').read_bytes().split(bytes([0])); assert sys.argv[1].encode() in env", "HEADLESS_TEST_VALUE="+req.Env["HEADLESS_TEST_VALUE"])
			t.Log("guest desktop ready; startup environment preserved")
		}
		stop, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = driver.Stop(stop, "vm_integration")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
}
