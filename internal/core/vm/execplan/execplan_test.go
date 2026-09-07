package execplan

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestResolveExecRequest(t *testing.T) {
	got, err := ResolveExecRequest(client.ExecRequest{
		ID:      "exec-7",
		Command: []string{"tool", "arg"},
		Env:     []string{"EXTRA=1"},
		Stdin:   []byte("input"),
	}, Resolver{
		Root:           testResolverRoot(t),
		BaseEnv:        []string{"PATH=/bin"},
		DefaultWorkDir: "/work",
		DefaultRootDir: "/rootfs",
		Env: func(base, overrides []string, replace bool) []string {
			return append(append([]string(nil), base...), overrides...)
		},
		User: func(user string) (string, error) {
			if user == "" {
				return "1000:1000", nil
			}
			return user, nil
		},
	})
	if err != nil {
		t.Fatalf("ResolveExecRequest: %v", err)
	}
	if strings.Join(got.Command, " ") != "/bin/tool arg" {
		t.Fatalf("command = %#v", got.Command)
	}
	if got.ID != "exec-7" {
		t.Fatalf("id = %q", got.ID)
	}
	if got.WorkDir != "/work" {
		t.Fatalf("workdir = %q", got.WorkDir)
	}
	if got.RootDir != "/rootfs" {
		t.Fatalf("rootdir = %q", got.RootDir)
	}
	if got.User != "1000:1000" {
		t.Fatalf("user = %q", got.User)
	}
	if string(got.Stdin) != "input" {
		t.Fatalf("stdin = %q", got.Stdin)
	}
}

func TestResolveExecRequestValidation(t *testing.T) {
	_, err := ResolveExecRequest(client.ExecRequest{Command: []string{"tool"}}, Resolver{
		MissingRootErr: "missing root",
	})
	if err == nil || err.Error() != "missing root" {
		t.Fatalf("missing root error = %v", err)
	}
	_, err = ResolveExecRequest(client.ExecRequest{
		Command:     []string{"tool"},
		SkipResolve: true,
		WorkDir:     "relative",
	}, Resolver{})
	if err == nil {
		t.Fatalf("relative workdir error = %v", err)
	}
}

func TestResolveExecRequestCanMarkHostResolved(t *testing.T) {
	got, err := ResolveExecRequest(client.ExecRequest{
		Command: []string{"tool"},
	}, Resolver{
		Root:           testResolverRoot(t),
		BaseEnv:        []string{"PATH=/bin"},
		DefaultWorkDir: "/",
		MarkResolved:   true,
	})
	if err != nil {
		t.Fatalf("ResolveExecRequest: %v", err)
	}
	if strings.Join(got.Command, " ") != "/bin/tool" {
		t.Fatalf("command = %#v", got.Command)
	}
	if !got.SkipResolve {
		t.Fatalf("SkipResolve = false")
	}
}

func TestResolveRunRequestMarksResolvedAlternateRoot(t *testing.T) {
	got, err := ResolveRunRequest(client.RunRequest{
		ID:      "exec-9",
		Command: []string{"tool"},
		Env:     []string{"EXTRA=1"},
		Stdin:   []byte("input"),
	}, "/run/image", Resolver{
		Root:           testResolverRoot(t),
		BaseEnv:        []string{"PATH=/bin", "BASE=1"},
		DefaultWorkDir: "/workspace",
		Env: func(base, overrides []string, _ bool) []string {
			return vmruntime.WithDefaultEnv(vmruntime.MergeEnv(base, overrides))
		},
	})
	if err != nil {
		t.Fatalf("ResolveRunRequest: %v", err)
	}
	if strings.Join(got.Command, " ") != "/bin/tool" {
		t.Fatalf("command = %#v", got.Command)
	}
	if got.ID != "exec-9" {
		t.Fatalf("id = %q", got.ID)
	}
	if got.RootDir != "/run/image" {
		t.Fatalf("rootdir = %q", got.RootDir)
	}
	if got.WorkDir != "/workspace" {
		t.Fatalf("workdir = %q", got.WorkDir)
	}
	if !got.ReplaceEnv || !got.SkipResolve {
		t.Fatalf("ReplaceEnv/SkipResolve = %v/%v", got.ReplaceEnv, got.SkipResolve)
	}
	if string(got.Stdin) != "input" {
		t.Fatalf("stdin = %q", got.Stdin)
	}
}

func testResolverRoot(t *testing.T) imagefs.Directory {
	t.Helper()
	overlay := imagefs.NewOverlay(nil)
	if err := overlay.AddDir("/bin", fs.ModeDir|0o755); err != nil {
		t.Fatal(err)
	}
	if err := overlay.AddFile("/bin/tool", 0o755, []byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	return overlay.Root()
}

func TestControlRequest(t *testing.T) {
	stdin := []byte("payload")
	limits := &client.ArchiveLimits{MaxEntries: 12, MaxFileBytes: 34, MaxExpandedBytes: 56}
	got := ControlRequest(client.ExecRequest{
		Kind:          "fs_write",
		RootDir:       "/root",
		Path:          "/file",
		Directory:     true,
		User:          "1000:1000",
		Stdin:         stdin,
		ArchiveLimits: limits,
	}, "/work")
	if got.Kind != "fs_write" || got.RootDir != "/root" || got.Path != "/file" || !got.Directory || got.User != "1000:1000" {
		t.Fatalf("control request = %+v", got)
	}
	if got.WorkDir != "/work" {
		t.Fatalf("workdir = %q", got.WorkDir)
	}
	if got.ArchiveLimits != limits {
		t.Fatalf("archive limits were not preserved: %#v", got.ArchiveLimits)
	}
	stdin[0] = 'X'
	if string(got.Stdin) != "payload" {
		t.Fatalf("stdin was not copied: %q", got.Stdin)
	}

	got = ControlRequest(client.ExecRequest{Kind: "sync", WorkDir: "/explicit"}, "/work")
	if got.WorkDir != "/explicit" {
		t.Fatalf("explicit workdir = %q", got.WorkDir)
	}
}

func TestCheckControlRequestCapabilities(t *testing.T) {
	denyCases := []struct {
		name string
		kind string
		caps managedguest.Capabilities
	}{
		{name: "mkdir needs copy in", kind: "fs_mkdir"},
		{name: "write needs copy in", kind: "fs_write"},
		{name: "extract needs copy in first", kind: "fs_extract"},
		{name: "extract needs archive", kind: "fs_extract", caps: managedguest.Capabilities{CopyIn: true}},
		{name: "archive needs copy out", kind: "fs_archive"},
		{name: "unknown kind", kind: "unknown", caps: managedguest.Capabilities{CopyIn: true, CopyOut: true, ArchiveExtract: true}},
	}
	for _, tc := range denyCases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckControlRequest("TestOS", tc.caps, tc.kind)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}

	allowCaps := managedguest.Capabilities{CopyIn: true, CopyOut: true, ArchiveExtract: true}
	for _, kind := range []string{"", "exec", "sync", "fs_mkdir", "fs_write", "fs_extract", "fs_archive"} {
		if err := CheckControlRequest("TestOS", allowCaps, kind); err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
	}
}

func TestUnsupportedFeatureUsesCapabilitySignal(t *testing.T) {
	err := UnsupportedFeature("TestOS", managedguest.Capabilities{RootSnapshot: true}, "root snapshots")
	if err == nil {
		t.Fatalf("advertised capability error = %v", err)
	}
	err = UnsupportedFeature("TestOS", managedguest.Capabilities{}, "root snapshots")
	if err == nil {
		t.Fatalf("unsupported capability error = %v", err)
	}
}

func TestCheckAlternateImageExecUsesCapabilities(t *testing.T) {
	if err := CheckAlternateImageExec(nil); err == nil {
		t.Fatalf("nil provider error = %v", err)
	}
	if err := CheckAlternateImageExec(staticCapabilityProvider{}); err == nil {
		t.Fatalf("denied provider error = %v", err)
	}
	if err := CheckAlternateImageExec(staticCapabilityProvider{caps: managedguest.Capabilities{AlternateImageExec: true}}); err != nil {
		t.Fatalf("allowed provider: %v", err)
	}
}

type staticCapabilityProvider struct {
	caps managedguest.Capabilities
}

func (p staticCapabilityProvider) ManagedCapabilities() managedguest.Capabilities {
	return p.caps
}
