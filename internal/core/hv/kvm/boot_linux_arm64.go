//go:build linux && arm64

package kvm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/fdt"
	"github.com/tinyrange/crumblecracker/internal/core/nvme"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

func BootKernelToSerial(ctx context.Context, kernel []byte, memoryMB uint64, dmesg bool) (string, error) {
	return bootToCondition(ctx, kernel, nil, memoryMB, dmesg, nil, nil, func(serial string) bool {
		return serial != ""
	})
}

func BootInitramfsToMarker(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	return bootToCondition(ctx, kernel, initrd, memoryMB, dmesg, nil, nil, func(serial string) bool {
		return strings.Contains(serial, marker)
	})
}

func BootInitramfsToMarkerWithFS(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string, fsdevs []*virtio.FS) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	nodes := make([]fdt.Node, 0, len(fsdevs))
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		nodes = append(nodes, fsdev.DeviceTreeNode())
	}
	return bootToCondition(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, nodes, func(serial string) bool {
		return strings.Contains(serial, marker)
	})
}

func BootInitramfsToMarkerWithNVMeBlock(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string, block *nvme.Controller) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	return bootToConditionWithNVMeBlock(ctx, kernel, initrd, memoryMB, dmesg, block, func(serial string) bool {
		return strings.Contains(serial, marker)
	})
}

func BootInitramfsToVsockMarkerWithFS(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, port uint32, marker string, fsdevs []*virtio.FS) (string, string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", "", fmt.Errorf("boot marker is required")
	}
	backend := virtio.NewSimpleVsockBackend()
	listener, err := backend.Listen(port)
	if err != nil {
		return "", "", fmt.Errorf("listen vsock control: %w", err)
	}
	defer listener.Close()

	controlConnCh := make(chan virtio.VsockConn, 1)
	controlErrCh := make(chan error, 1)
	var controlOut bytes.Buffer
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			controlErrCh <- err
			return
		}
		controlConnCh <- conn
		_, copyErr := io.Copy(&controlOut, conn)
		if copyErr != nil {
			controlErrCh <- copyErr
			return
		}
		controlErrCh <- nil
	}()

	vsock := virtio.NewVsock(arm64vm.VsockBase, arm64vm.VsockSize, arm64vm.VsockIRQ, vmruntime.GuestCID, backend)
	defer vsock.Close()
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)

	nodes := []fdt.Node{vsock.DeviceTreeNode(), rng.DeviceTreeNode()}
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			nodes = append(nodes, fsdev.DeviceTreeNode())
		}
	}

	serial, err := bootToConditionWithDevices(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, vsock, rng, nodes, func(serial string) bool {
		return strings.Contains(controlOut.String(), marker)
	})
	select {
	case conn := <-controlConnCh:
		_ = conn.Close()
	default:
	}
	if err != nil {
		return serial, controlOut.String(), err
	}
	return serial, controlOut.String(), nil
}

func bootToCondition(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, extraNodes []fdt.Node, done func(string) bool) (string, error) {
	return bootToConditionWithDevices(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, nil, nil, extraNodes, done)
}

func bootToConditionWithNVMeBlock(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, block *nvme.Controller, done func(string) bool) (string, error) {
	vm, err := NewVM()
	if err != nil {
		return "", err
	}
	defer vm.Close()

	mem, err := vm.MapAnonymousMemory(arm64vm.MemorySizeBytes(memoryMB), arm64vm.MemoryBase)
	if err != nil {
		return "", fmt.Errorf("map guest memory: %w", err)
	}

	var serialOut bytes.Buffer
	uart := serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, &serialOut)
	uart.AttachIRQ(vm, arm64vm.UARTSPI)
	block.Attach(vm, vm)
	pci := NewArm64PCIHost(NewArm64NVMePCIDevice(1, arm64vm.NVMeBase, arm64vm.NVMeIRQ, block))
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)
	rng.Attach(vm, vm)

	plan, err := arm64vm.PrepareBoot(mem, kernel, initrd, arm64vm.BootConfig{
		MemoryMB:   memoryMB,
		GICVersion: arm64vm.GICVersionV2,
		Dmesg:      dmesg,
		ExtraNodes: []fdt.Node{pci.DeviceTreeNode(), rng.DeviceTreeNode()},
	})
	if err != nil {
		return "", fmt.Errorf("prepare boot: %w", err)
	}

	if err := vm.SetPC(plan.EntryGPA); err != nil {
		return "", fmt.Errorf("set PC: %w", err)
	}
	if err := vm.SetPState(arm64vm.DefaultPStateBits); err != nil {
		return "", fmt.Errorf("set PSTATE: %w", err)
	}
	if err := vm.SetSpEl1(plan.StackTopGPA); err != nil {
		return "", fmt.Errorf("set SP_EL1: %w", err)
	}
	if err := vm.SetX(0, plan.DeviceTreeGPA); err != nil {
		return "", fmt.Errorf("set X0: %w", err)
	}
	for reg := 1; reg <= 3; reg++ {
		if err := vm.SetX(reg, 0); err != nil {
			return "", fmt.Errorf("set X%d: %w", reg, err)
		}
	}

	var exit Exit
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			if done(serialOut.String()) {
				return serialOut.String(), nil
			}
			return serialOut.String(), err
		}

		if err := vm.Run(&exit); err != nil {
			return serialOut.String(), fmt.Errorf("run step %d: %w", step, err)
		}
		if done(serialOut.String()) {
			return serialOut.String(), nil
		}

		switch exit.Reason {
		case ExitMMIO:
			if uart.Contains(exit.MMIO.Addr, int(exit.MMIO.Len)) {
				if err := handleUARTExit(vm, uart, exit.MMIO); err != nil {
					return serialOut.String(), err
				}
				continue
			}
			if handled, err := pci.HandleMMIO(vm, exit.MMIO); handled || err != nil {
				if err != nil {
					return serialOut.String(), err
				}
				continue
			}
			if err := handleBootMMIO(vm, uart, nil, nil, rng, exit.MMIO); err != nil {
				return serialOut.String(), err
			}
		case ExitShutdown:
			return serialOut.String(), fmt.Errorf("guest shut down before serial output")
		case ExitSystemEvent:
			return serialOut.String(), fmt.Errorf("unexpected system event %d before serial output", exit.SystemEvent)
		default:
			pc, _ := vm.GetPC()
			return serialOut.String(), fmt.Errorf("unexpected exit reason %d at pc=%#x", exit.Reason, pc)
		}
	}
}

func bootToConditionWithDevices(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, extraNodes []fdt.Node, done func(string) bool) (string, error) {
	vm, err := NewVM()
	if err != nil {
		return "", err
	}
	defer vm.Close()
	defer closeFSDevices(fsdevs)

	mem, err := vm.MapAnonymousMemory(arm64vm.MemorySizeBytes(memoryMB), arm64vm.MemoryBase)
	if err != nil {
		return "", fmt.Errorf("map guest memory: %w", err)
	}

	var serialOut bytes.Buffer
	uart := serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, &serialOut)
	uart.AttachIRQ(vm, arm64vm.UARTSPI)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Attach(vm, vm)
		}
	}
	if vsock != nil {
		vsock.Attach(vm, vm)
	}
	if rng != nil {
		rng.Attach(vm, vm)
	}

	plan, err := arm64vm.PrepareBoot(mem, kernel, initrd, arm64vm.BootConfig{
		MemoryMB:   memoryMB,
		GICVersion: arm64vm.GICVersionV2,
		Dmesg:      dmesg,
		ExtraNodes: append([]fdt.Node(nil), extraNodes...),
	})
	if err != nil {
		return "", fmt.Errorf("prepare boot: %w", err)
	}

	if err := vm.SetPC(plan.EntryGPA); err != nil {
		return "", fmt.Errorf("set PC: %w", err)
	}
	if err := vm.SetPState(arm64vm.DefaultPStateBits); err != nil {
		return "", fmt.Errorf("set PSTATE: %w", err)
	}
	if err := vm.SetSpEl1(plan.StackTopGPA); err != nil {
		return "", fmt.Errorf("set SP_EL1: %w", err)
	}
	if err := vm.SetX(0, plan.DeviceTreeGPA); err != nil {
		return "", fmt.Errorf("set X0: %w", err)
	}
	for reg := 1; reg <= 3; reg++ {
		if err := vm.SetX(reg, 0); err != nil {
			return "", fmt.Errorf("set X%d: %w", reg, err)
		}
	}

	var exit Exit
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			if done(serialOut.String()) {
				return serialOut.String(), nil
			}
			return "", err
		}

		if err := vm.Run(&exit); err != nil {
			return serialOut.String(), fmt.Errorf("run step %d: %w", step, err)
		}
		if done(serialOut.String()) {
			return serialOut.String(), nil
		}

		switch exit.Reason {
		case ExitMMIO:
			if err := handleBootMMIO(vm, uart, fsdevs, vsock, rng, exit.MMIO); err != nil {
				return serialOut.String(), err
			}
		case ExitShutdown:
			return serialOut.String(), fmt.Errorf("guest shut down before serial output")
		case ExitSystemEvent:
			return serialOut.String(), fmt.Errorf("unexpected system event %d before serial output", exit.SystemEvent)
		default:
			pc, _ := vm.GetPC()
			return serialOut.String(), fmt.Errorf("unexpected exit reason %d at pc=%#x", exit.Reason, pc)
		}
	}
}

func BootKernelToSerialWithTimeout(kernel []byte, memoryMB uint64, dmesg bool, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return BootKernelToSerial(ctx, kernel, memoryMB, dmesg)
}

func BootInitramfsToMarkerWithTimeout(kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return BootInitramfsToMarker(ctx, kernel, initrd, memoryMB, dmesg, marker)
}

func handleUARTExit(vm *VM, uart *serial.UART8250, mmio MMIOExit) error {
	if !uart.Contains(mmio.Addr, int(mmio.Len)) {
		return fmt.Errorf("unhandled mmio addr=%#x len=%d write=%v", mmio.Addr, mmio.Len, mmio.Write)
	}
	if mmio.Write {
		return uart.Write(mmio.Addr, mmio.Data[:mmio.Len])
	}
	value, err := uart.ReadValue(mmio.Addr, int(mmio.Len))
	if err != nil {
		return err
	}
	vm.CompleteMMIORead(value, mmio.Len)
	return nil
}

func handleBootMMIO(vm *VM, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, mmio MMIOExit) error {
	return handleBootMMIOWithExtra(vm, uart, fsdevs, vsock, rng, nil, mmio)
}

func handleBootMMIOWithExtra(vm *VM, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, extra []virtio.MMIODevice, mmio MMIOExit) error {
	if uart.Contains(mmio.Addr, int(mmio.Len)) {
		return handleUARTExit(vm, uart, mmio)
	}
	for _, fsdev := range fsdevs {
		if fsdev == nil || !fsdev.Contains(mmio.Addr, int(mmio.Len)) {
			continue
		}
		if mmio.Write {
			return fsdev.Write(mmio.Addr, int(mmio.Len), mmioValue(mmio))
		}
		value, err := fsdev.Read(mmio.Addr, int(mmio.Len))
		if err != nil {
			return err
		}
		vm.CompleteMMIORead(value, mmio.Len)
		return nil
	}
	if vsock != nil && vsock.Contains(mmio.Addr, int(mmio.Len)) {
		if mmio.Write {
			return vsock.Write(mmio.Addr, int(mmio.Len), mmioValue(mmio))
		}
		value, err := vsock.Read(mmio.Addr, int(mmio.Len))
		if err != nil {
			return err
		}
		vm.CompleteMMIORead(value, mmio.Len)
		return nil
	}
	if rng != nil && rng.Contains(mmio.Addr, int(mmio.Len)) {
		if mmio.Write {
			return rng.Write(mmio.Addr, int(mmio.Len), mmioValue(mmio))
		}
		value, err := rng.Read(mmio.Addr, int(mmio.Len))
		if err != nil {
			return err
		}
		vm.CompleteMMIORead(value, mmio.Len)
		return nil
	}
	for _, device := range extra {
		if device == nil || !device.Contains(mmio.Addr, int(mmio.Len)) {
			continue
		}
		if mmio.Write {
			return device.Write(mmio.Addr, int(mmio.Len), mmioValue(mmio))
		}
		value, err := device.Read(mmio.Addr, int(mmio.Len))
		if err != nil {
			return err
		}
		vm.CompleteMMIORead(value, mmio.Len)
		return nil
	}
	if inRange(mmio.Addr, arm64vm.GICDistributorMin, arm64vm.GICDistributorMax) {
		if !mmio.Write {
			vm.CompleteMMIORead(readBootGICDistributor(mmio.Addr-arm64vm.GICDistributorMin), mmio.Len)
		}
		return nil
	}
	if inRange(mmio.Addr, arm64vm.GICRedistributorMin, arm64vm.GICRedistributorMax) {
		if !mmio.Write {
			vm.CompleteMMIORead(readBootGICRedistributor(mmio.Addr-arm64vm.GICRedistributorMin), mmio.Len)
		}
		return nil
	}
	return fmt.Errorf("unhandled mmio addr=%#x len=%d write=%v", mmio.Addr, mmio.Len, mmio.Write)
}

func readBootGICDistributor(offset uint64) uint64 {
	switch offset {
	case 0xffe8:
		return 0x30
	default:
		return 0
	}
}

func readBootGICRedistributor(offset uint64) uint64 {
	switch offset {
	case 0x0:
		return 0
	case 0x8:
		return 1 << 4
	case 0x14:
		return 0
	case 0xffe8:
		return 0x30
	default:
		return 0
	}
}

func inRange(addr, start, end uint64) bool {
	return addr >= start && addr < end
}

func mmioValue(mmio MMIOExit) uint64 {
	switch mmio.Len {
	case 1:
		return uint64(mmio.Data[0])
	case 2:
		return uint64(mmio.Data[0]) | uint64(mmio.Data[1])<<8
	case 4:
		return uint64(mmio.Data[0]) |
			uint64(mmio.Data[1])<<8 |
			uint64(mmio.Data[2])<<16 |
			uint64(mmio.Data[3])<<24
	default:
		var value uint64
		for i := uint32(0); i < mmio.Len && i < 8; i++ {
			value |= uint64(mmio.Data[i]) << (8 * i)
		}
		return value
	}
}
