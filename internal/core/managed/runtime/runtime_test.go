package runtime

import (
	"context"
	"errors"
	"testing"

	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	managedhost "github.com/tinyrange/crumblecracker/internal/core/managed/host"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	"github.com/tinyrange/crumblecracker/internal/core/managed/rootartifact"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestStartClosesArtifactWhenHostMissing(t *testing.T) {
	closed := 0
	_, err := (Service{}).Start(context.Background(), StartRequest{
		Artifact: rootartifact.Artifact{Cleanup: func() error {
			closed++
			return nil
		}},
	}, nil)
	if err == nil {
		t.Fatalf("Start succeeded without host")
	}
	if closed != 1 {
		t.Fatalf("artifact closed %d times, want 1", closed)
	}
}

func TestStartClosesArtifactWhenHostStartFails(t *testing.T) {
	closed := 0
	wantErr := errors.New("boot failed")
	_, err := (Service{}).Start(context.Background(), StartRequest{
		Profile: managedguest.Profile{Name: "TestOS"},
		Host:    failingHost{err: wantErr},
		Artifact: rootartifact.Artifact{Cleanup: func() error {
			closed++
			return nil
		}},
	}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want %v", err, wantErr)
	}
	if closed != 1 {
		t.Fatalf("artifact closed %d times, want 1", closed)
	}
}

func TestStartClosesArtifactWhenProfileMissing(t *testing.T) {
	closed := 0
	_, err := (Service{}).Start(context.Background(), StartRequest{
		Host: successfulHost{session: fakeSession{}},
		Artifact: rootartifact.Artifact{Cleanup: func() error {
			closed++
			return nil
		}},
	}, nil)
	if err == nil {
		t.Fatalf("Start succeeded without profile")
	}
	if closed != 1 {
		t.Fatalf("artifact closed %d times, want 1", closed)
	}
}

func TestStartClosesArtifactWhenSpecGuestMismatchesProfile(t *testing.T) {
	closed := 0
	_, err := (Service{}).Start(context.Background(), StartRequest{
		Profile: managedguest.Profile{Name: "FreeBSD"},
		Host:    successfulHost{session: fakeSession{}},
		Spec:    machine.Spec{Guest: "Linux"},
		Artifact: rootartifact.Artifact{Cleanup: func() error {
			closed++
			return nil
		}},
	}, nil)
	if err == nil {
		t.Fatalf("Start succeeded with mismatched guest/profile")
	}
	if closed != 1 {
		t.Fatalf("artifact closed %d times, want 1", closed)
	}
}

func TestStartReturnsSessionAndDoesNotCloseArtifactOnSuccess(t *testing.T) {
	closed := 0
	session := fakeSession{}
	got, err := (Service{}).Start(context.Background(), StartRequest{
		Profile: managedguest.Profile{Name: "TestOS"},
		Host:    successfulHost{session: session},
		Spec:    machine.Spec{Arch: "amd64"},
		Artifact: rootartifact.Artifact{Cleanup: func() error {
			closed++
			return nil
		}},
	}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got.Session == nil {
		t.Fatalf("session is nil")
	}
	if got.Spec.Guest != "TestOS" {
		t.Fatalf("guest = %q, want TestOS", got.Spec.Guest)
	}
	if closed != 0 {
		t.Fatalf("artifact closed on success")
	}
}

type failingHost struct {
	err error
}

func (h failingHost) Start(context.Context, managedhost.StartRequest, func(client.BootEvent) error) (managedsession.Session, error) {
	return nil, h.err
}

type successfulHost struct {
	session managedsession.Session
}

func (h successfulHost) Start(context.Context, managedhost.StartRequest, func(client.BootEvent) error) (managedsession.Session, error) {
	return h.session, nil
}

type fakeSession struct{}

func (fakeSession) Exec(context.Context, client.ExecRequest) (client.ExecResponse, error) {
	return client.ExecResponse{}, nil
}

func (fakeSession) ExecStream(context.Context, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

func (fakeSession) Flush(context.Context) error { return nil }

func (fakeSession) ConsoleHistory(context.Context) (string, error) { return "", nil }

func (fakeSession) Wait() error { return nil }

func (fakeSession) Close() error { return nil }
