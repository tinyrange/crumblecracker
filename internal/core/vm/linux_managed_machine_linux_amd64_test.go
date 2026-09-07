//go:build linux && amd64

package vm

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

func TestLinuxManagedMachineSpec(t *testing.T) {
	spec := linuxManagedMachineSpec("vm1", 768, 2, true, nil)
	if spec.ID != "vm1" || spec.Guest != "Linux" || spec.Arch != "amd64" {
		t.Fatalf("unexpected spec identity: %+v", spec)
	}
	if spec.MemoryMB != 768 || spec.CPUs != 2 || !spec.Dmesg {
		t.Fatalf("unexpected spec resources: %+v", spec)
	}
	if spec.Boot.Kind != "linux" {
		t.Fatalf("boot kind = %q", spec.Boot.Kind)
	}
	if spec.Control.Kind != "vsock" || spec.Control.Port != vmruntime.ControlPort {
		t.Fatalf("control spec = %+v", spec.Control)
	}
	if spec.Network != nil || len(spec.Devices) != 0 {
		t.Fatalf("networkless spec has network/device data: %+v", spec)
	}
}
