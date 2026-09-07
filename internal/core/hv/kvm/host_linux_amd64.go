//go:build linux && amd64

package kvm

import (
	"context"
	"fmt"

	"strings"

	managedhost "github.com/tinyrange/crumblecracker/internal/core/managed/host"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/protocol"

	"github.com/tinyrange/crumblecracker/internal/core/shmem"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

type Host struct{}

type LinuxManagedMachine struct {
	Spec            machine.Spec
	Kernel          []byte
	Initrd          []byte
	FSDevices       []*virtio.FS
	NetDevice       *virtio.Net
	BalloonMB       uint64
	DisplayWidth    uint32
	DisplayHeight   uint32
	SnapshotDir     string
	RestoreSnapshot string
	SharedMemory    *shmem.Attachment
}

type LinuxManagedAttachments struct {
	FSDevices       []*virtio.FS
	NetDevice       *virtio.Net
	BalloonMB       uint64
	DisplayWidth    uint32
	DisplayHeight   uint32
	SnapshotDir     string
	RestoreSnapshot string
	SharedMemory    *shmem.Attachment
}

func (Host) Start(ctx context.Context, req managedhost.StartRequest, onEvent func(client.BootEvent) error) (managedsession.Session, error) {
	switch managedGuestKind(req.Spec) {
	case "linux":
		return Host{}.startLinux(ctx, req, onEvent)

	default:
		return nil, fmt.Errorf("kvm host does not support managed guest %q boot %q", req.Spec.Guest, req.Spec.Boot.Kind)
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
		return nil, fmt.Errorf("kvm linux managed attachments have type %T", req.Attachments)
	}
	return Host{}.StartLinuxManaged(ctx, LinuxManagedMachine{
		Spec:            req.Spec,
		Kernel:          req.Artifact.Kernel,
		Initrd:          req.Artifact.Initrd,
		FSDevices:       attachments.FSDevices,
		NetDevice:       attachments.NetDevice,
		BalloonMB:       attachments.BalloonMB,
		DisplayWidth:    attachments.DisplayWidth,
		DisplayHeight:   attachments.DisplayHeight,
		SnapshotDir:     strings.TrimSpace(attachments.SnapshotDir),
		RestoreSnapshot: strings.TrimSpace(attachments.RestoreSnapshot),
		SharedMemory:    attachments.SharedMemory,
	}, onEvent)
}

func (Host) StartLinuxManaged(ctx context.Context, machine LinuxManagedMachine, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	machine = normalizeLinuxManagedMachine(machine)
	return StartManagedSessionWithNetOptions(
		ctx,
		machine.Kernel,
		machine.Initrd,
		machine.Spec.MemoryMB,
		machine.Spec.CPUs,
		machine.Spec.Dmesg,
		machine.FSDevices,
		machine.NetDevice,
		ManagedSessionOptions{
			SnapshotDir:     strings.TrimSpace(machine.SnapshotDir),
			RestoreSnapshot: strings.TrimSpace(machine.RestoreSnapshot),
			BalloonMB:       machine.BalloonMB,
			DisplayWidth:    machine.DisplayWidth,
			DisplayHeight:   machine.DisplayHeight,
			SharedMemory:    machine.SharedMemory,
		},
		onEvent,
	)
}

func normalizeLinuxManagedMachine(machine LinuxManagedMachine) LinuxManagedMachine {
	if machine.Spec.Guest == "" {
		machine.Spec.Guest = "Linux"
	}
	if machine.Spec.Arch == "" {
		machine.Spec.Arch = "amd64"
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
