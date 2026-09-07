//go:build linux && (amd64 || arm64)

package kvm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const (
	execTerminateGrace = 500 * time.Millisecond
	execKillWait       = 2 * time.Second
	execKeepalive      = 100 * time.Millisecond
	execSignalAckWait  = 100 * time.Millisecond
	execSignalAttempts = 5
)

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
	stopKeepalive := s.startExecKeepalive(ctx, execKeepalive)
	defer stopKeepalive()
	if req.Kind == "" || req.Kind == "exec" {
		inputReady := vmruntime.ExecTimingMarker + id + ":input_ready:"
		exited := vmruntime.CommandExitMarkerPref + id + ":"
		if _, err := s.waitForTranscriptCommandEvent(ctx, start, id, func(text string) bool {
			return strings.Contains(text, inputReady) || strings.Contains(text, exited)
		}); err != nil {
			return transcriptError(err, s.serialOut.String(), s.transcript.String())
		}
	}
	inputErr := make(chan error, 1)
	if inputs != nil {
		go func() {
			if err := s.forwardExecInputs(ctx, id, inputs); err != nil {
				inputErr <- err
			}
		}()
	} else if len(req.Stdin) == 0 && !req.TTY {
		if err := s.sendStdinClose(id); err != nil {
			return transcriptError(err, s.serialOut.String(), s.transcript.String())
		}
	}
	return s.streamExecEvents(ctx, start, id, reader, inputErr, onEvent)
}

// startExecKeepalive gives a blocked restored vCPU a periodic interrupt while
// an exec is outstanding. It does not consume vsock credit or write control
// bytes, either of which could block behind the command it is trying to wake.
func (s *ManagedSession) startExecKeepalive(ctx context.Context, interval time.Duration) context.CancelFunc {
	if s == nil {
		_, cancel := context.WithCancel(ctx)
		return cancel
	}
	return startVirtioKeepalive(ctx, interval, func() {
		_ = s.vsock.Poke()
		for _, fsdev := range s.fsdevs {
			_ = fsdev.Poke()
		}
	})
}

func startVsockKeepalive(ctx context.Context, vsock *virtio.Vsock, interval time.Duration) context.CancelFunc {
	return startVirtioKeepalive(ctx, interval, func() { _ = vsock.Poke() })
}

func startVirtioKeepalive(ctx context.Context, interval time.Duration, poke func()) context.CancelFunc {
	keepaliveCtx, cancel := context.WithCancel(ctx)
	if poke == nil || interval <= 0 {
		return cancel
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-keepaliveCtx.Done():
				return
			case <-ticker.C:
				poke()
			}
		}
	}()
	return cancel
}

func (s *ManagedSession) nextExecID() string {
	return strconv.FormatUint(s.nextID.Add(1), 10)
}

func (s *ManagedSession) sendExecStart(id string, req client.ExecRequest) error {
	return s.sendExecMessage(managedagent.ExecRequest(id, req))
}

func (s *ManagedSession) forwardExecInputs(ctx context.Context, id string, inputs <-chan client.ExecInput) error {
	return managedagent.ForwardInputs(ctx, id, inputs, func(msg vmruntime.ManagedExecRequest) error {
		if msg.Kind == "signal" {
			return s.sendExecSignal(ctx, id, msg.Signal)
		}
		return s.sendExecMessage(msg)
	})
}

func (s *ManagedSession) sendExecInput(id string, input client.ExecInput) error {
	if input.Kind == "signal" {
		ctx, cancel := context.WithTimeout(context.Background(), execSignalAckWait*execSignalAttempts)
		defer cancel()
		return s.sendExecSignal(ctx, id, input.Signal)
	}
	msg, ok := managedagent.InputRequest(id, input)
	if !ok {
		return nil
	}
	return s.sendExecMessage(msg)
}

func (s *ManagedSession) sendExecSignal(ctx context.Context, id, signal string) error {
	controlID := id + "-" + strconv.FormatUint(s.nextID.Add(1), 10)
	msg, _ := managedagent.InputRequest(id, client.ExecInput{Kind: "signal", Signal: signal})
	msg.ControlID = controlID
	start := s.transcript.Len()
	var lastErr error
	for attempt := 0; attempt < execSignalAttempts; attempt++ {
		if err := s.sendExecMessage(msg); err != nil {
			lastErr = err
		} else {
			ackCtx, cancel := context.WithTimeout(ctx, execSignalAckWait)
			_, err := s.waitForTranscript(ackCtx, start, func(text string) bool {
				return vmruntime.HasManagedControlAck(text, controlID)
			})
			cancel()
			if err == nil {
				return nil
			}
			lastErr = err
		}
		if ctx.Err() != nil {
			break
		}
		_ = s.vsock.Poke()
	}
	return fmt.Errorf("guest did not acknowledge %s signal for exec %s: %w", signal, id, lastErr)
}

func (s *ManagedSession) sendStdinClose(id string) error {
	return s.sendExecMessage(managedagent.StdinCloseRequest(id))
}

func (s *ManagedSession) sendExecMessage(msg vmruntime.ManagedExecRequest) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return managedagent.Send(s.control, msg)
}

func (s *ManagedSession) streamExecEvents(ctx context.Context, start int, id string, reader vmruntime.TranscriptReader, inputErr <-chan error, onEvent func(client.ExecEvent) error) error {
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
			case err := <-inputErr:
				return fmt.Errorf("deliver exec input: %w", err)
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
