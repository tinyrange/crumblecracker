//go:build windows && amd64

package whp

import (
	"context"
	"testing"

	managedhost "github.com/tinyrange/crumblecracker/internal/core/managed/host"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	"github.com/tinyrange/crumblecracker/internal/core/managed/rootartifact"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestNormalizeLinuxManagedMachineDefaultsSpec(t *testing.T) {
	machine := normalizeLinuxManagedMachine(LinuxManagedMachine{})
	if machine.Spec.Guest != "Linux" {
		t.Fatalf("guest = %q", machine.Spec.Guest)
	}
	if machine.Spec.Arch != "amd64" {
		t.Fatalf("arch = %q", machine.Spec.Arch)
	}
	if machine.Spec.Boot.Kind != "linux" {
		t.Fatalf("boot kind = %q", machine.Spec.Boot.Kind)
	}
	if machine.Spec.Control.Kind != "vsock" {
		t.Fatalf("control kind = %q", machine.Spec.Control.Kind)
	}
}

func TestWHPHostRejectsUnexpectedLinuxAttachments(t *testing.T) {
	_, err := (Host{}).Start(context.Background(), managedhost.StartRequest{
		Spec:        machine.Spec{Guest: "Linux", Boot: machine.BootSpec{Kind: "linux"}},
		Artifact:    rootartifact.Artifact{Kernel: []byte("kernel"), Initrd: []byte("initrd")},
		Attachments: "bad",
	}, nil)
	if err == nil {
		t.Fatalf("Start unexpected attachments error = %v", err)
	}
}

func TestManagedDesktopProvidesFramebufferAndInput(t *testing.T) {
	desktop, devices, clipboardListener, displayListener, err := newManagedDesktop(virtio.NewSimpleVsockBackend(), 1440, 900)
	if err != nil {
		t.Fatal(err)
	}
	defer closeManagedDisplayListeners(clipboardListener, displayListener)
	if desktop == nil || desktop.GPU == nil || desktop.Keyboard == nil || desktop.Pointer == nil || desktop.Clipboard == nil {
		t.Fatalf("desktop is incomplete: %+v", desktop)
	}
	if width, height := desktop.Framebuffer.Size(); width != 1440 || height != 900 {
		t.Fatalf("framebuffer size = %dx%d, want 1440x900", width, height)
	}
	if len(devices) != 3 {
		t.Fatalf("display device count = %d, want 3", len(devices))
	}
}
