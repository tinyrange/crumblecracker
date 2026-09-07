//go:build darwin && arm64

package hvf

import (
	"context"
	"fmt"

	"strings"

	managedhost "github.com/tinyrange/crumblecracker/internal/core/managed/host"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/protocol"

	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

type Host struct{}

type LinuxManagedMachine struct {
	Spec       machine.Spec
	RunRequest vmruntime.RunRequest
}

type LinuxManagedAttachments struct {
	RunRequest vmruntime.RunRequest
}

func (Host) Start(ctx context.Context, req managedhost.StartRequest, onEvent func(client.BootEvent) error) (managedsession.Session, error) {
	switch managedGuestKind(req.Spec) {
	case "linux":
		return Host{}.startLinux(ctx, req, onEvent)

	default:
		return nil, fmt.Errorf("hvf host does not support managed guest %q boot %q", req.Spec.Guest, req.Spec.Boot.Kind)
	}
}

func (Host) startLinux(ctx context.Context, req managedhost.StartRequest, onEvent func(client.BootEvent) error) (managedsession.Session, error) {
	var attachments LinuxManagedAttachments
	switch value := req.Attachments.(type) {
	case LinuxManagedAttachments:
		attachments = value
	case *LinuxManagedAttachments:
		if value != nil {
			attachments = *value
		}
	default:
		return nil, fmt.Errorf("hvf linux managed attachments have type %T", req.Attachments)
	}
	return Host{}.StartLinuxManaged(ctx, LinuxManagedMachine{
		Spec:       req.Spec,
		RunRequest: attachments.RunRequest,
	}, onEvent)
}

func (Host) StartLinuxManaged(ctx context.Context, machine LinuxManagedMachine, onEvent func(client.BootEvent) error) (*ContainerSession, error) {
	machine = normalizeLinuxManagedMachine(machine)
	req := machine.RunRequest
	req.Persistent = true
	if req.MemoryMB == 0 {
		req.MemoryMB = machine.Spec.MemoryMB
	}
	if req.CPUs == 0 {
		req.CPUs = machine.Spec.CPUs
	}
	if !req.Dmesg {
		req.Dmesg = machine.Spec.Dmesg
	}
	if snapshotPath := strings.TrimSpace(req.RestoreSnapshot); snapshotPath != "" {
		return StartContainerFromSnapshot(ctx, req, snapshotPath, onEvent)
	}
	return StartContainerStream(ctx, req, onEvent)
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
	if guest == "openbsd" || boot == "openbsd" {
		return "openbsd"
	}
	if guest == "freebsd" || boot == "freebsd" {
		return "freebsd"
	}
	if guest == "netbsd" || boot == "netbsd" {
		return "netbsd"
	}
	return guest
}
