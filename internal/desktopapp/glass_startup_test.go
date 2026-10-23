package desktopapp

import (
	"context"
	"errors"
	"reflect"
	"testing"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestGlassServiceActivation(t *testing.T) {
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })
	for _, tc := range []struct {
		name, kind                    string
		exit                          int
		transport                     error
		sendExit, wantCall, wantError bool
	}{
		{name: "neurodesktop", kind: "ndappx", sendExit: true, wantCall: true},
		{name: "missing service", kind: "ndappx", exit: 5, sendExit: true, wantCall: true, wantError: true},
		{name: "cancellation", kind: "ndappx", transport: context.Canceled, wantCall: true, wantError: true},
		{name: "missing exit", kind: "ndappx", wantCall: true, wantError: true},
		{name: "squad unchanged", kind: "squadvm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appConfig = Config{Kind: tc.kind}
			called := false
			api := &testDesktopRuntime{run: func(_ context.Context, id string, req client.RunRequest, event func(client.ExecEvent) error) error {
				called = true
				if id != "isolated-test" || req.User != "root" || req.TimeoutSeconds <= 0 || !reflect.DeepEqual(req.Command, []string{"systemctl", "start", "neurodesktop-glass.service"}) {
					t.Fatalf("unexpected service request: %s %+v", id, req)
				}
				if tc.transport != nil {
					return tc.transport
				}
				if tc.sendExit {
					return event(client.ExecEvent{Kind: "exit", ExitCode: tc.exit})
				}
				return nil
			}}
			err := startDesktopService(t.Context(), api, "isolated-test")
			if called != tc.wantCall || (err != nil) != tc.wantError {
				t.Fatalf("called=%v err=%v", called, err)
			}
			if tc.transport != nil && !errors.Is(err, tc.transport) {
				t.Fatalf("lost transport error: %v", err)
			}
		})
	}
}

func TestHeadlessGlassRuntimeValidation(t *testing.T) {
	config, err := normalizeConfig(Config{ProductName: "test", Kind: "ndappx", DefaultMemoryMB: 256, DefaultCPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		env    map[string]string
		reject bool
	}{
		{name: "omitted"},
		{name: "explicit", env: map[string]string{"NEURODESKTOP_RUNTIME": "glass", "CUSTOM": "retained"}},
		{name: "container conflict", env: map[string]string{"NEURODESKTOP_RUNTIME": "container"}, reject: true},
		{name: "empty conflict", env: map[string]string{"NEURODESKTOP_RUNTIME": ""}, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := headlessDefaults(config, t.TempDir())
			req.ImageID = "img_test"
			req.Env = tc.env
			err := req.validate(config)
			if (err != nil) != tc.reject {
				t.Fatalf("validate: %v", err)
			}
			if !tc.reject {
				if req.Env["NEURODESKTOP_RUNTIME"] != "glass" {
					t.Fatalf("runtime not selected: %v", req.Env)
				}
				if tc.name == "explicit" && req.Env["CUSTOM"] != "retained" {
					t.Fatal("lost caller environment")
				}
			}
		})
	}
}
