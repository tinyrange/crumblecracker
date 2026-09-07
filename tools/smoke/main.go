// Smoke exercises the in-process runtime with isolated, explicitly supplied paths.
// It is a manual release check, not a commit CI job.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
	appruntime "github.com/tinyrange/crumblecracker/internal/runtime"
)

func main() {
	cache := flag.String("cache-dir", "", "isolated development cache directory (required)")
	storage := flag.String("storage", "", "isolated shared directory (required)")
	source := flag.String("image", "", "Linux image source containing /bin/sh (required)")
	flag.Parse()
	if err := check(*cache, *storage, *source); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func check(cache, storage, source string) (retErr error) {
	if cache == "" || storage == "" || source == "" {
		return errors.New("provide -cache-dir, -storage, and -image")
	}
	storage, err := filepath.Abs(storage)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(storage, 0755); err != nil {
		return err
	}
	api, err := appruntime.New(appruntime.Options{CacheDir: cache})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, api.ShutdownContext(ctx))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := api.PullImageStreamContext(ctx, "smoke", client.PullImageRequest{Source: source}, nil); err != nil {
		return err
	}
	request := client.CreateInstanceRequest{Image: "smoke", MemoryMB: 1024, CPUs: 1, Network: &client.NetworkConfig{Enabled: true, AllowInternet: true}, Shares: []client.ShareMount{{Source: storage, Mount: "/shared", Writable: true}}}
	for pass := 0; pass < 2; pass++ {
		state, err := api.CreateInstanceStreamWithIDContext(ctx, "product-smoke", request, nil)
		if err != nil {
			return err
		}
		if state.Status != "running" {
			return fmt.Errorf("boot status: %s", state.Status)
		}
		script := "printf 'persisted\\n' > /shared/product-smoke.txt"
		if pass == 1 {
			script = "cat /shared/product-smoke.txt"
		}
		response, err := api.RunInContext(ctx, "product-smoke", client.RunRequest{Command: []string{"/bin/sh", "-c", script}, TimeoutSeconds: 10})
		if err != nil {
			return err
		}
		if response.ExitCode != 0 {
			return fmt.Errorf("guest command exited %d: %s", response.ExitCode, response.Output)
		}
		if pass == 1 && response.Output != "persisted" {
			return fmt.Errorf("restart lost shared contents: %q", response.Output)
		}
		if pass == 0 {
			data, err := os.ReadFile(filepath.Join(storage, "product-smoke.txt"))
			if err != nil {
				return err
			}
			if string(data) != "persisted\n" {
				return fmt.Errorf("host readback: %q", data)
			}
			deadlineErr := api.RunStreamInContext(ctx, "product-smoke", client.RunRequest{Command: []string{"/bin/sh", "-c", "sleep 30"}, TimeoutSeconds: 0.1}, func(client.ExecEvent) error { return nil })
			if !errors.Is(deadlineErr, context.DeadlineExceeded) {
				return fmt.Errorf("guest command deadline: %v", deadlineErr)
			}
			response, err = api.RunInContext(ctx, "product-smoke", client.RunRequest{Command: []string{"/bin/sh", "-c", "exit 7"}, TimeoutSeconds: 10})
			if err != nil {
				return err
			}
			if response.ExitCode != 7 {
				return fmt.Errorf("guest exit status: %d", response.ExitCode)
			}
		}
		if err := api.ShutdownInstanceWithIDContext(ctx, "product-smoke"); err != nil {
			return err
		}
	}
	fmt.Println("PASS: boot, repeated guest commands, deadline, exit status, shared writes, shutdown, and restart")
	return nil
}
