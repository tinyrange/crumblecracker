//go:build windows && amd64

package whp

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestBootPICLevelLineResamplesUntilDeasserted(t *testing.T) {
	var pic bootPIC
	pic.master.vectorBase = 0x20
	pic.slave.vectorBase = 0x28
	pic.master.mask = 0xff
	pic.slave.mask = 0xff
	pic.master.mask &^= 1 << 2
	pic.slave.mask &^= 1 << 2
	pic.elcr[1] = 1 << 2

	pic.SetIRQ(10, true)
	vector, line, ok := pic.AcknowledgePending()
	if !ok || vector != 0x2a || line != 10 {
		t.Fatalf("first ack = vector %#x line %d ok %t, want vector 0x2a line 10", vector, line, ok)
	}
	vector, line, ok = pic.AcknowledgePending()
	if !ok || vector != 0x2a || line != 10 {
		t.Fatalf("level ack = vector %#x line %d ok %t, want vector 0x2a line 10", vector, line, ok)
	}
	pic.SetIRQ(10, false)
	if vector, line, ok := pic.AcknowledgePending(); ok {
		t.Fatalf("ack after deassert = vector %#x line %d ok %t, want no pending irq", vector, line, ok)
	}
}

func TestBootPICSetLevelTriggeredResamplesAssertedLine(t *testing.T) {
	var pic bootPIC
	pic.master.vectorBase = 0x20
	pic.slave.vectorBase = 0x28
	pic.master.mask = 0xff
	pic.slave.mask = 0xff
	pic.master.mask &^= 1 << 2
	pic.slave.mask &^= 1 << 2

	pic.SetIRQ(10, true)
	vector, line, ok := pic.AcknowledgePending()
	if !ok || vector != 0x2a || line != 10 {
		t.Fatalf("edge ack = vector %#x line %d ok %t, want vector 0x2a line 10", vector, line, ok)
	}
	if _, _, ok := pic.AcknowledgePending(); ok {
		t.Fatalf("edge line delivered twice before level trigger was enabled")
	}

	pic.SetLevelTriggered(10, true)
	vector, line, ok = pic.AcknowledgePending()
	if !ok || vector != 0x2a || line != 10 {
		t.Fatalf("level ack = vector %#x line %d ok %t, want vector 0x2a line 10", vector, line, ok)
	}
}

func TestBootPICEndOfInterruptClearsSlaveISRAndResamples(t *testing.T) {
	var pic bootPIC
	pic.master.vectorBase = 0x30
	pic.slave.vectorBase = 0x38
	pic.master.mask = 0xff
	pic.slave.mask = 0xff
	pic.master.mask &^= 1 << 2
	pic.slave.mask &^= 1 << 2
	pic.SetLevelTriggered(10, true)

	pic.SetIRQ(10, true)
	vector, line, ok := pic.AcknowledgePending()
	if !ok || vector != 0x3a || line != 10 {
		t.Fatalf("ack = vector %#x line %d ok %t, want vector 0x3a line 10", vector, line, ok)
	}
	if !pic.EndOfInterrupt(vector) {
		t.Fatalf("EndOfInterrupt(%#x) = false, want true", vector)
	}
	if pic.slave.isr&(1<<2) != 0 {
		t.Fatalf("slave ISR still has IRQ 10 set after EOI")
	}
	if pic.master.isr&(1<<2) != 0 {
		t.Fatalf("master cascade ISR still set after slave EOI")
	}
	vector, line, ok = pic.AcknowledgePending()
	if !ok || vector != 0x3a || line != 10 {
		t.Fatalf("resampled ack = vector %#x line %d ok %t, want vector 0x3a line 10", vector, line, ok)
	}
}

func TestBootPICEdgeLineRequiresNewRisingEdge(t *testing.T) {
	var pic bootPIC
	pic.master.vectorBase = 0x20
	pic.master.mask = 0xfe

	pic.SetIRQ(0, true)
	vector, line, ok := pic.AcknowledgePending()
	if !ok || vector != 0x20 || line != 0 {
		t.Fatalf("first ack = vector %#x line %d ok %t, want vector 0x20 line 0", vector, line, ok)
	}
	if vector, line, ok := pic.AcknowledgePending(); ok {
		t.Fatalf("second ack = vector %#x line %d ok %t, want no pending irq", vector, line, ok)
	}
	pic.SetIRQ(0, false)
	pic.SetIRQ(0, true)
	vector, line, ok = pic.AcknowledgePending()
	if !ok || vector != 0x20 || line != 0 {
		t.Fatalf("new edge ack = vector %#x line %d ok %t, want vector 0x20 line 0", vector, line, ok)
	}
}

func TestBootIOAPICActiveLowLevelLineUsesAssertedState(t *testing.T) {
	var ioapic bootIOAPIC
	ioapic.init()
	ioapic.redir[12] = 0x62 | 1<<13 | 1<<15

	route, pending := ioapic.assert(12, true)
	if !pending {
		t.Fatalf("assert active-low level line did not produce a pending route")
	}
	if route.line != 12 || route.vector != 0x62 || !route.level {
		t.Fatalf("route = %+v, want line 12 vector 0x62 level", route)
	}
	if _, pending := ioapic.deviceHighRoute(12); !pending {
		t.Fatalf("deviceHighRoute after assert = false, want true")
	}

	ioapic.assert(12, false)
	if _, pending := ioapic.deviceHighRoute(12); pending {
		t.Fatalf("deviceHighRoute after deassert = true, want false")
	}
}

func TestPendingInterruptionWaitsUntilGuestEnablesInterrupts(t *testing.T) {
	ctx := &runVPExitContext{
		VpContext: vpExitContext{
			Rflags: 0x2,
		},
	}
	if canSetPendingInterruption(ctx, 0x3b) {
		t.Fatal("pending interruption accepted while guest interrupts are disabled")
	}

	ctx.VpContext.Rflags |= 1 << 9
	if !canSetPendingInterruption(ctx, 0x3b) {
		t.Fatal("pending interruption rejected after guest enabled interrupts")
	}
}

func TestBootIOAPICRoutePreservesVirtualCPUDestination(t *testing.T) {
	var ioapic bootIOAPIC
	ioapic.init()
	ioapic.redir[5] = 0x51 | 1<<8 | 1<<11 | uint64(0x04)<<56

	route, ok := ioapic.routeForLine(5)
	if !ok {
		t.Fatal("routeForLine(5) did not return the configured route")
	}
	if route.vector != 0x51 || route.interruptType != interruptTypeLowestPriority {
		t.Fatalf("route vector/type = %#x/%d, want 0x51/%d", route.vector, route.interruptType, interruptTypeLowestPriority)
	}
	if route.destinationMode != interruptDestinationLogical || route.destination != 0x04 {
		t.Fatalf("route destination = mode %d value %#x, want logical/0x04", route.destinationMode, route.destination)
	}
}

func TestVirtioPendingInterruptTargetsRoutedVCPU(t *testing.T) {
	platform := &bootPlatform{
		vm:     &VM{vcpuCount: 4},
		fsdevs: []*virtio.FS{{IRQ: 5}},
	}
	route := bootIOAPICRoute{
		line:            5,
		vector:          0x51,
		destinationMode: interruptDestinationPhysical,
		destination:     3,
	}
	if target := platform.pendingRouteVCPU(route); target != 3 {
		t.Fatalf("pending physical virtio IRQ target = %d, want vCPU 3", target)
	}

	route.destinationMode = interruptDestinationLogical
	route.destination = 0x04
	if target := platform.pendingRouteVCPU(route); target != 2 {
		t.Fatalf("pending logical virtio IRQ target = %d, want vCPU 2", target)
	}
}
