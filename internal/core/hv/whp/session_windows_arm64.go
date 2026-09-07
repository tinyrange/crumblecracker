//go:build windows && arm64

package whp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const (
	execTerminateGrace = 500 * time.Millisecond
	execKillWait       = 2 * time.Second
)

type ManagedSession struct {
	cancel     context.CancelFunc
	doneCh     chan error
	control    io.ReadWriteCloser
	listener   io.Closer
	vsock      *virtio.Vsock
	bootWriter *vmruntime.BootEventWriter
	transcript *vmruntime.SerialTranscript
	serialOut  *vmruntime.SerialTranscript
	cleanup    func()
	waitOnce   sync.Once
	waitErr    error
	sendMu     sync.Mutex
	nextID     atomic.Uint64
	dmesg      bool
}

type ManagedSessionOptions struct {
	SnapshotDir     string
	RestoreSnapshot string
}

func StartManagedSession(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	return StartManagedSessionWithNet(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, nil, onEvent)
}

func StartManagedSessionWithNet(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	return StartManagedSessionWithNetOptions(ctx, kernel, initrd, memoryMB, dmesg, fsdevs, netdev, ManagedSessionOptions{}, onEvent)
}

func StartManagedSessionWithNetOptions(ctx context.Context, kernel []byte, initrd []byte, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, opts ManagedSessionOptions, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	if snapshotPath := strings.TrimSpace(opts.RestoreSnapshot); snapshotPath != "" {
		return StartManagedSessionFromSnapshot(ctx, snapshotPath, memoryMB, dmesg, fsdevs, netdev, onEvent)
	}
	trace := os.Getenv("CC_WHP_ARM64_TIMING") != "" || os.Getenv("CC_WHP_BSD_TIMING") != ""
	startTime := time.Now()
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 linux +0s: starting managed session\n")
	}
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

	serialOut := vmruntime.NewSerialTranscript()
	var serialWriter io.Writer = serialOut
	var bootWriter *vmruntime.BootEventWriter
	if onEvent != nil {
		bootWriter = vmruntime.NewBootEventWriter(onEvent)
		serialWriter = io.MultiWriter(serialOut, bootWriter)
	}
	snapshot := newSnapshotTrigger(opts.SnapshotDir, nil)
	prepareStart := time.Now()
	vm, uart, _, err := prepareArm64VM(kernel, initrd, memoryMB, dmesg, dmesg, fsdevs, vsock, rng, netdev, serialWriter)
	if err != nil {
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, err
	}
	if snapshot != nil {
		snapshot.mem = vm.Memory()
		for _, fsdev := range fsdevs {
			if fsdev != nil {
				fsdev.Async = false
			}
		}
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 linux +%s: vm prepared took=%s\n", time.Since(startTime).Round(time.Millisecond), time.Since(prepareStart).Round(time.Millisecond))
	}

	runCtx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		err := runManagedExecVM(runCtx, vm, uart, fsdevs, vsock, rng, netdev, serialOut, snapshot, newWHPPCSampler("linux", kernel))
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 linux +%s: run loop stopped err=%v\n", time.Since(startTime).Round(time.Millisecond), err)
		}
		closeFSDevices(fsdevs)
		closeStart := time.Now()
		closeErr := vm.Close()
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 linux +%s: vm close took=%s err=%v\n", time.Since(startTime).Round(time.Millisecond), time.Since(closeStart).Round(time.Millisecond), closeErr)
		}
		err = errors.Join(err, closeErr)
		doneCh <- err
	}()
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 linux +%s: vm run loop started\n", time.Since(startTime).Round(time.Millisecond))
	}

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
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 linux +%s: control connected\n", time.Since(startTime).Round(time.Millisecond))
		}
	case err := <-doneCh:
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
	if snapshot != nil {
		if err := snapshot.wait(ctx); err != nil {
			cancel()
			_ = control.Close()
			_ = listener.Close()
			vsock.Close()
			if bootWriter != nil {
				_ = bootWriter.Close()
			}
			return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
		}
	}
	if err := emitManagedBootStatus(onEvent, "guest ready"); err != nil {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, err
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 linux +%s: ready marker received\n", time.Since(startTime).Round(time.Millisecond))
	}

	return &ManagedSession{cancel: cancel, doneCh: doneCh, control: control, listener: listener, vsock: vsock, bootWriter: bootWriter, transcript: controlTranscript, serialOut: serialOut, dmesg: dmesg}, nil
}

func (s *ManagedSession) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	if len(req.Command) == 0 {
		return client.ExecResponse{}, fmt.Errorf("exec command is required")
	}
	trace := os.Getenv("CC_WHP_BSD_TIMING") != ""
	startTime := time.Now()
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-managed exec %s start command=%q\n", id, strings.Join(req.Command, " "))
	}
	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	s.sendMu.Lock()
	err := managedagent.SendExec(s.control, id, req)
	s.sendMu.Unlock()
	if err != nil {
		return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	if len(req.Stdin) == 0 {
		if err := s.sendExecMessage(managedagent.StdinCloseRequest(id)); err != nil {
			return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
		}
	}
	segment, err := s.transcript.WaitForCommand(ctx, start, id, func(text string) bool {
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
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-managed exec %s done duration=%s code=%d output=%d\n", id, time.Since(startTime).Round(time.Millisecond), code, len(output))
	}
	if s.dmesg {
		output = s.serialOut.String() + "\n[control]\n" + output
	}
	return client.ExecResponse{ExitCode: code, Output: output, Usage: usage}, nil
}

func (s *ManagedSession) ExecStream(ctx context.Context, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	if (req.Kind == "" || req.Kind == "exec") && len(req.Command) == 0 {
		return fmt.Errorf("exec command is required")
	}
	trace := os.Getenv("CC_WHP_BSD_TIMING") != ""
	startTime := time.Now()
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-managed stream %s start command=%q\n", id, strings.Join(req.Command, " "))
	}
	start := s.transcript.Len()
	reader := s.transcript.RetainReader(start)
	defer reader.Close()
	if err := s.sendExecMessage(managedagent.ExecRequest(id, req)); err != nil {
		return transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	if inputs != nil {
		go managedagent.ForwardInputs(ctx, id, inputs, s.sendExecMessage)
	} else if len(req.Stdin) == 0 && !req.TTY {
		_ = s.sendExecMessage(managedagent.StdinCloseRequest(id))
	}
	err := managedsession.StreamExecEvents(ctx, managedsession.StreamExecOptions{
		Transcript: s.transcript,
		Reader:     reader,
		Start:      start,
		ID:         id,
		OnEvent:    onEvent,
		OnCallbackFail: func() {
			s.terminateExecAndWait(id, start)
		},
		OnContextDone: func() {
			s.terminateExecAndWait(id, start)
		},
		Wait: func(context.Context) error {
			select {
			case vmErr := <-s.doneCh:
				select {
				case s.doneCh <- vmErr:
				default:
				}
				if vmErr != nil {
					return vmErr
				}
				return fmt.Errorf("managed session exited")
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
				return nil
			}
		},
	})
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-managed stream %s done duration=%s err=%v\n", id, time.Since(startTime).Round(time.Millisecond), err)
	}
	return err
}

func (s *ManagedSession) terminateExecAndWait(id string, start int) {
	if msg, ok := managedagent.InputRequest(id, client.ExecInput{Kind: "signal", Signal: "TERM"}); ok {
		_ = s.sendExecMessage(msg)
	}
	if s.waitForExecExit(id, start, execTerminateGrace) {
		return
	}
	if msg, ok := managedagent.InputRequest(id, client.ExecInput{Kind: "signal", Signal: "KILL"}); ok {
		_ = s.sendExecMessage(msg)
	}
	_ = s.waitForExecExit(id, start, execKillWait)
}

func (s *ManagedSession) waitForExecExit(id string, start int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := s.transcript.WaitForCommand(ctx, start, id, func(text string) bool {
		return strings.Contains(text, vmruntime.CommandExitMarkerPref+id+":")
	})
	return err == nil
}

func (s *ManagedSession) Flush(ctx context.Context) error {
	trace := os.Getenv("CC_WHP_BSD_TIMING") != ""
	startTime := time.Now()
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-managed flush %s start\n", id)
	}
	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	if err := s.sendExecMessage(managedagent.SyncRequest(id)); err != nil {
		return transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	segment, err := s.transcript.WaitForCommand(ctx, start, id, func(text string) bool {
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
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-managed flush %s done duration=%s\n", id, time.Since(startTime).Round(time.Millisecond))
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
	return s.waitDone()
}

func (s *ManagedSession) waitDone() error {
	if s == nil || s.doneCh == nil {
		return nil
	}
	s.waitOnce.Do(func() {
		s.waitErr = <-s.doneCh
	})
	return s.waitErr
}

func (s *ManagedSession) waitDoneTimeout(timeout time.Duration) error {
	if s == nil || s.doneCh == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.waitDone()
	}()
	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		return fmt.Errorf("timed out waiting for managed session to stop")
	}
}

func (s *ManagedSession) Close() error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	waitErr := s.waitDoneTimeout(15 * time.Second)
	if s.control != nil {
		_ = s.control.Close()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.vsock != nil {
		_ = s.vsock.Close()
	}
	if s.bootWriter != nil {
		_ = s.bootWriter.Close()
	}
	if s.cleanup != nil {
		s.cleanup()
	}
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
		return waitErr
	}
	return nil
}

func (s *ManagedSession) sendExecMessage(msg vmruntime.ManagedExecRequest) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return managedagent.Send(s.control, msg)
}

func transcriptError(err error, serialText, controlText string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w\nserial:\n%s\ncontrol:\n%s", err, serialText, controlText)
}
