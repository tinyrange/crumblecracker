//go:build windows && arm64

package whp

import (
	"context"
	"fmt"

	"strings"

	managedhost "github.com/tinyrange/crumblecracker/internal/core/managed/host"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/protocol"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

type Host struct{}

type LinuxManagedMachine struct {
	Spec            machine.Spec
	Kernel          []byte
	Initrd          []byte
	FSDevices       []*virtio.FS
	NetDevice       *virtio.Net
	SnapshotDir     string
	RestoreSnapshot string
}

type LinuxManagedAttachments struct {
	FSDevices       []*virtio.FS
	NetDevice       *virtio.Net
	SnapshotDir     string
	RestoreSnapshot string
}

func (Host) Start(ctx context.Context, req managedhost.StartRequest, onEvent func(client.BootEvent) error) (managedsession.Session, error) {
	switch managedGuestKind(req.Spec) {
	case "linux":
		return Host{}.startLinux(ctx, req, onEvent)

	default:
		return nil, fmt.Errorf("whp arm64 host does not support managed guest %q boot %q", req.Spec.Guest, req.Spec.Boot.Kind)
	}
}

func (Host) startLinux(ctx context.Context, req managedhost.StartRequest, onEvent func(client.BootEvent) error) (managedsession.Session, error) {
	var attachments LinuxManagedAttachments
	switch value := req.Attachments.(type) {
	case nil:
	case LinuxManagedAttachments:
		attachments = value
	case *LinuxManagedAttachments:
		if value != nil {
			attachments = *value
		}
	default:
		return nil, fmt.Errorf("whp linux managed attachments have type %T", req.Attachments)
	}
	return Host{}.StartLinuxManaged(ctx, LinuxManagedMachine{
		Spec:            req.Spec,
		Kernel:          req.Artifact.Kernel,
		Initrd:          req.Artifact.Initrd,
		FSDevices:       attachments.FSDevices,
		NetDevice:       attachments.NetDevice,
		SnapshotDir:     attachments.SnapshotDir,
		RestoreSnapshot: attachments.RestoreSnapshot,
	}, onEvent)
}

func (Host) StartLinuxManaged(ctx context.Context, machine LinuxManagedMachine, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	machine = normalizeLinuxManagedMachine(machine)
	opts := ManagedSessionOptions{
		SnapshotDir:     strings.TrimSpace(machine.SnapshotDir),
		RestoreSnapshot: strings.TrimSpace(machine.RestoreSnapshot),
	}
	return StartManagedSessionWithNetOptions(ctx, machine.Kernel, machine.Initrd, machine.Spec.MemoryMB, machine.Spec.Dmesg, machine.FSDevices, machine.NetDevice, opts, onEvent)
}

func normalizeLinuxManagedMachine(machine LinuxManagedMachine) LinuxManagedMachine {
	if machine.Spec.Guest == "" {
		machine.Spec.Guest = "Linux"
	}
	if machine.Spec.Arch == "" {
		machine.Spec.Arch = "arm64"
	}
	if machine.Spec.Boot.Kind == "" {
		machine.Spec.Boot.Kind = "linux"
	}
	if machine.Spec.Control.Kind == "" {
		machine.Spec.Control.Kind = "vsock"
	}
	return machine
}

func managedGuestKind(spec machine.Spec) string {
	guest := strings.ToLower(strings.TrimSpace(spec.Guest))
	boot := strings.ToLower(strings.TrimSpace(spec.Boot.Kind))
	if guest == "" && boot == "" {
		return "linux"
	}
	if guest == "" {
		guest = boot
	}
	if boot == "" {
		return guest
	}
	if guest == "linux" && boot == "linux" {
		return "linux"
	}
	for _, bsd := range []string{"openbsd", "freebsd", "netbsd"} {
		if guest == bsd || boot == bsd {
			return bsd
		}
	}
	return guest
}
