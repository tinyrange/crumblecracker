package vm

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	"github.com/tinyrange/crumblecracker/internal/core/vm/execplan"
	"github.com/tinyrange/crumblecracker/internal/core/vm/mounts"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

type managedConformanceFeature struct {
	name      string
	supported func(guestCapabilities) bool
}

var managedConformanceChecklist = []managedConformanceFeature{
	{name: "boot", supported: func(c guestCapabilities) bool { return c.PersistentExec }},
	{name: "exec", supported: func(c guestCapabilities) bool { return c.PersistentExec }},
	{name: "streaming exec", supported: func(c guestCapabilities) bool { return c.StreamingExec }},
	{name: "tty", supported: func(c guestCapabilities) bool { return c.TTY }},
	{name: "resize tty", supported: func(c guestCapabilities) bool { return c.ResizeTTY }},
	{name: "stdin", supported: func(c guestCapabilities) bool { return c.StreamingExec }},
	{name: "signals", supported: func(c guestCapabilities) bool { return c.Signals }},
	{name: "copy in", supported: func(c guestCapabilities) bool { return c.CopyIn }},
	{name: "copy out", supported: func(c guestCapabilities) bool { return c.CopyOut }},
	{name: "archive extract", supported: func(c guestCapabilities) bool { return c.ArchiveExtract }},
	{name: "network", supported: func(c guestCapabilities) bool { return c.Network }},
	{name: "dns", supported: func(c guestCapabilities) bool { return c.DNS }},
	{name: "package manager", supported: func(c guestCapabilities) bool { return c.PackageManager != "" }},
	{name: "alternate image exec", supported: func(c guestCapabilities) bool { return c.AlternateImageExec }},
	{name: "root snapshot", supported: func(c guestCapabilities) bool { return c.RootSnapshot }},
	{name: "image snapshot", supported: func(c guestCapabilities) bool { return c.ImageSnapshot }},
	{name: "writable root block", supported: func(c guestCapabilities) bool { return c.WritableRootBlock }},
	{name: "console history", supported: func(c guestCapabilities) bool { return c.PersistentExec }},
	{name: "shutdown", supported: func(c guestCapabilities) bool { return c.PersistentExec }},
}

func TestManagedGuestProfilesDeclareConformance(t *testing.T) {
	tests := []struct {
		name    string
		profile managedguest.Profile
		want    map[string]bool
		wantPkg string
	}{
		{
			name: "Linux on KVM", profile: managedguest.LinuxProfile,
			want: managedConformanceSupport(map[string]bool{"package manager": false}),
		},
		{
			name: "Linux on WHP/Windows", profile: managedguest.LinuxProfile,
			want: managedConformanceSupport(map[string]bool{"package manager": false}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := guestCapabilities(tc.profile.Caps)
			for _, feature := range managedConformanceChecklist {
				supported := feature.supported(caps)
				want, ok := tc.want[feature.name]
				if !ok {
					t.Fatalf("%s conformance matrix missing feature %q", tc.name, feature.name)
				}
				if supported != want {
					t.Fatalf("%s %q support = %v, want %v: %+v", tc.name, feature.name, supported, want, caps)
				}
			}
			if caps.PackageManager != tc.wantPkg {
				t.Fatalf("%s package manager = %q, want %q", tc.name, caps.PackageManager, tc.wantPkg)
			}
			if vmruntime.InstanceReadyMarker == "" {
				t.Fatalf("%s ready marker is empty", tc.name)
			}
		})
	}
}

type managedConformanceGate struct {
	name      string
	supported func(guestCapabilities) bool
	check     func(managedguest.Profile) error
}

func TestManagedConformanceMatrixMatchesFeatureGates(t *testing.T) {
	gates := []managedConformanceGate{
		{
			name:      "copy in",
			supported: func(c guestCapabilities) bool { return c.CopyIn },
			check: func(profile managedguest.Profile) error {
				return execplan.CheckControlRequest(profile.Name, guestCapabilities(profile.Caps), "fs_write")
			},
		},
		{
			name:      "copy out",
			supported: func(c guestCapabilities) bool { return c.CopyOut },
			check: func(profile managedguest.Profile) error {
				return execplan.CheckControlRequest(profile.Name, guestCapabilities(profile.Caps), "fs_archive")
			},
		},
		{
			name:      "archive extract",
			supported: func(c guestCapabilities) bool { return c.CopyIn && c.ArchiveExtract },
			check: func(profile managedguest.Profile) error {
				return execplan.CheckControlRequest(profile.Name, guestCapabilities(profile.Caps), "fs_extract")
			},
		},
		{
			name:      "alternate image exec",
			supported: func(c guestCapabilities) bool { return c.AlternateImageExec },
			check: func(profile managedguest.Profile) error {
				return execplan.CheckAlternateImageExec(staticCapabilityProvider{caps: guestCapabilities(profile.Caps)})
			},
		},
		{
			name:      "root snapshot",
			supported: func(c guestCapabilities) bool { return c.RootSnapshot },
			check: func(profile managedguest.Profile) error {
				_, err := mounts.RootSnapshotWithCapabilities(profile.Name, guestCapabilities(profile.Caps), &recordingSnapshotter{}, "")
				return err
			},
		},
		{
			name:      "image snapshot",
			supported: func(c guestCapabilities) bool { return c.ImageSnapshot },
			check: func(profile managedguest.Profile) error {
				_, err := mounts.ImageSnapshotWithCapabilities(profile.Name, guestCapabilities(profile.Caps), &recordingSnapshotter{}, "tools", "/run/images/tools")
				return err
			},
		},
	}
	profiles := []managedguest.Profile{
		managedguest.LinuxProfile,
	}
	for _, profile := range profiles {
		t.Run(profile.Name, func(t *testing.T) {
			caps := guestCapabilities(profile.Caps)
			for _, gate := range gates {
				t.Run(gate.name, func(t *testing.T) {
					err := gate.check(profile)
					if gate.supported(caps) {
						if err != nil {
							t.Fatalf("%s gate rejected advertised capability: %v", gate.name, err)
						}
						return
					}
					if err == nil {
						t.Fatalf("%s gate allowed unsupported capability", gate.name)
					}
				})
			}
		})
	}
}

func managedConformanceSupport(overrides map[string]bool) map[string]bool {
	out := make(map[string]bool, len(managedConformanceChecklist))
	for _, feature := range managedConformanceChecklist {
		out[feature.name] = true
	}
	for feature, supported := range overrides {
		out[feature] = supported
	}
	return out
}

type recordingSnapshotter struct{}

func (recordingSnapshotter) RootSnapshot() (imagefs.Directory, error) {
	return nil, nil
}

func (recordingSnapshotter) RootSnapshotAt(string) (imagefs.Directory, error) {
	return nil, nil
}

type staticCapabilityProvider struct {
	caps guestCapabilities
}

func (p staticCapabilityProvider) ManagedCapabilities() guestCapabilities {
	return p.caps
}

func TestManagedBootFatalTextMarkers(t *testing.T) {
	fatal := []string{
		"ccx3-init-fatal: setup failed",
		"Kernel panic - not syncing",
		"kernel panic",
		"panic: trap",
		"reboot: System halted",
	}
	for _, text := range fatal {
		if !vmruntime.HasFatalBootText(text) {
			t.Fatalf("HasFatalBootText(%q) = false, want true", text)
		}
	}
	if vmruntime.HasFatalBootText(vmruntime.InstanceReadyMarker + "\n") {
		t.Fatalf("ready marker was classified as fatal")
	}
}
