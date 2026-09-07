//go:build darwin && arm64

package hvf

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type ManagedSession struct {
	cancel     context.CancelFunc
	done       *managedSessionDone
	closeDone  chan struct{}
	control    io.ReadWriteCloser
	listener   io.Closer
	bootWriter *bootEventWriter
	transcript *serialTranscript
	serialOut  *serialTranscript
	cleanup    func()
	sendMu     sync.Mutex
	nextID     atomic.Uint64
	dmesg      bool
}

func (s *ManagedSession) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	if len(req.Command) == 0 {
		return client.ExecResponse{}, fmt.Errorf("exec command is required")
	}
	id := s.nextExecID()
	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	if err := s.sendExecStart(id, req); err != nil {
		return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	if len(req.Stdin) == 0 {
		if err := s.sendStdinClose(id); err != nil {
			return client.ExecResponse{}, transcriptError(err, s.serialOut.String(), s.transcript.String())
		}
	}
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

func (s *ManagedSession) ExecStream(ctx context.Context, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	if (req.Kind == "" || req.Kind == "exec") && len(req.Command) == 0 {
		return fmt.Errorf("exec command is required")
	}
	id := s.nextExecID()
	start := s.transcript.Len()
	reader := s.transcript.RetainReader(start)
	defer reader.Close()
	if err := s.sendExecStart(id, req); err != nil {
		return transcriptError(err, s.serialOut.String(), s.transcript.String())
	}
	if inputs != nil {
		go s.forwardExecInputs(ctx, id, inputs)
	} else if len(req.Stdin) == 0 && !req.TTY {
		if err := s.sendStdinClose(id); err != nil {
			return transcriptError(err, s.serialOut.String(), s.transcript.String())
		}
	}
	return s.streamExecEvents(ctx, start, id, reader, onEvent)
}

func (s *ManagedSession) Flush(ctx context.Context) error {
	id := s.nextExecID()
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
	return s.done.wait()
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
	if s.bootWriter != nil {
		_ = s.bootWriter.Close()
	}
	if s.cleanup != nil {
		s.cleanup()
	}
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *ManagedSession) nextExecID() string {
	return strconv.FormatUint(s.nextID.Add(1), 10)
}

func (s *ManagedSession) sendExecStart(id string, req client.ExecRequest) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return managedagent.Send(s.control, managedagent.ExecRequest(id, req))
}

func (s *ManagedSession) forwardExecInputs(ctx context.Context, id string, inputs <-chan client.ExecInput) {
	managedagent.ForwardInputs(ctx, id, inputs, s.sendExecMessage)
}

func (s *ManagedSession) sendExecInput(id string, input client.ExecInput) error {
	msg, ok := managedagent.InputRequest(id, input)
	if !ok {
		return nil
	}
	return s.sendExecMessage(msg)
}

func (s *ManagedSession) sendStdinClose(id string) error {
	return s.sendExecMessage(managedagent.StdinCloseRequest(id))
}

func (s *ManagedSession) sendExecMessage(msg vmruntime.ManagedExecRequest) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return managedagent.Send(s.control, msg)
}

func (s *ManagedSession) streamExecEvents(ctx context.Context, start int, id string, reader vmruntime.TranscriptReader, onEvent func(client.ExecEvent) error) error {
	return managedsession.StreamExecEvents(ctx, managedsession.StreamExecOptions{
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
			case <-s.done.done():
				return sessionExitError(s.done.result())
			case <-ctx.Done():
				s.terminateExecAndWait(id, start)
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
				return nil
			}
		},
	})
}

func (s *ManagedSession) terminateExecAndWait(id string, start int) {
	_ = s.sendExecInput(id, client.ExecInput{Kind: "signal", Signal: "TERM"})
	if s.waitForExecExit(id, start, execTerminateGrace) {
		return
	}
	_ = s.sendExecInput(id, client.ExecInput{Kind: "signal", Signal: "KILL"})
	_ = s.waitForExecExit(id, start, execKillWait)
}

func (s *ManagedSession) waitForExecExit(id string, start int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := s.waitForTranscriptCommand(ctx, start, id, func(text string) bool {
		return strings.Contains(text, vmruntime.CommandExitMarkerPref+id+":")
	})
	return err == nil
}

func (s *ManagedSession) waitForTranscript(ctx context.Context, start int, predicate func(string) bool) (string, error) {
	return s.waitForTranscriptCommand(ctx, start, "", predicate)
}

func (s *ManagedSession) waitForTranscriptCommand(ctx context.Context, start int, commandID string, predicate func(string) bool) (string, error) {
	if s == nil || s.transcript == nil {
		return "", fmt.Errorf("managed session is not running")
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		segment string
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		var segment string
		var err error
		if commandID == "" {
			segment, err = s.transcript.WaitFor(waitCtx, start, predicate)
		} else {
			segment, err = s.transcript.WaitForCommand(waitCtx, start, commandID, predicate)
		}
		resultCh <- result{segment: segment, err: err}
	}()

	select {
	case res := <-resultCh:
		return res.segment, res.err
	case <-s.done.done():
		return "", sessionExitError(s.done.result())
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type managedSessionDone struct {
	ch   chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newManagedSessionDone() *managedSessionDone {
	return &managedSessionDone{ch: make(chan struct{})}
}

func (d *managedSessionDone) finish(err error) {
	if d == nil {
		return
	}
	d.once.Do(func() {
		d.mu.Lock()
		d.err = err
		d.mu.Unlock()
		close(d.ch)
	})
}

func (d *managedSessionDone) wait() error {
	if d == nil {
		return nil
	}
	<-d.ch
	return d.result()
}

func (d *managedSessionDone) result() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *managedSessionDone) done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.ch
}

func sessionExitError(err error) error {
	if err != nil {
		return fmt.Errorf("managed session exited: %w", err)
	}
	return fmt.Errorf("managed session exited")
}

func transcriptError(err error, serialText, controlText string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w\nserial:\n%s\ncontrol:\n%s", err, serialText, controlText)
}

func bsdStartupError(err error, serialText, controlText string) error {
	return transcriptError(err, serialText, controlText)
}

func containsReadyOrFatal(text string) bool {
	return strings.Contains(text, vmruntime.InstanceReadyMarker) || vmruntime.HasFatalBootText(text)
}

func emitManagedBootStatus(onEvent func(client.BootEvent) error, message string) error {
	if onEvent == nil {
		return nil
	}
	return onEvent(client.BootEvent{Kind: "status", Message: message})
}
