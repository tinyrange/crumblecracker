//go:build linux && amd64

package kvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	"github.com/tinyrange/crumblecracker/internal/core/shmem"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type ManagedSession struct {
	cancel            context.CancelFunc
	done              *sessionDone
	control           io.ReadWriteCloser
	listener          io.Closer
	clipboardListener io.Closer
	displayListener   io.Closer
	vsock             *virtio.Vsock
	balloon           *virtio.Balloon
	desktop           *virtio.Desktop
	fsdevs            []*virtio.FS
	fsCloseErr        *error
	bootWriter        *vmruntime.BootEventWriter
	transcript        *vmruntime.SerialTranscript
	serialOut         *vmruntime.SerialTranscript
	cleanup           func()
	sendMu            sync.Mutex
	nextID            atomic.Uint64
	dmesg             bool
	inlineExec        bool
}

type ManagedSessionOptions struct {
	SnapshotDir     string
	RestoreSnapshot string
	BalloonMB       uint64
	DisplayWidth    uint32
	DisplayHeight   uint32
	SharedMemory    *shmem.Attachment
}

func StartManagedSession(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	return StartManagedSessionWithNet(ctx, kernel, initrd, memoryMB, cpus, dmesg, fsdevs, nil, onEvent)
}

func StartManagedSessionWithNet(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	return StartManagedSessionWithNetOptions(ctx, kernel, initrd, memoryMB, cpus, dmesg, fsdevs, netdev, ManagedSessionOptions{}, onEvent)
}

func StartManagedSessionWithNetOptions(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, cpus int, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, opts ManagedSessionOptions, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	if strings.TrimSpace(opts.SnapshotDir) != "" || strings.TrimSpace(opts.RestoreSnapshot) != "" {
		if opts.DisplayWidth != 0 || opts.DisplayHeight != 0 {
			return nil, fmt.Errorf("display-enabled VMs do not support startup snapshots")
		}
		if cpus > 1 {
			return nil, fmt.Errorf("KVM startup snapshots currently support only one vCPU")
		}
	}
	if snapshotPath := strings.TrimSpace(opts.RestoreSnapshot); snapshotPath != "" {
		return StartManagedSessionFromSnapshot(ctx, snapshotPath, memoryMB, opts.BalloonMB, dmesg, fsdevs, netdev, onEvent)
	}
	if err := emitManagedBootStatus(onEvent, "starting VM"); err != nil {
		return nil, err
	}
	backend := virtio.NewSimpleVsockBackend()
	listener, err := backend.Listen(vmruntime.ControlPort)
	if err != nil {
		return nil, fmt.Errorf("listen vsock control: %w", err)
	}

	vsock := virtio.NewVsock(amd64vm.VsockBase, amd64vm.VsockSize, amd64vm.VsockIRQ, vmruntime.GuestCID, backend)
	rng := virtio.NewRNG(amd64vm.RNGBase, amd64vm.RNGSize, amd64vm.RNGIRQ)
	balloon := virtio.NewBalloon(amd64vm.BalloonBase, amd64vm.BalloonSize, amd64vm.BalloonIRQ)
	var desktop *virtio.Desktop
	var displayDevices []virtio.MMIODevice
	var clipboardListener virtio.VsockListener
	var displayListener virtio.VsockListener
	if opts.DisplayWidth != 0 || opts.DisplayHeight != 0 {
		framebuffer, err := virtio.NewFramebuffer(int(opts.DisplayWidth), int(opts.DisplayHeight))
		if err != nil {
			_ = listener.Close()
			vsock.Close()
			return nil, fmt.Errorf("create display: %w", err)
		}
		gpu := virtio.NewGPU(amd64vm.GPUBase, amd64vm.GPUSize, amd64vm.GPUIRQ, framebuffer)
		keyboard := virtio.NewKeyboardInput(amd64vm.KeyboardBase, amd64vm.KeyboardSize, amd64vm.KeyboardIRQ)
		pointer := virtio.NewAbsolutePointerInput(amd64vm.PointerBase, amd64vm.PointerSize, amd64vm.PointerIRQ, opts.DisplayWidth, opts.DisplayHeight)
		clipboard := virtio.NewClipboard()
		clipboardListener, err = backend.Listen(vmruntime.ClipboardPort)
		if err != nil {
			_ = listener.Close()
			vsock.Close()
			return nil, fmt.Errorf("listen for guest clipboard bridge: %w", err)
		}
		desktop = &virtio.Desktop{Framebuffer: framebuffer, GPU: gpu, Keyboard: keyboard, Pointer: pointer, Clipboard: clipboard}
		displayListener, err = backend.Listen(vmruntime.DisplayPort)
		if err != nil {
			_ = clipboardListener.Close()
			_ = listener.Close()
			vsock.Close()
			return nil, fmt.Errorf("listen for guest display bridge: %w", err)
		}
		displayDevices = []virtio.MMIODevice{gpu, keyboard, pointer}
	}
	if targetPages := balloonTargetPages(opts.BalloonMB); targetPages != 0 {
		if err := balloon.SetTargetPages(targetPages); err != nil {
			_ = listener.Close()
			vsock.Close()
			return nil, fmt.Errorf("set balloon target: %w", err)
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
		_ = listener.Close()
		vsock.Close()
		return nil, err
	}
	mem, err := mapAMD64GuestMemory(vm, memoryMB)
	if err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		vsock.Close()
		return nil, fmt.Errorf("map guest memory: %w", err)
	}
	runtimeDevices := append([]virtio.MMIODevice(nil), displayDevices...)
	if opts.SharedMemory != nil {
		device, err := shmem.NewDevice(opts.SharedMemory.Config().PhysAddr, opts.SharedMemory, vm)
		if err != nil {
			closeVMWithFS(vm, fsdevs)
			_ = listener.Close()
			vsock.Close()
			return nil, err
		}
		opts.SharedMemory.Claim()
		runtimeDevices = append(runtimeDevices, device)
	}

	serialOut := vmruntime.NewSerialTranscript()
	var serialWriter io.Writer = serialOut
	var bootWriter *vmruntime.BootEventWriter
	if onEvent != nil {
		bootWriter = vmruntime.NewBootEventWriter(onEvent)
		serialWriter = io.MultiWriter(serialOut, bootWriter)
	}
	snapshot := newSnapshotTrigger(opts.SnapshotDir, mem)
	serialWriter = snapshot.wrapSerialWriter(serialWriter)
	uart := newAMD64UART(vm, serialWriter)
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
	for _, device := range displayDevices {
		switch typed := device.(type) {
		case *virtio.GPU:
			typed.Attach(vm, vm)
		case *virtio.Input:
			typed.Attach(vm, vm)
		}
	}

	extraCmdline := amd64vm.VirtioFSCommandLineArgs(fsdevs)
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(vsock.Base, vsock.IRQ))
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(rng.Base, rng.IRQ))
	extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(balloon.Base, balloon.IRQ))
	if netdev != nil {
		extraCmdline = append(extraCmdline, amd64vm.VirtioMMIODeviceArg(netdev.Base, netdev.IRQ))
	}
	if desktop != nil {
		extraCmdline = append(extraCmdline,
			amd64vm.VirtioMMIODeviceArg(amd64vm.GPUBase, amd64vm.GPUIRQ),
			amd64vm.VirtioMMIODeviceArg(amd64vm.KeyboardBase, amd64vm.KeyboardIRQ),
			amd64vm.VirtioMMIODeviceArg(amd64vm.PointerBase, amd64vm.PointerIRQ),
		)
	}
	extraCmdline = append(extraCmdline, linuxKVMHostKernelArgs()...)
	plan, err := amd64vm.PrepareBoot(mem, kernel, initrd, amd64vm.BootConfig{
		MemoryMB:     memoryMB,
		NumCPUs:      cpus,
		Dmesg:        dmesg,
		ExtraCmdline: extraCmdline,
	})
	if err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, fmt.Errorf("prepare boot: %w", err)
	}
	if err := vm.SetLongMode(plan.EntryGPA, plan.ZeroPageGPA, plan.StackTopGPA, plan.PagingBase); err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, fmt.Errorf("set long mode: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	if clipboardListener != nil {
		go serveClipboardConnections(runCtx, clipboardListener, desktop.Clipboard)
	}
	if displayListener != nil {
		go serveDisplayConnections(runCtx, displayListener, desktop)
	}
	done := newSessionDone()
	var fsCloseErr error
	go func() {
		err := runManagedExecVMWithSnapshot(runCtx, vm, uart, fsdevs, vsock, rng, balloon, netdev, runtimeDevices, serialOut, snapshot)
		fsCloseErr = closeVMWithFS(vm, fsdevs)
		done.finish(err)
	}()

	var control virtio.VsockConn
	select {
	case err := <-acceptErrCh:
		cancel()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	case conn := <-connCh:
		control = conn
	case <-done.done():
		err := done.result()
		cancel()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	case <-ctx.Done():
		cancel()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(ctx.Err(), serialOut.String(), controlTranscript.String())
	}

	if _, err := controlTranscript.WaitFor(ctx, 0, func(text string) bool {
		return strings.Contains(text, vmruntime.InstanceReadyMarker) || vmruntime.HasFatalBootText(text)
	}); err != nil {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	}
	if vmruntime.HasFatalBootText(controlTranscript.String()) {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(fmt.Errorf("guest reported boot failure"), serialOut.String(), controlTranscript.String())
	}
	if err := emitManagedBootStatus(onEvent, "guest ready"); err != nil {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	}

	return &ManagedSession{
		cancel:            cancel,
		done:              done,
		control:           control,
		listener:          listener,
		clipboardListener: clipboardListener,
		displayListener:   displayListener,
		vsock:             vsock,
		balloon:           balloon,
		desktop:           desktop,
		fsdevs:            fsdevs,
		fsCloseErr:        &fsCloseErr,
		bootWriter:        bootWriter,
		cleanup: func() {
			_ = vm.CancelRun()
		},
		transcript: controlTranscript,
		serialOut:  serialOut,
		dmesg:      dmesg,
	}, nil
}

func (s *ManagedSession) SetBalloonMB(target uint64) error {
	if s == nil || s.balloon == nil {
		return fmt.Errorf("virtio balloon is unavailable")
	}
	return s.balloon.SetTargetPages(balloonTargetPages(target))
}

func (s *ManagedSession) BalloonState() (targetMB, actualMB uint64, driverReady bool) {
	if s == nil || s.balloon == nil {
		return 0, 0, false
	}
	target, actual, ready := s.balloon.State()
	return uint64(target) * 4096 >> 20, uint64(actual) * 4096 >> 20, ready
}

func (s *ManagedSession) Desktop() *virtio.Desktop {
	if s == nil {
		return nil
	}
	return s.desktop
}

func (s *ManagedSession) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	if len(req.Command) == 0 {
		return client.ExecResponse{}, fmt.Errorf("exec command is required")
	}
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	if s.inlineExec {
		execReq := req
		execReq.Kind = "exec_inline"
		if err := s.sendExecMessage(managedagent.ExecRequest(id, execReq)); err != nil {
			return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
		}
	} else {
		s.sendMu.Lock()
		err := managedagent.SendExec(s.control, id, req)
		s.sendMu.Unlock()
		if err != nil {
			return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
		}
	}
	stopKeepalive := s.startExecKeepalive(ctx, execKeepalive)
	defer stopKeepalive()
	segment, err := s.waitForTranscriptCommand(ctx, start, id, func(text string) bool {
		_, _, _, ok := vmruntime.ExtractManagedExecResult(text, id, s.dmesg)
		return ok
	})
	if err != nil {
		if ctx.Err() != nil {
			s.terminateExecAndWait(id, start)
		}
		return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	code, output, usage, ok := vmruntime.ExtractManagedExecResult(segment, id, s.dmesg)
	if !ok {
		return client.ExecResponse{}, transcriptError(fmt.Errorf("exec did not produce a complete result"), s.serialOut.String(), s.transcript.String())
	}
	if s.dmesg {
		output = s.serialOut.String() + "\n[control]\n" + output
	}
	return client.ExecResponse{ExitCode: code, Output: output, Usage: usage}, nil
}

func (s *ManagedSession) Flush(ctx context.Context) error {
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	if err := s.sendExecMessage(managedagent.SyncRequest(id)); err != nil {
		return transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	segment, err := s.waitForTranscriptCommand(ctx, start, id, func(text string) bool {
		_, _, _, ok := vmruntime.ExtractManagedExecResult(text, id, s.dmesg)
		return ok
	})
	if err != nil {
		return transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	code, output, _, ok := vmruntime.ExtractManagedExecResult(segment, id, s.dmesg)
	if !ok {
		return transcriptError(fmt.Errorf("sync did not produce a complete result"), s.serialOut.String(), s.transcript.String())
	}
	if code != 0 {
		return transcriptError(fmt.Errorf("sync exited with status %d: %s", code, output), s.serialOut.String(), s.transcript.String())
	}
	return nil
}

func (s *ManagedSession) ConsoleHistory(context.Context) (string, error) {
	if s == nil || s.serialOut == nil {
		return "", nil
	}
	return s.serialOut.String(), nil
}

func (s *ManagedSession) Wait() error {
	if s == nil || s.done == nil {
		return nil
	}
	err := s.done.wait()
	if s.fsCloseErr != nil {
		err = errors.Join(err, *s.fsCloseErr)
	}
	return err
}

func (s *ManagedSession) Close() error {
	if s == nil {
		return nil
	}
	if s.control != nil {
		_ = s.control.Close()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.clipboardListener != nil {
		_ = s.clipboardListener.Close()
	}
	if s.displayListener != nil {
		_ = s.displayListener.Close()
	}
	if s.vsock != nil {
		_ = s.vsock.Close()
	}
	if s.bootWriter != nil {
		_ = s.bootWriter.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.cleanup != nil {
		s.cleanup()
	}
	var closeErr error
	if s.done != nil {
		closeErr = s.done.wait()
		if errors.Is(closeErr, context.Canceled) {
			closeErr = nil
		}
	}
	if s.fsCloseErr != nil {
		closeErr = errors.Join(closeErr, *s.fsCloseErr)
	}
	if s.transcript != nil {
		closeErr = errors.Join(closeErr, s.transcript.Close())
	}
	if s.serialOut != nil && s.serialOut != s.transcript {
		closeErr = errors.Join(closeErr, s.serialOut.Close())
	}
	return closeErr
}

func transcriptError(err error, serialText, controlText string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w\nserial:\n%s\ncontrol:\n%s", err, boundedManagedTranscript(serialText), boundedManagedTranscript(controlText))
}
