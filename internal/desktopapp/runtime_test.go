package desktopapp

import (
	"context"
	"errors"
	"github.com/tinyrange/crumblecracker/internal/protocol"
	"testing"
)

type testDesktopRuntime struct {
	desktopRuntime
	run func(context.Context, string, client.RunRequest, func(client.ExecEvent) error) error
}

func (r *testDesktopRuntime) RunStreamInContext(ctx context.Context, id string, req client.RunRequest, event func(client.ExecEvent) error) error {
	if r.run != nil {
		return r.run(ctx, id, req, event)
	}
	return event(client.ExecEvent{Kind: "exit"})
}
func TestGuestSetupRoutesToSelectedVMAsRoot(t *testing.T) {
	called := false
	api := &testDesktopRuntime{run: func(ctx context.Context, id string, req client.RunRequest, event func(client.ExecEvent) error) error {
		called = true
		if id != "ndappx" || req.User != "root" {
			t.Fatalf("target=%q user=%q", id, req.User)
		}
		return event(client.ExecEvent{Kind: "exit"})
	}}
	if err := runGuestRootScript(t.Context(), api, "ndappx", "true"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("guest setup was not executed")
	}
}
func TestGuestSetupPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	api := &testDesktopRuntime{run: func(ctx context.Context, _ string, _ client.RunRequest, _ func(client.ExecEvent) error) error {
		return ctx.Err()
	}}
	if err := runGuestRootScript(ctx, api, "ndappx", "true"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
func TestGuestSetupRejectsNonzeroExit(t *testing.T) {
	api := &testDesktopRuntime{run: func(_ context.Context, _ string, _ client.RunRequest, event func(client.ExecEvent) error) error {
		return event(client.ExecEvent{Kind: "exit", ExitCode: 7})
	}}
	if err := runGuestRootScript(t.Context(), api, "ndappx", "false"); err == nil {
		t.Fatal("failed guest setup reported success")
	}
}
