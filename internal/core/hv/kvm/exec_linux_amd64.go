//go:build linux && amd64

package kvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	"golang.org/x/sys/unix"
)

func RunManagedExecWithFS(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, req client.ExecRequest) (client.ExecResponse, string, error) {
	return RunManagedExecWithFSAndNet(ctx, kernel, initrd, memoryMB, cpus, dmesg, fsdevs, nil, req)
}

func RunManagedExecWithFSAndNet(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, req client.ExecRequest) (client.ExecResponse, string, error) {
	return RunManagedExecWithFSNetAndBalloon(ctx, kernel, initrd, memoryMB, 0, cpus, dmesg, fsdevs, netdev, req)
}

func RunManagedExecWithFSNetAndBalloon(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, balloonMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, req client.ExecRequest) (client.ExecResponse, string, error) {
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
	rng := virtio.NewRNG(amd64vm.RNGBase, amd64vm.RNGSize, amd64vm.RNGIRQ)
	balloon := virtio.NewBalloon(amd64vm.BalloonBase, amd64vm.BalloonSize, amd64vm.BalloonIRQ)
	if targetPages := balloonTargetPages(balloonMB); targetPages != 0 {
		if err := balloon.SetTargetPages(targetPages); err != nil {
			return client.ExecResponse{}, "", fmt.Errorf("set balloon target: %w", err)
		}
	}

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

	vm, err := NewVMWithCPUs(cpus)
	if err != nil {
		return client.ExecResponse{}, "", err
	}
	defer vm.Close()
	defer closeFSDevices(fsdevs)

	mem, err := mapAMD64GuestMemory(vm, memoryMB)
	if err != nil {
		return client.ExecResponse{}, "", fmt.Errorf("map guest memory: %w", err)
	}

	serialOut := vmruntime.NewSerialTranscript()
	uart := newAMD64UART(vm, serialOut)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Attach(vm, vm)
		}
	}
	vsock.Attach(vm, vm)
	rng.Attach(vm, vm)
	balloon.Attach(vm, vm)
	if netdev != nil {
		netdev.Attach(vm, vm)
	}

	extraCmdline := amd64vm.VirtioFSCommandLineArgs(fsdevs)
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(vsock.Base, vsock.IRQ))
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(rng.Base, rng.IRQ))
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(balloon.Base, balloon.IRQ))
	if netdev != nil {
		extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(netdev.Base, netdev.IRQ))
	}
	extraCmdline = append(extraCmdline, linuxKVMHostKernelArgs()...)
	plan, err := amd64vm.PrepareBoot(mem, kernel, initrd, amd64vm.BootConfig{
		MemoryMB:     memoryMB,
		NumCPUs:      cpus,
		Dmesg:        dmesg,
		ExtraCmdline: extraCmdline,
	})
	if err != nil {
		return client.ExecResponse{}, serialOut.String(), fmt.Errorf("prepare boot: %w", err)
	}
	if err := vm.SetLongMode(plan.EntryGPA, plan.ZeroPageGPA, plan.StackTopGPA, plan.PagingBase); err != nil {
		return client.ExecResponse{}, serialOut.String(), fmt.Errorf("set long mode: %w", err)
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
		setRunErr(runManagedExecVM(runCtx, vm, uart, fsdevs, vsock, rng, balloon, netdev, serialOut))
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

func runManagedExecVM(ctx context.Context, vm *VM, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, serialOut *vmruntime.SerialTranscript) error {
	return runManagedExecVMWithSnapshot(ctx, vm, uart, fsdevs, vsock, rng, balloon, netdev, nil, serialOut, nil)
}

func runManagedExecVMWithSnapshot(ctx context.Context, vm *VM, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, extra []virtio.MMIODevice, serialOut *vmruntime.SerialTranscript, snapshot *snapshotTrigger) error {
	if vm != nil && len(vm.vcpus) > 1 {
		if snapshot != nil {
			return fmt.Errorf("KVM startup snapshots currently support only one vCPU")
		}
		return runManagedExecVMMulti(ctx, vm, uart, fsdevs, vsock, rng, balloon, netdev, extra, serialOut)
	}
	runtime.LockOSThread()
	// This loop always runs in a dedicated goroutine. Leave it locked so the
	// Go runtime terminates the OS thread when that goroutine exits instead of
	// retaining one parked thread for every VM that has ever run.
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
	acpiPM := NewACPIPM()
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := vm.RunVCPUInterruptible(0, &exit); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("run step %d: %w", step, err)
		}
		switch exit.Reason {
		case ExitIO:
			if handled, err := acpiPM.HandleIO(exit.IO); handled {
				if errors.Is(err, errGuestPoweroff) {
					return nil
				}
				if err != nil {
					return err
				}
				break
			}
			if err := handleBootIO(uart, exit.IO); err != nil {
				return err
			}
		case ExitMMIO:
			if handled, err := snapshot.handleMMIO(vm, balloon, exit.MMIO); err != nil {
				return err
			} else if handled {
				break
			}
			if err := handleBootMMIOForVCPUWithExtra(vm, 0, fsdevs, vsock, rng, balloon, netdev, extra, exit.MMIO); err != nil {
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
		if err := snapshot.captureIfPending(vm, fsdevs, vsock, rng, balloon, netdev); err != nil {
			return err
		}
	}
}

func runManagedExecVMMulti(ctx context.Context, vm *VM, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, extra []virtio.MMIODevice, serialOut *vmruntime.SerialTranscript) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sampler := startKVMRegisterSampler(runCtx, vm)
	defer sampler.Close()
	errCh := make(chan error, len(vm.vcpus))
	var wg sync.WaitGroup
	var exitMu sync.Mutex
	acpiPM := NewACPIPM()
	for index := range vm.vcpus {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			runtime.LockOSThread()
			// Terminate this dedicated vCPU thread with its goroutine rather than
			// returning it to Go's idle thread pool.
			vm.SetVCPUTID(index, unix.Gettid())
			var exit Exit
			for {
				if err := runCtx.Err(); err != nil {
					return
				}
				err := vm.RunVCPUInterruptible(index, &exit)
				if err != nil {
					if errors.Is(err, unix.EINTR) {
						if sampler.Enabled() {
							sampler.Record(vm.VCPURegisters(index))
						}
						continue
					}
					reportRunErr(errCh, cancel, fmt.Errorf("run vcpu %d: %w", index, err))
					return
				}
				exitMu.Lock()
				err = handleManagedExit(vm, index, uart, fsdevs, vsock, rng, balloon, netdev, extra, acpiPM, serialOut, exit)
				exitMu.Unlock()
				if err != nil {
					if errors.Is(err, errGuestPoweroff) {
						reportRunErr(errCh, cancel, nil)
						return
					}
					reportRunErr(errCh, cancel, err)
					return
				}
				if exit.Reason == ExitHLT {
					time.Sleep(100 * time.Microsecond)
				}
			}
		}(index)
	}
	defer func() {
		cancel()
		vm.RequestImmediateExit()
		wg.Wait()
	}()

	select {
	case err := <-errCh:
		return err
	case <-runCtx.Done():
		return runCtx.Err()
	}
}

type kvmRegisterSampler struct {
	file *os.File
	mu   sync.Mutex
}

func startKVMRegisterSampler(ctx context.Context, vm *VM) *kvmRegisterSampler {
	path := strings.TrimSpace(os.Getenv("CCX3_KVM_REGISTER_SAMPLE"))
	if path == "" || vm == nil {
		return &kvmRegisterSampler{}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccx3: open KVM register sample file %q: %v\n", path, err)
		return &kvmRegisterSampler{}
	}
	s := &kvmRegisterSampler{file: file}
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				vm.RequestImmediateExit()
			}
		}
	}()
	return s
}

func (s *kvmRegisterSampler) Enabled() bool {
	return s != nil && s.file != nil
}

func (s *kvmRegisterSampler) Record(regs map[string]any) {
	if !s.Enabled() {
		return
	}
	payload := map[string]any{
		"time": time.Now().Format(time.RFC3339Nano),
		"regs": regs,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.file.Write(append(data, '\n'))
}

func (s *kvmRegisterSampler) Close() {
	if s == nil || s.file == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.file.Close()
	s.file = nil
}

func reportRunErr(errCh chan<- error, cancel context.CancelFunc, err error) {
	cancel()
	select {
	case errCh <- err:
	default:
	}
}

func handleManagedExit(vm *VM, vcpuIndex int, uart *serial.UART8250, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, extra []virtio.MMIODevice, acpiPM *ACPIPM, serialOut *vmruntime.SerialTranscript, exit Exit) error {
	switch exit.Reason {
	case ExitIO:
		if handled, err := acpiPM.HandleIO(exit.IO); handled {
			return err
		}
		if err := handleBootIO(uart, exit.IO); err != nil {
			return err
		}
	case ExitMMIO:
		if err := handleBootMMIOForVCPUWithExtra(vm, vcpuIndex, fsdevs, vsock, rng, balloon, netdev, extra, exit.MMIO); err != nil {
			return err
		}
	case ExitHLT:
		return nil
	case ExitShutdown:
		return fmt.Errorf("guest shut down before exec completed\nserial:\n%s", serialOut.String())
	case ExitSystemEvent:
		return fmt.Errorf("unexpected system event %d before exec completed\nserial:\n%s", exit.SystemEvent, serialOut.String())
	default:
		pc, _ := vm.GetVCPUPC(vcpuIndex)
		return fmt.Errorf("unexpected exit reason %d on vcpu %d at pc=%#x\nserial:\n%s", exit.Reason, vcpuIndex, pc, serialOut.String())
	}
	return nil
}
