package managed

import (
	"context"
	"testing"

	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestCoreControlRequestsRespectCapabilities(t *testing.T) {
	ctx := context.Background()
	denySession := &helperSession{}
	denyInst := NewCore(Config{
		OSName:  "TestOS",
		Session: denySession,
	})
	if _, err := denyInst.Exec(ctx, client.ExecRequest{Kind: "fs_write"}); err == nil {
		t.Fatalf("denied control request error = %v", err)
	}

	allowSession := &helperSession{}
	allowInst := NewCore(Config{
		OSName:  "TestOS",
		Session: allowSession,
		Capabilities: managedguest.Capabilities{
			CopyIn:         true,
			CopyOut:        true,
			ArchiveExtract: true,
		},
	})
	if _, err := allowInst.Exec(ctx, client.ExecRequest{Kind: "fs_write"}); err != nil {
		t.Fatalf("allowed control request: %v", err)
	}
}

func TestCoreUnsupportedExtensionsUseCapabilities(t *testing.T) {
	inst := NewCore(Config{
		OSName:       "TestOS",
		Capabilities: managedguest.Capabilities{DynamicShares: true},
	})
	if err := inst.AddShare(context.Background(), client.ShareMount{}); err == nil {
		t.Fatalf("AddShare error = %v", err)
	}
}

func TestCoreUsesConfiguredDefaultUserWithoutOverridingExplicitUser(t *testing.T) {
	inst := NewCore(Config{DefaultUser: "jovyan"})
	resolved, err := inst.ExecRequest(client.ExecRequest{Command: []string{"/bin/true"}, SkipResolve: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.User != "jovyan" {
		t.Fatalf("default user = %q", resolved.User)
	}

	resolved, err = inst.ExecRequest(client.ExecRequest{
		Command:     []string{"/bin/true"},
		SkipResolve: true,
		User:        "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.User != "root" {
		t.Fatalf("explicit user = %q", resolved.User)
	}
}

type helperSession struct{}

func (s *helperSession) Exec(context.Context, client.ExecRequest) (client.ExecResponse, error) {
	return client.ExecResponse{}, nil
}

func (s *helperSession) ExecStream(context.Context, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

func (s *helperSession) Flush(context.Context) error {
	return nil
}

func (s *helperSession) ConsoleHistory(context.Context) (string, error) {
	return "", nil
}

func (s *helperSession) Wait() error {
	return nil
}

func (s *helperSession) Close() error {
	return nil
}
