//go:build linux && (amd64 || arm64)

package kvm

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type sessionDone struct {
	ch   chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newSessionDone() *sessionDone {
	return &sessionDone{ch: make(chan struct{})}
}

func (d *sessionDone) finish(err error) {
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

func (d *sessionDone) wait() error {
	if d == nil {
		return nil
	}
	<-d.ch
	return d.result()
}

func (d *sessionDone) result() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *sessionDone) done() <-chan struct{} {
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

func (s *ManagedSession) waitForTranscript(ctx context.Context, start int, predicate func(string) bool) (string, error) {
	return s.waitForTranscriptMode(ctx, start, "", false, predicate)
}

func (s *ManagedSession) waitForTranscriptCommand(ctx context.Context, start int, commandID string, predicate func(string) bool) (string, error) {
	return s.waitForTranscriptMode(ctx, start, commandID, false, predicate)
}

func (s *ManagedSession) waitForTranscriptCommandEvent(ctx context.Context, start int, commandID string, predicate func(string) bool) (string, error) {
	return s.waitForTranscriptMode(ctx, start, commandID, true, predicate)
}

func (s *ManagedSession) waitForTranscriptMode(ctx context.Context, start int, commandID string, beforeExit bool, predicate func(string) bool) (string, error) {
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
		} else if beforeExit {
			segment, err = s.transcript.WaitForCommandEvent(waitCtx, start, commandID, predicate)
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
		// The transcript result and the caller deadline can become ready in the
		// same scheduler turn. Give bytes already delivered by the guest one
		// final bounded parse instead of randomly choosing a false timeout.
		finalCtx, cancelFinal := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancelFinal()
		var segment string
		var err error
		if commandID == "" {
			segment, err = s.transcript.WaitFor(finalCtx, start, predicate)
		} else if beforeExit {
			segment, err = s.transcript.WaitForCommandEvent(finalCtx, start, commandID, predicate)
		} else {
			segment, err = s.transcript.WaitForCommand(finalCtx, start, commandID, predicate)
		}
		if err == nil {
			return segment, nil
		}
		return "", ctx.Err()
	}
}
