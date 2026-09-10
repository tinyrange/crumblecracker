//go:build linux && arm64

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
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/fdt"
	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/shmem"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
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
}

type ManagedSessionOptions struct {
	SnapshotDir     string
	RestoreSnapshot string
	DisplayWidth    uint32
	DisplayHeight   uint32
	NetDevice       *virtio.Net
	SharedMemory    *shmem.Attachment
}

func StartManagedSession(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	return StartManagedSessionWithOptions(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, ManagedSessionOptions{}, onEvent)
}

func StartManagedSessionWithOptions(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, opts ManagedSessionOptions, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	if (strings.TrimSpace(opts.SnapshotDir) != "" || strings.TrimSpace(opts.RestoreSnapshot) != "") &&
		(opts.DisplayWidth != 0 || opts.DisplayHeight != 0) {
		return nil, fmt.Errorf("display-enabled VMs do not support startup snapshots")
	}
	if snapshotPath := strings.TrimSpace(opts.RestoreSnapshot); snapshotPath != "" {
		return StartManagedSessionFromSnapshot(ctx, snapshotPath, memoryMB, dmesg, fsdevs, onEvent)
	}
	stageStart := time.Now()
	if err := emitManagedBootStatus(onEvent, "starting VM"); err != nil {
		return nil, err
	}
	backend := virtio.NewSimpleVsockBackend()
	listener, err := backend.Listen(vmruntime.ControlPort)
	if err != nil {
		return nil, fmt.Errorf("listen vsock control: %w", err)
	}

	vsock := virtio.NewVsock(arm64vm.VsockBase, arm64vm.VsockSize, arm64vm.VsockIRQ, vmruntime.GuestCID, backend)
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)
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
		gpu := virtio.NewGPU(arm64vm.GPUBase, arm64vm.GPUSize, arm64vm.GPUIRQ, framebuffer)
		keyboard := virtio.NewKeyboardInput(arm64vm.KeyboardBase, arm64vm.KeyboardSize, arm64vm.KeyboardIRQ)
		pointer := virtio.NewAbsolutePointerInput(arm64vm.PointerBase, arm64vm.PointerSize, arm64vm.PointerIRQ, opts.DisplayWidth, opts.DisplayHeight)
		relativePointer := virtio.NewRelativePointerInput(arm64vm.RelativePointerBase, arm64vm.RelativePointerSize, arm64vm.RelativePointerIRQ)
		clipboard := virtio.NewClipboard()
		clipboardListener, err = backend.Listen(vmruntime.ClipboardPort)
		if err != nil {
			_ = listener.Close()
			vsock.Close()
			return nil, fmt.Errorf("listen for guest clipboard bridge: %w", err)
		}
		displayListener, err = backend.Listen(vmruntime.DisplayPort)
		if err != nil {
			_ = clipboardListener.Close()
			_ = listener.Close()
			vsock.Close()
			return nil, fmt.Errorf("listen for guest display bridge: %w", err)
		}
		desktop = &virtio.Desktop{Framebuffer: framebuffer, GPU: gpu, Keyboard: keyboard, Pointer: pointer, RelativePointer: relativePointer, Clipboard: clipboard}
		displayDevices = []virtio.MMIODevice{gpu, keyboard, pointer, relativePointer}
	}
	displayListenersOwned := true
	defer func() {
		if !displayListenersOwned {
			return
		}
		if clipboardListener != nil {
			_ = clipboardListener.Close()
		}
		if displayListener != nil {
			_ = displayListener.Close()
		}
	}()
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
	if opts.NetDevice != nil {
		nodes = append(nodes, opts.NetDevice.DeviceTreeNode())
	}
	if desktop != nil {
		nodes = append(nodes, desktop.GPU.DeviceTreeNode(), desktop.Keyboard.DeviceTreeNode(), desktop.Pointer.DeviceTreeNode(), desktop.RelativePointer.DeviceTreeNode())
	}
	snapshot := newSnapshotTrigger(opts.SnapshotDir, nil)
	if snapshot != nil {
		nodes = append(nodes, arm64vm.SnapshotDeviceNode())
	}
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			nodes = append(nodes, fsdev.DeviceTreeNode())
		}
	}
	timing.Since(ctx, "startup.kvm.host_devices", stageStart)

	stageStart = time.Now()
	vm, err := NewVM()
	timing.Since(ctx, "startup.kvm.vm_create", stageStart)
	if err != nil {
		_ = listener.Close()
		vsock.Close()
		return nil, err
	}
	stageStart = time.Now()
	mem, err := vm.MapAnonymousMemory(arm64vm.MemorySizeBytes(memoryMB), arm64vm.MemoryBase)
	timing.Since(ctx, "startup.kvm.memory_map", stageStart)
	if err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		vsock.Close()
		return nil, fmt.Errorf("map guest memory: %w", err)
	}
	var sharedMemoryDevice *shmem.Device
	if opts.SharedMemory != nil {
		sharedMemoryDevice, err = shmem.NewDevice(opts.SharedMemory.Config().PhysAddr, opts.SharedMemory, vm)
		if err != nil {
			closeVMWithFS(vm, fsdevs)
			_ = listener.Close()
			vsock.Close()
			return nil, err
		}
		opts.SharedMemory.Claim()
	}
	if snapshot != nil {
		snapshot.mem = mem
	}

	stageStart = time.Now()
	serialOut := vmruntime.NewSerialTranscript()
	var serialWriter io.Writer = serialOut
	var bootWriter *vmruntime.BootEventWriter
	if onEvent != nil {
		bootWriter = vmruntime.NewBootEventWriter(onEvent)
		serialWriter = io.MultiWriter(serialOut, bootWriter)
	}
	serialWriter = snapshot.wrapSerialWriter(serialWriter)
	uart := serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, serialWriter)
	uart.AttachIRQ(vm, arm64vm.UARTSPI)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Attach(vm, vm)
		}
	}
	vsock.Attach(vm, vm)
	rng.Attach(vm, vm)
	if opts.NetDevice != nil {
		opts.NetDevice.Attach(vm, vm)
	}
	runtimeDevices := append([]virtio.MMIODevice(nil), displayDevices...)
	if sharedMemoryDevice != nil {
		runtimeDevices = append(runtimeDevices, sharedMemoryDevice)
	}
	if opts.NetDevice != nil {
		runtimeDevices = append(runtimeDevices, opts.NetDevice)
	}
	for _, device := range displayDevices {
		switch typed := device.(type) {
		case *virtio.GPU:
			typed.Attach(vm, vm)
		case *virtio.Input:
			typed.Attach(vm, vm)
		}
	}
	timing.Since(ctx, "startup.kvm.device_attach", stageStart)

	stageStart = time.Now()
	plan, err := arm64vm.PrepareBoot(mem, kernel, initrd, arm64vm.BootConfig{
		MemoryMB:   memoryMB,
		GICVersion: arm64vm.GICVersionV2,
		Dmesg:      dmesg,
		ExtraNodes: nodes,
		RecordTime: func(name string, duration time.Duration) {
			timing.Record(ctx, "startup.kvm.boot_prepare."+name, duration)
		},
	})
	timing.Since(ctx, "startup.kvm.boot_prepare", stageStart)
	if err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, fmt.Errorf("prepare boot: %w", err)
	}
	if err := setupBootRegisters(vm, plan); err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, err
	}

	stageStart = time.Now()
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
		err := runManagedExecVMWithSnapshot(runCtx, vm, uart, fsdevs, vsock, rng, runtimeDevices, serialOut, snapshot)
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
	timing.Since(ctx, "startup.kvm.vcpu_to_control", stageStart)

	stageStart = time.Now()
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
	timing.Since(ctx, "startup.kvm.control_to_ready", stageStart)
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

	displayListenersOwned = false
	return &ManagedSession{
		cancel:            cancel,
		done:              done,
		control:           control,
		listener:          listener,
		clipboardListener: clipboardListener,
		displayListener:   displayListener,
		vsock:             vsock,
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

func (s *ManagedSession) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	if len(req.Command) == 0 {
		return client.ExecResponse{}, fmt.Errorf("exec command is required")
	}
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	s.sendMu.Lock()
	err := managedagent.SendExec(s.control, id, req)
	s.sendMu.Unlock()
	if err != nil {
		return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
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
	s.sendMu.Lock()
	err := managedagent.Send(s.control, managedagent.SyncRequest(id))
	s.sendMu.Unlock()
	if err != nil {
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

func (s *ManagedSession) Desktop() *virtio.Desktop {
	if s == nil {
		return nil
	}
	return s.desktop
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
