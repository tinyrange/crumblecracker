//go:build linux && arm64

package kvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/fdt"
	bootarm64 "github.com/tinyrange/crumblecracker/internal/core/linux/boot/arm64"
	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func RunManagedExecWithFS(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, req client.ExecRequest) (client.ExecResponse, string, error) {
	if len(req.Command) == 0 {
		return client.ExecResponse{}, "", fmt.Errorf("exec command is required")
	}

	backend := virtio.NewSimpleVsockBackend()
	listener, err := backend.Listen(vmruntime.ControlPort)
	if err != nil {
		return client.ExecResponse{}, "", fmt.Errorf("listen vsock control: %w", err)
	}
	defer listener.Close()

	vsock := virtio.NewVsock(arm64vm.VsockBase, arm64vm.VsockSize, arm64vm.VsockIRQ, vmruntime.GuestCID, backend)
	defer vsock.Close()
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)

	connCh := make(chan virtio.VsockConn, 1)
	acceptErrCh := make(chan error, 1)
	controlTranscript := vmruntime.NewSerialTranscript()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		connCh <- conn
		_, _ = io.Copy(controlTranscript, conn)
	}()

	nodes := []fdt.Node{vsock.DeviceTreeNode(), rng.DeviceTreeNode()}
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			nodes = append(nodes, fsdev.DeviceTreeNode())
		}
	}

	vm, err := NewVM()
	if err != nil {
		return client.ExecResponse{}, "", err
	}
	defer vm.Close()
	defer closeFSDevices(fsdevs)

	mem, err := vm.MapAnonymousMemory(arm64vm.MemorySizeBytes(memoryMB), arm64vm.MemoryBase)
	if err != nil {
		return client.ExecResponse{}, "", fmt.Errorf("map guest memory: %w", err)
	}

	serialOut := vmruntime.NewSerialTranscript()
	uart := serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, serialOut)
	uart.AttachIRQ(vm, arm64vm.UARTSPI)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Attach(vm, vm)
		}
	}
	vsock.Attach(vm, vm)
	rng.Attach(vm, vm)

	plan, err := arm64vm.PrepareBoot(mem, kernel, initrd, arm64vm.BootConfig{
		MemoryMB:   memoryMB,
		GICVersion: arm64vm.GICVersionV2,
		Dmesg:      dmesg,
		ExtraNodes: nodes,
	})
	if err != nil {
		return client.ExecResponse{}, serialOut.String(), fmt.Errorf("prepare boot: %w", err)
	}
	if err := setupBootRegisters(vm, plan); err != nil {
		return client.ExecResponse{}, serialOut.String(), err
	}

	const execID = "1"
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var runErrMu sync.Mutex
	var runErr error
	setRunErr := func(err error) {
		if err == nil {
			return
		}
		runErrMu.Lock()
		if runErr == nil {
			runErr = err
		}
		runErrMu.Unlock()
		cancel()
	}
	currentRunErr := func() error {
		runErrMu.Lock()
		defer runErrMu.Unlock()
		return runErr
	}
	withTranscripts := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%w\nserial:\n%s\ncontrol:\n%s", err, serialOut.String(), controlTranscript.String())
	}

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		setRunErr(runManagedExecVM(runCtx, vm, uart, fsdevs, vsock, rng, serialOut))
	}()
	defer func() {
		cancel()
		_ = vm.CancelRun()
		<-runDone
	}()
	go func() {
		text, err := serialOut.WaitFor(runCtx, 0, vmruntime.HasFatalBootText)
		if err == nil {
			setRunErr(fmt.Errorf("guest reported boot failure\nserial:\n%s\ncontrol:\n%s", text, controlTranscript.String()))
		}
	}()

	var control virtio.VsockConn
	select {
	case err := <-acceptErrCh:
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
	case conn := <-connCh:
		control = conn
		defer control.Close()
	case <-runCtx.Done():
		err := currentRunErr()
		if err == nil {
			err = runCtx.Err()
		}
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
	}

	if _, err := controlTranscript.WaitFor(runCtx, 0, func(text string) bool {
		return strings.Contains(text, vmruntime.InstanceReadyMarker) || vmruntime.HasFatalBootText(text)
	}); err != nil {
		if runErr := currentRunErr(); runErr != nil {
			return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(runErr)
		}
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
	}
	if vmruntime.HasFatalBootText(controlTranscript.String()) {
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(fmt.Errorf("guest reported boot failure"))
	}
	if err := managedagent.SendExec(control, execID, req); err != nil {
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
	}
	segment, err := controlTranscript.WaitForCommand(runCtx, 0, execID, func(text string) bool {
		_, _, _, ok := vmruntime.ExtractManagedExecResult(text, execID, dmesg)
		return ok
	})
	if err != nil {
		if runErr := currentRunErr(); runErr != nil {
			return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(runErr)
		}
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
	}
	code, output, usage, ok := vmruntime.ExtractManagedExecResult(segment, execID, dmesg)
	if !ok {
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(fmt.Errorf("exec did not produce a complete result"))
	}
	if dmesg {
		output = serialOut.String() + "\n[control]\n" + output
	}
	cancel()
	return client.ExecResponse{ExitCode: code, Output: output, Usage: usage}, serialOut.String(), nil
}

func runManagedExecVM(ctx context.Context, vm *VM, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, serialOut *vmruntime.SerialTranscript) error {
	return runManagedExecVMWithSnapshot(ctx, vm, uart, fsdevs, vsock, rng, nil, serialOut, nil)
}

func runManagedExecVMWithSnapshot(ctx context.Context, vm *VM, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, extra []virtio.MMIODevice, serialOut *vmruntime.SerialTranscript, snapshot *snapshotTrigger) error {
	runtime.LockOSThread()
	// This loop always runs in a dedicated goroutine. Leave it locked so the
	// OS thread terminates with the goroutine instead of remaining parked.
	vm.SetVCPUTID(unix.Gettid())
	defer vm.SetVCPUTID(0)
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
			return err
		}
		if err := vm.RunInterruptible(&exit); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("run step %d: %w", step, err)
		}
		switch exit.Reason {
		case ExitMMIO:
			handled, err := snapshot.handleMMIO(vm, exit.MMIO)
			if err != nil {
				return err
			}
			if !handled {
				err = handleBootMMIOWithExtra(vm, uart, fsdevs, vsock, rng, extra, exit.MMIO)
			}
			if err != nil {
				return err
			}
			if err := snapshot.captureIfPending(vm, fsdevs, vsock, rng); err != nil {
				return err
			}
		case ExitShutdown:
			return fmt.Errorf("guest shut down before exec completed\nserial:\n%s", serialOut.String())
		case ExitSystemEvent:
			return fmt.Errorf("unexpected system event %d before exec completed\nserial:\n%s", exit.SystemEvent, serialOut.String())
		default:
			pc, _ := vm.GetPC()
			return fmt.Errorf("unexpected exit reason %d at pc=%#x\nserial:\n%s", exit.Reason, pc, serialOut.String())
		}
	}
}

func setupBootRegisters(vm *VM, plan *bootarm64.BootPlan) error {
	if err := vm.SetPC(plan.EntryGPA); err != nil {
		return fmt.Errorf("set PC: %w", err)
	}
	if err := vm.SetPState(arm64vm.DefaultPStateBits); err != nil {
		return fmt.Errorf("set PSTATE: %w", err)
	}
	if err := vm.SetSpEl1(plan.StackTopGPA); err != nil {
		return fmt.Errorf("set SP_EL1: %w", err)
	}
	if err := vm.SetX(0, plan.DeviceTreeGPA); err != nil {
		return fmt.Errorf("set X0: %w", err)
	}
	for reg := 1; reg <= 3; reg++ {
		if err := vm.SetX(reg, 0); err != nil {
			return fmt.Errorf("set X%d: %w", reg, err)
		}
	}
	return nil
}
