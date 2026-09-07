//go:build linux && amd64

package kvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	"github.com/tinyrange/crumblecracker/internal/core/nvme"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"golang.org/x/sys/unix"
)

const (
	hpetBaseAddress          = 0xFED00000
	hpetNetBSDBaseAddress    = 0xFED40000
	hpetAlternateBaseAddress = 0xFED80000
	hpetMMIOWindowSize       = 0x400

	hpetRegGeneralCapabilities  = 0x000
	hpetRegGeneralConfiguration = 0x010
	hpetRegInterruptStatus      = 0x020
	hpetRegMainCounter          = 0x0F0

	hpetClockPeriodFemtoseconds = 10_000_000
	hpetVendorID                = 0x8086
	hpetNumTimers               = 3
	hpetLegacyReplacementCap    = uint64(1 << 15)
	hpetCounterSizeCap          = uint64(1 << 13)
)

func BootKernelToSerial(ctx context.Context, kernel []byte, memoryMB uint64, dmesg bool) (string, error) {
	return bootToCondition(ctx, kernel, nil, memoryMB, dmesg, func(serial string) bool {
		return serial != ""
	})
}

func BootInitramfsToMarker(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	return bootToCondition(ctx, kernel, initrd, memoryMB, dmesg, func(serial string) bool {
		return strings.Contains(serial, marker)
	})
}

func BootInitramfsToMarkerWithFS(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string, fsdevs []*virtio.FS) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	return bootToConditionWithDevices(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, nil, nil, func(serial string) bool {
		return strings.Contains(serial, marker)
	})
}

func BootInitramfsToMarkerWithFSAndNet(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string, fsdevs []*virtio.FS, netdev *virtio.Net) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	return bootToConditionWithDevices(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, nil, netdev, func(serial string) bool {
		return strings.Contains(serial, marker)
	})
}

func BootInitramfsToMarkerWithNVMeBlock(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, marker string, block *nvme.Controller) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	return bootToConditionWithNVMeBlock(ctx, kernel, initrd, memoryMB, dmesg, nil, nil, nil, block, func(serial string) bool {
		return strings.Contains(serial, marker)
	})
}

func bootToCondition(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, done func(string) bool) (string, error) {
	return bootToConditionWithDevices(ctx, kernel, initrd, memoryMB, dmesg, nil, nil, nil, done)
}

func bootToConditionWithDevices(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, vsock *virtio.Vsock, netdev *virtio.Net, done func(string) bool) (string, error) {
	return bootToConditionWithNVMeBlock(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, vsock, netdev, nil, done)
}

func bootToConditionWithNVMeBlock(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, vsock *virtio.Vsock, netdev *virtio.Net, nvmeBlock *nvme.Controller, done func(string) bool) (string, error) {
	vm, err := NewVM()
	if err != nil {
		return "", err
	}
	defer vm.Close()
	defer closeFSDevices(fsdevs)

	mem, err := mapAMD64GuestMemory(vm, memoryMB)
	if err != nil {
		return "", fmt.Errorf("map guest memory: %w", err)
	}

	var serialOut bytes.Buffer
	uart := newAMD64UART(vm, &serialOut)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Attach(vm, vm)
		}
	}
	if vsock != nil {
		vsock.Attach(vm, vm)
	}
	if netdev != nil {
		netdev.Attach(vm, vm)
	}
	var pci *PCIBus
	if nvmeBlock != nil {
		nvmeBlock.Attach(vm, vm)
		pci = NewPCIBus(NewNVMePCIDevice(1, 0xfeb00000, 10, nvmeBlock))
	}
	rng := virtio.NewRNG(amd64vm.RNGBase, amd64vm.RNGSize, amd64vm.RNGIRQ)
	rng.Attach(vm, vm)

	extraCmdline := amd64vm.VirtioFSCommandLineArgs(fsdevs)
	if vsock != nil {
		extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(vsock.Base, vsock.IRQ))
	}
	if netdev != nil {
		extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(netdev.Base, netdev.IRQ))
	}
	if nvmeBlock != nil {
		extraCmdline = append(extraCmdline, "acpi=off", "pci=conf1")
	}
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(rng.Base, rng.IRQ))
	if nvmeBlock == nil {
		extraCmdline = append(extraCmdline, linuxKVMHostKernelArgs()...)
	}
	plan, err := amd64vm.PrepareBoot(mem, kernel, initrd, amd64vm.BootConfig{
		MemoryMB:     memoryMB,
		Dmesg:        dmesg,
		ExtraCmdline: extraCmdline,
	})
	if err != nil {
		return "", fmt.Errorf("prepare boot: %w", err)
	}
	if err := vm.SetLongMode(plan.EntryGPA, plan.ZeroPageGPA, plan.StackTopGPA, plan.PagingBase); err != nil {
		return "", fmt.Errorf("set long mode: %w", err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	vm.SetVCPUTID(0, unix.Gettid())
	defer vm.SetVCPUTID(0, 0)
	cancelDone := make(chan struct{})
	defer close(cancelDone)
	go func() {
		select {
		case <-ctx.Done():
			vm.RequestImmediateExit()
		case <-cancelDone:
		}
	}()

	var exit Exit
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			if done(serialOut.String()) {
				return serialOut.String(), nil
			}
			return serialOut.String(), err
		}
		if err := vm.RunVCPUInterruptible(0, &exit); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return serialOut.String(), fmt.Errorf("run step %d: %w", step, err)
		}
		if done(serialOut.String()) {
			return serialOut.String(), nil
		}
		switch exit.Reason {
		case ExitIO:
			if err := handleBootIOWithPCI(func(ioExit IOExit) error {
				return handleBootIO(uart, ioExit)
			}, pci, exit.IO); err != nil {
				return serialOut.String(), err
			}
		case ExitMMIO:
			if err := handleBootMMIOWithPCI(vm, 0, pci, fsdevs, vsock, rng, nil, netdev, exit.MMIO); err != nil {
				return serialOut.String(), err
			}
		case ExitHLT, ExitShutdown:
			return serialOut.String(), fmt.Errorf("guest shut down before serial output")
		case ExitSystemEvent:
			return serialOut.String(), fmt.Errorf("unexpected system event %d before serial output", exit.SystemEvent)
		default:
			pc, _ := vm.GetPC()
			return serialOut.String(), fmt.Errorf("unexpected exit reason %d at pc=%#x", exit.Reason, pc)
		}
		if done(serialOut.String()) {
			return serialOut.String(), nil
		}
	}
}

func newAMD64UART(irq virtio.IRQController, out io.Writer) *serial.UART8250 {
	uart := serial.NewUART8250(amd64vm.COM1Base, 0, out)
	uart.AttachIRQ(irq, amd64vm.COM1IRQ)
	return uart
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

func handleBootIO(uart *serial.UART8250, ioExit IOExit) error {
	if ioExit.Port >= amd64vm.COM1Base && ioExit.Port < amd64vm.COM1Base+8 {
		return handleUARTIO(uart, ioExit)
	}
	return handleDefaultIO(ioExit)
}

func handleUARTIO(uart *serial.UART8250, ioExit IOExit) error {
	if ioExit.Size == 0 || ioExit.Count == 0 {
		return nil
	}
	for i := uint32(0); i < ioExit.Count; i++ {
		off := uint64(i) * uint64(ioExit.Size)
		port := uint64(ioExit.Port)
		if ioExit.Write {
			if err := uart.Write(port, ioExit.Data[off:off+uint64(ioExit.Size)]); err != nil {
				return err
			}
			continue
		}
		value, err := uart.ReadValue(port, int(ioExit.Size))
		if err != nil {
			return err
		}
		switch ioExit.Size {
		case 1:
			ioExit.Data[off] = byte(value)
		case 2:
			ioExit.Data[off] = byte(value)
			ioExit.Data[off+1] = byte(value >> 8)
		default:
			for j := uint8(0); j < ioExit.Size; j++ {
				ioExit.Data[off+uint64(j)] = byte(value >> (8 * j))
			}
		}
	}
	return nil
}

func handleBootMMIO(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, mmio MMIOExit) error {
	return handleBootMMIOForVCPU(vm, 0, fsdevs, vsock, rng, balloon, netdev, mmio)
}

func handleBootMMIOForVCPU(vm *VM, vcpuIndex int, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, mmio MMIOExit) error {
	return handleBootMMIOForVCPUWithExtra(vm, vcpuIndex, fsdevs, vsock, rng, balloon, netdev, nil, mmio)
}

func handleBootMMIOForVCPUWithExtra(vm *VM, vcpuIndex int, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, extra []virtio.MMIODevice, mmio MMIOExit) error {
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		if handled, err := handleBootMMIODevice(vm, vcpuIndex, fsdev, mmio); handled {
			return err
		}
	}
	if vsock != nil {
		if handled, err := handleBootMMIODevice(vm, vcpuIndex, vsock, mmio); handled {
			return err
		}
	}
	if rng != nil {
		if handled, err := handleBootMMIODevice(vm, vcpuIndex, rng, mmio); handled {
			return err
		}
	}
	if balloon != nil {
		if handled, err := handleBootMMIODevice(vm, vcpuIndex, balloon, mmio); handled {
			return err
		}
	}
	if netdev != nil {
		if handled, err := handleBootMMIODevice(vm, vcpuIndex, netdev, mmio); handled {
			return err
		}
	}
	for _, device := range extra {
		if handled, err := handleBootMMIODevice(vm, vcpuIndex, device, mmio); handled {
			return err
		}
	}
	if offset, ok := bootHPETOffset(mmio.Addr, mmio.Len); ok {
		if mmio.Write {
			return nil
		}
		vm.CompleteVCPUMMIORead(vcpuIndex, readBootHPET(offset), mmio.Len)
		return nil
	}
	return fmt.Errorf("unhandled mmio addr=%#x len=%d write=%v", mmio.Addr, mmio.Len, mmio.Write)
}

func handleBootMMIODevice(vm *VM, vcpuIndex int, device virtio.MMIODevice, mmio MMIOExit) (bool, error) {
	if device == nil || !device.Contains(mmio.Addr, int(mmio.Len)) {
		return false, nil
	}
	if mmio.Write {
		return true, device.Write(mmio.Addr, int(mmio.Len), mmioValue(mmio))
	}
	value, err := device.Read(mmio.Addr, int(mmio.Len))
	if err != nil {
		return true, err
	}
	vm.CompleteVCPUMMIORead(vcpuIndex, value, mmio.Len)
	return true, nil
}

func bootHPETOffset(addr uint64, size uint32) (uint64, bool) {
	if size == 0 {
		return 0, false
	}
	for _, base := range [...]uint64{hpetBaseAddress, hpetNetBSDBaseAddress, hpetAlternateBaseAddress} {
		end := base + hpetMMIOWindowSize
		if addr >= base && addr+uint64(size) <= end {
			return addr - base, true
		}
	}
	return 0, false
}

func readBootHPET(offset uint64) uint64 {
	switch offset {
	case hpetRegGeneralCapabilities:
		return uint64(hpetClockPeriodFemtoseconds)<<32 |
			uint64(hpetVendorID)<<16 |
			hpetCounterSizeCap |
			(hpetNumTimers - 1) |
			hpetLegacyReplacementCap
	case hpetRegGeneralConfiguration, hpetRegInterruptStatus, hpetRegMainCounter:
		return 0
	default:
		return 0
	}
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

func handleDefaultIO(ioExit IOExit) error {
	if ioExit.Write {
		return nil
	}
	fill := byte(0)
	switch {
	case ioExit.Port == 0xcfc || ioExit.Port == 0xcfd || ioExit.Port == 0xcfe || ioExit.Port == 0xcff:
		// No PCI devices are exposed during minimal boot. A config read of all
		// ones tells Linux the probed bus/device/function is absent.
		fill = 0xff
	case ioExit.Port == 0xcf8:
		fill = 0
	case ioExit.Port == 0x61:
		fill = 0
	case ioExit.Port == 0x70 || ioExit.Port == 0x71:
		fill = 0
	case ioExit.Port == 0x60 || ioExit.Port == 0x64:
		fill = 0
	default:
		fill = 0
	}
	for i := range ioExit.Data {
		ioExit.Data[i] = fill
	}
	return nil
}
