package managed

import (
	"context"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type sessionHelper struct {
	flushes int
	history string
}

func (s *sessionHelper) Exec(context.Context, client.ExecRequest) (client.ExecResponse, error) {
	return client.ExecResponse{}, nil
}

func (s *sessionHelper) ExecStream(context.Context, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

func (s *sessionHelper) Flush(context.Context) error {
	s.flushes++
	return nil
}

func (s *sessionHelper) ConsoleHistory(context.Context) (string, error) {
	return s.history, nil
}

func (s *sessionHelper) Wait() error {
	return nil
}

func (s *sessionHelper) Close() error {
	return nil
}

func TestSessionHelpers(t *testing.T) {
	if err := FlushSession(context.Background(), nil); err == nil {
		t.Fatalf("nil flush error = %v", err)
	}
	history, err := SessionConsoleHistory(context.Background(), nil)
	if err != nil || history != "" {
		t.Fatalf("nil history = %q, %v", history, err)
	}

	session := &sessionHelper{history: "console"}
	if err := FlushSession(context.Background(), session); err != nil {
		t.Fatalf("flush session: %v", err)
	}
	if session.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", session.flushes)
	}
	history, err = SessionConsoleHistory(context.Background(), session)
	if err != nil || history != "console" {
		t.Fatalf("history = %q, %v", history, err)
	}
	if err := WaitSession(session); err != nil {
		t.Fatalf("wait session: %v", err)
	}
	if err := WaitSession(nil); err != nil {
		t.Fatalf("wait nil session: %v", err)
	}
	cleanups := 0
	if err := CloseSession(session, func() error {
		cleanups++
		return nil
	}); err != nil {
		t.Fatalf("close session: %v", err)
	}
	if cleanups != 1 {
		t.Fatalf("cleanups = %d, want 1", cleanups)
	}
	if err := CloseSession(nil, func() error {
		t.Fatal("nil session should not run cleanups")
		return nil
	}); err != nil {
		t.Fatalf("close nil session: %v", err)
	}
}

func TestCloseSessionWithNetwork(t *testing.T) {
	network := &networkCloserHelper{}
	if err := CloseSessionWithNetwork(nil, network); err != nil {
		t.Fatalf("close nil session with network: %v", err)
	}
	if network.closes != 1 {
		t.Fatalf("network closes = %d, want 1", network.closes)
	}

	network = &networkCloserHelper{}
	if err := CloseSessionWithNetwork(&sessionHelper{}, network); err != nil {
		t.Fatalf("close session with network: %v", err)
	}
	if network.closes != 1 {
		t.Fatalf("network closes with session = %d, want 1", network.closes)
	}
}

type networkCloserHelper struct {
	closes int
}

func (n *networkCloserHelper) Close() error {
	n.closes++
	return nil
}
