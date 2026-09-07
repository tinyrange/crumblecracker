//go:build darwin && arm64

package hvf

import (
	"context"
	"testing"

	managedhost "github.com/tinyrange/crumblecracker/internal/core/managed/host"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
)

func TestNormalizeLinuxManagedMachineDefaultsSpec(t *testing.T) {
	machine := normalizeLinuxManagedMachine(LinuxManagedMachine{})
	if machine.Spec.Guest != "Linux" {
		t.Fatalf("guest = %q", machine.Spec.Guest)
	}
	if machine.Spec.Arch != "arm64" {
		t.Fatalf("arch = %q", machine.Spec.Arch)
	}
	if machine.Spec.Boot.Kind != "linux" {
		t.Fatalf("boot kind = %q", machine.Spec.Boot.Kind)
	}
	if machine.Spec.Control.Kind != "vsock" {
		t.Fatalf("control kind = %q", machine.Spec.Control.Kind)
	}
}

func TestHVFHostRejectsUnsupportedManagedGuest(t *testing.T) {
	_, err := (Host{}).Start(context.Background(), managedhost.StartRequest{
		Spec: machine.Spec{Guest: "Plan9", Boot: machine.BootSpec{Kind: "plan9"}},
	}, nil)
	if err == nil {
		t.Fatalf("Start unsupported guest error = %v", err)
	}
}

func TestHVFHostRejectsUnexpectedLinuxAttachments(t *testing.T) {
	_, err := (Host{}).Start(context.Background(), managedhost.StartRequest{
		Spec:        machine.Spec{Guest: "Linux", Boot: machine.BootSpec{Kind: "linux"}},
		Attachments: "bad",
	}, nil)
	if err == nil {
		t.Fatalf("Start unexpected attachments error = %v", err)
	}
}
