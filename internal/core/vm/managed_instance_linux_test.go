//go:build linux

package vm

import (
	"context"
	"os"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestManagedInstanceControlRequestsRespectCapabilities(t *testing.T) {
	ctx := context.Background()
	denySession := &recordingManagedSession{}
	denyInst := &managedInstance{
		osName:  "TestOS",
		session: denySession,
	}
	denyCases := []struct {
		kind string
	}{
		{kind: "fs_mkdir"},
		{kind: "fs_write"},
		{kind: "fs_extract"},
		{kind: "fs_archive"},
	}
	for _, tc := range denyCases {
		if _, err := denyInst.Exec(ctx, client.ExecRequest{Kind: tc.kind}); err == nil {
			t.Fatalf("Exec(%s) expected error", tc.kind)
		}
		if denySession.execs != 0 {
			t.Fatalf("Exec(%s) reached session despite denied capability", tc.kind)
		}
	}

	allowSession := &recordingManagedSession{}
	allowInst := &managedInstance{
		osName:  "TestOS",
		session: allowSession,
		caps: guestCapabilities{
			CopyIn:         true,
			CopyOut:        true,
			ArchiveExtract: true,
		},
	}
	for _, kind := range []string{"fs_mkdir", "fs_write", "fs_extract", "fs_archive", "sync"} {
		if _, err := allowInst.Exec(ctx, client.ExecRequest{Kind: kind}); err != nil {
			t.Fatalf("Exec(%s): %v", kind, err)
		}
	}
	if allowSession.execs != 5 {
		t.Fatalf("allowed control requests reached session %d times, want 5", allowSession.execs)
	}
}

func TestManagedInstanceAlternateImageExecRespectsCapabilities(t *testing.T) {
	inst := &managedInstance{
		osName:  "TestOS",
		session: &recordingManagedSession{},
	}
	_, err := (&runtimeBackend{}).RunInInstance(context.Background(), inst, "@openbsd", client.RunRequest{
		Image:   "alpine",
		Command: []string{"true"},
	})
	if err == nil {
		t.Fatalf("RunInInstance alternate image error = %v", err)
	}
}

func TestManagedInstanceRootSnapshotRespectsCapabilities(t *testing.T) {
	denied := &managedInstance{osName: "TestOS"}
	if _, err := denied.RootSnapshot(); err == nil {
		t.Fatalf("denied RootSnapshot error = %v", err)
	}

	root := t.TempDir()
	if err := os.WriteFile(root+"/file.txt", []byte("snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	allowed := &managedInstance{
		osName: "TestOS",
		root:   imagefs.NewHostFS(root, nil),
		caps:   guestCapabilities{RootSnapshot: true},
	}
	got, err := allowed.RootSnapshot()
	if err != nil {
		t.Fatalf("RootSnapshot: %v", err)
	}
	if got == nil {
		t.Fatalf("RootSnapshot returned nil root")
	}
}

type recordingManagedSession struct {
	execs   int
	flushes int
	history string
}

func (s *recordingManagedSession) Exec(context.Context, client.ExecRequest) (client.ExecResponse, error) {
	s.execs++
	return client.ExecResponse{}, nil
}

func (s *recordingManagedSession) ExecStream(context.Context, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

func (s *recordingManagedSession) Flush(context.Context) error {
	s.flushes++
	return nil
}

func (s *recordingManagedSession) ConsoleHistory(context.Context) (string, error) {
	return s.history, nil
}

func (s *recordingManagedSession) Wait() error { return nil }

func (s *recordingManagedSession) Close() error { return nil }
