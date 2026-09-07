//go:build windows && amd64

package whp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func RunManagedExecWithFS(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, req client.ExecRequest) (client.ExecResponse, string, error) {
	return RunManagedExecWithFSAndNet(ctx, kernel, initrd, memoryMB, cpus, dmesg, fsdevs, nil, req)
}

func RunManagedExecWithFSAndNet(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, req client.ExecRequest) (client.ExecResponse, string, error) {
	if len(req.Command) == 0 {
		return client.ExecResponse{}, "", fmt.Errorf("exec command is required")
	}

	backend := virtio.NewSimpleVsockBackend()
	listener, err := backend.Listen(vmruntime.ControlPort)
	if err != nil {
		return client.ExecResponse{}, "", fmt.Errorf("listen vsock control: %w", err)
	}
	defer listener.Close()

	vsock := virtio.NewVsock(amd64vm.VsockBase, amd64vm.VsockSize, amd64vm.VsockIRQ, vmruntime.GuestCID, backend)
	defer vsock.Close()

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

	vm, platform, serialOut, err := prepareManagedVM(kernel, initrd, memoryMB, cpus, dmesg, fsdevs, vsock, netdev, nil, "", nil)
	if err != nil {
		return client.ExecResponse{}, "", err
	}
	defer vm.Close()
	defer platform.Close()

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
		return transcriptError(err, serialOut.String(), controlTranscript.String())
	}

	go func() {
		setRunErr(runManagedExecVM(runCtx, vm, platform, serialOut))
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
			err = fmt.Errorf("%w (%s)", runCtx.Err(), platform.Summary())
		}
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
	}

	if _, err := controlTranscript.WaitFor(runCtx, 0, func(text string) bool {
		return strings.Contains(text, vmruntime.InstanceReadyMarker) || vmruntime.HasFatalBootText(text)
	}); err != nil {
		if runErr := currentRunErr(); runErr != nil {
			return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(runErr)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w (%s)", err, platform.Summary())
		}
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
	}
	if vmruntime.HasFatalBootText(controlTranscript.String()) {
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(fmt.Errorf("guest reported boot failure"))
	}
	if err := onlineManagedVCPUs(runCtx, control, controlTranscript, cpus); err != nil {
		return client.ExecResponse{Output: serialOut.String()}, serialOut.String(), withTranscripts(err)
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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w (%s)", err, platform.Summary())
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

func onlineManagedVCPUs(ctx context.Context, control io.Writer, transcript *vmruntime.SerialTranscript, cpus int) error {
	if cpus <= 1 {
		return nil
	}
	for cpu := 1; cpu < cpus; cpu++ {
		id := fmt.Sprintf("whp-online-%d", cpu)
		start := transcript.Len()
		releaseTranscript := transcript.RetainFrom(start)
		err := managedagent.SendExec(control, id, client.ExecRequest{
			Command: []string{
				"/bin/sh",
				"-c",
				fmt.Sprintf("printf '1\\n' > /sys/devices/system/cpu/cpu%d/online", cpu),
			},
		})
		if err != nil {
			releaseTranscript()
			return fmt.Errorf("online vCPU %d: %w", cpu, err)
		}
		segment, err := transcript.WaitForCommand(ctx, start, id, func(text string) bool {
			_, _, _, ok := vmruntime.ExtractManagedExecResult(text, id, false)
			return ok
		})
		releaseTranscript()
		if err != nil {
			return fmt.Errorf("online vCPU %d: %w", cpu, err)
		}
		code, output, _, ok := vmruntime.ExtractManagedExecResult(segment, id, false)
		if !ok {
			return fmt.Errorf("online vCPU %d: guest did not produce a complete result", cpu)
		}
		if code != 0 {
			return fmt.Errorf("online vCPU %d: guest exited with status %d: %s", cpu, code, strings.TrimSpace(output))
		}
	}

	const id = "whp-online-check"
	start := transcript.Len()
	releaseTranscript := transcript.RetainFrom(start)
	defer releaseTranscript()
	if err := managedagent.SendExec(control, id, client.ExecRequest{
		Command: []string{"cat", "/sys/devices/system/cpu/online"},
	}); err != nil {
		return fmt.Errorf("verify online vCPUs: %w", err)
	}
	segment, err := transcript.WaitForCommand(ctx, start, id, func(text string) bool {
		_, _, _, ok := vmruntime.ExtractManagedExecResult(text, id, false)
		return ok
	})
	if err != nil {
		return fmt.Errorf("verify online vCPUs: %w", err)
	}
	code, output, _, ok := vmruntime.ExtractManagedExecResult(segment, id, false)
	if !ok {
		return fmt.Errorf("verify online vCPUs: guest did not produce a complete result")
	}
	if code != 0 {
		return fmt.Errorf("verify online vCPUs: guest exited with status %d: %s", code, strings.TrimSpace(output))
	}
	want := fmt.Sprintf("0-%d", cpus-1)
	if got := strings.TrimSpace(output); got != want {
		return fmt.Errorf("verify online vCPUs: got %q, want %q", got, want)
	}
	return nil
}

func prepareManagedVM(kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, vsock *virtio.Vsock, netdev *virtio.Net, displayDevices []virtio.MMIODevice, snapshotDir string, serialWriter io.Writer) (*VM, *bootPlatform, *vmruntime.SerialTranscript, error) {
	if cpus <= 0 {
		cpus = 1
	}
	vm, err := newBootVM(amd64vm.MemorySizeBytes(memoryMB), cpus)
	if err != nil {
		return nil, nil, nil, err
	}

	extraCmdline := []string{
		"tsc=reliable",
		"tsc_early_khz=3000000",
		"lpj=10000000",
		"no_timer_check",
	}
	if cpus > 1 {
		extraCmdline = append(extraCmdline, "maxcpus=1")
	}
	extraCmdline = append(extraCmdline, amd64vm.VirtioFSCommandLineArgs(fsdevs)...)
	if vsock != nil {
		extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(vsock.Base, vsock.IRQ))
	}
	if netdev != nil {
		extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(netdev.Base, netdev.IRQ))
	}
	if len(displayDevices) != 0 {
		extraCmdline = append(extraCmdline,
			amd64vm.VirtioMMIODeviceArg(amd64vm.GPUBase, amd64vm.GPUIRQ),
			amd64vm.VirtioMMIODeviceArg(amd64vm.KeyboardBase, amd64vm.KeyboardIRQ),
			amd64vm.VirtioMMIODeviceArg(amd64vm.PointerBase, amd64vm.PointerIRQ),
		)
	}
	rng := virtio.NewRNG(amd64vm.RNGBase, amd64vm.RNGSize, amd64vm.RNGIRQ)
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(rng.Base, rng.IRQ))
	plan, err := amd64vm.PrepareBoot(vm.Memory(), kernel, initrd, amd64vm.BootConfig{
		MemoryMB:     memoryMB,
		NumCPUs:      cpus,
		Dmesg:        dmesg,
		ExtraCmdline: extraCmdline,
	})
	if err != nil {
		_ = vm.Close()
		return nil, nil, nil, fmt.Errorf("prepare boot: %w", err)
	}
	if err := installBootACPIForZeroPage(vm.Memory(), plan.ZeroPageGPA, cpus); err != nil {
		_ = vm.Close()
		return nil, nil, nil, fmt.Errorf("install acpi: %w", err)
	}
	if err := vm.SetLongMode(plan.EntryGPA, plan.ZeroPageGPA, plan.StackTopGPA, plan.PagingBase); err != nil {
		_ = vm.Close()
		return nil, nil, nil, fmt.Errorf("set long mode: %w", err)
	}

	serialOut := vmruntime.NewSerialTranscript()
	if serialWriter == nil {
		serialWriter = serialOut
	} else {
		serialWriter = io.MultiWriter(serialOut, serialWriter)
	}
	snapshot := newSnapshotTrigger(snapshotDir, vm.Memory())
	serialWriter = snapshot.wrapSerialWriter(serialWriter)
	platform := newBootPlatform(vm, serial.NewUART8250(amd64vm.COM1Base, 0, serialWriter))
	platform.snapshot = snapshot
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			platform.AttachFS(fsdev)
		}
	}
	if vsock != nil {
		platform.AttachVsock(vsock)
	}
	if netdev != nil {
		platform.AttachNet(netdev)
	}
	platform.AttachDisplayDevices(displayDevices)
	platform.AttachRNG(rng)
	if err := vm.EnableEmulation(platform); err != nil {
		platform.Close()
		_ = vm.Close()
		return nil, nil, serialOut, fmt.Errorf("enable emulation: %w", err)
	}
	return vm, platform, serialOut, nil
}

func runManagedExecVM(ctx context.Context, vm *VM, platform *bootPlatform, serialOut *vmruntime.SerialTranscript) error {
	if vm != nil && vm.vcpuCount > 1 {
		if platform != nil && platform.snapshot != nil {
			return fmt.Errorf("WHP startup snapshots currently support only one vCPU")
		}
		return runManagedExecVMMulti(ctx, vm, platform, serialOut)
	}
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w (%s)", err, platform.Summary())
		}
		if err := platform.armPendingIRQWindow(); err != nil {
			return fmt.Errorf("arm pending irq window: %w", err)
		}
		var raw runVPExitContext
		exit, err := vm.runWithCancel(ctx, &raw)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w (%s)", ctx.Err(), platform.Summary())
			}
			return fmt.Errorf("run step %d: %w", step, err)
		}
		platform.recordExit(exit, &raw)
		switch exit.Reason {
		case runVPExitReasonX64IoPortAccess:
			if err := vm.emulateIO(&raw); err != nil {
				if errors.Is(err, errGuestPoweroff) {
					return nil
				}
				io := raw.ioPortAccess()
				return fmt.Errorf("emulate io at rip=%#x port=%#x: %w", exit.RIP, io.Port, err)
			}
			if err := platform.captureSnapshotIfPending(vm); err != nil {
				return err
			}
		case runVPExitReasonMemoryAccess:
			if err := vm.emulateMMIO(&raw); err != nil {
				mem := raw.memoryAccess()
				return fmt.Errorf("emulate mmio at rip=%#x gpa=%#x gva=%#x access=%d insn_len=%d insn=% x: %w", exit.RIP, uint64(mem.GPA), mem.GVA, mem.AccessInfo.accessType(), mem.InstructionByteCount, mem.InstructionBytes[:mem.InstructionByteCount], err)
			}
		case runVPExitReasonX64Halt:
			if !platform.hasPendingIRQ() {
				return fmt.Errorf("guest halted before exec completed\nserial:\n%s\n%s", serialOut.String(), platform.Summary())
			}
		case runVPExitReasonX64ApicEoi:
			platform.HandleEOI(raw.apicEoi().InterruptVector)
		case runVPExitReasonX64MsrAccess:
			if err := handleMSRAccess(vm, 0, exit, &raw); err != nil {
				return fmt.Errorf("handle msr at rip=%#x: %w", exit.RIP, err)
			}
		case runVPExitReasonX64InterruptWindow:
		case runVPExitReasonCanceled:
		default:
			return fmt.Errorf("unexpected exit %s at rip=%#x\nserial:\n%s\n%s", exit.Reason, exit.RIP, serialOut.String(), platform.Summary())
		}
		if flushed, err := platform.flushPendingIRQ(&raw); err != nil {
			return fmt.Errorf("flush pending irq after %s at rip=%#x: %w", exit.Reason, exit.RIP, err)
		} else if exit.Reason == runVPExitReasonX64Halt && !flushed {
			return fmt.Errorf("guest halted with pending irq blocked\nserial:\n%s\n%s", serialOut.String(), platform.Summary())
		}
	}
}

func runManagedExecVMMulti(ctx context.Context, vm *VM, platform *bootPlatform, serialOut *vmruntime.SerialTranscript) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, int(vm.vcpuCount))
	var wg sync.WaitGroup
	for index := uint32(0); index < vm.vcpuCount; index++ {
		wg.Add(1)
		go func(index uint32) {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			for step := 0; ; step++ {
				if runCtx.Err() != nil {
					return
				}
				err := platform.armPendingIRQWindowForVCPU(index, index == 0)
				if err != nil {
					reportWHPRunError(errCh, cancel, fmt.Errorf("arm pending irq window for vcpu %d: %w", index, err))
					return
				}

				var raw runVPExitContext
				exit, err := vm.runVCPUWithContext(index, &raw)
				if err != nil {
					reportWHPRunError(errCh, cancel, fmt.Errorf("run vcpu %d step %d: %w", index, step, err))
					return
				}
				if exit.Reason == runVPExitReasonCanceled && runCtx.Err() != nil {
					return
				}
				err = handleManagedWHPExit(vm, index, platform, serialOut, exit, &raw)
				if err == nil {
					_, err = platform.flushPendingIRQForVCPU(index, &raw)
					if err != nil {
						err = fmt.Errorf("flush pending irq after %s at rip=%#x: %w", exit.Reason, exit.RIP, err)
					}
				}
				if err != nil {
					if errors.Is(err, errGuestPoweroff) {
						reportWHPRunError(errCh, cancel, nil)
					} else {
						reportWHPRunError(errCh, cancel, err)
					}
					return
				}
				if exit.Reason == runVPExitReasonX64Halt {
					time.Sleep(100 * time.Microsecond)
				}
			}
		}(index)
	}
	defer func() {
		cancel()
		_ = vm.CancelRun()
		wg.Wait()
	}()

	wakeTicker := time.NewTicker(time.Millisecond)
	defer wakeTicker.Stop()
	for {
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-wakeTicker.C:
			if index, ok := platform.queuedIRQVCPU(); ok {
				vm.kickVCPUIfRunning(index)
			}
		}
	}
}

func handleManagedWHPExit(vm *VM, index uint32, platform *bootPlatform, serialOut *vmruntime.SerialTranscript, exit Exit, raw *runVPExitContext) error {
	platform.recordExit(exit, raw)
	switch exit.Reason {
	case runVPExitReasonX64IoPortAccess:
		if err := vm.emulateVCPUIO(index, raw); err != nil {
			io := raw.ioPortAccess()
			return fmt.Errorf("emulate vcpu %d io at rip=%#x port=%#x: %w", index, exit.RIP, io.Port, err)
		}
	case runVPExitReasonMemoryAccess:
		if err := vm.emulateVCPUMMIO(index, raw); err != nil {
			mem := raw.memoryAccess()
			return fmt.Errorf("emulate vcpu %d mmio at rip=%#x gpa=%#x gva=%#x access=%d insn_len=%d insn=% x: %w", index, exit.RIP, uint64(mem.GPA), mem.GVA, mem.AccessInfo.accessType(), mem.InstructionByteCount, mem.InstructionBytes[:mem.InstructionByteCount], err)
		}
	case runVPExitReasonX64Halt:
	case runVPExitReasonX64ApicEoi:
		platform.HandleEOI(raw.apicEoi().InterruptVector)
	case runVPExitReasonX64MsrAccess:
		if err := handleMSRAccess(vm, index, exit, raw); err != nil {
			return fmt.Errorf("handle vcpu %d msr at rip=%#x: %w", index, exit.RIP, err)
		}
	case runVPExitReasonX64InterruptWindow:
	case runVPExitReasonCanceled:
	default:
		return fmt.Errorf("unexpected exit %s on vcpu %d at rip=%#x\nserial:\n%s\n%s", exit.Reason, index, exit.RIP, serialOut.String(), platform.Summary())
	}
	return nil
}

func reportWHPRunError(errCh chan<- error, cancel context.CancelFunc, err error) {
	cancel()
	select {
	case errCh <- err:
	default:
	}
}
