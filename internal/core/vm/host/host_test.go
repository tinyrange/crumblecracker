package host

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestInProcessHostReportsBackendCapabilities(t *testing.T) {
	host := NewInProcess(&fakeBackend{}, func() client.CapabilitiesResponse {
		return client.CapabilitiesResponse{
			Backend:      "kvm",
			MaxInstances: 4,
			NetworkModes: []string{
				"user",
			},
		}
	})
	caps := host.HostCapabilities(context.Background())
	if caps.Backend != "kvm" {
		t.Fatalf("backend = %q, want kvm", caps.Backend)
	}
	if caps.MaxVMs != 4 {
		t.Fatalf("max VMs = %d, want 4", caps.MaxVMs)
	}
	if !caps.SupportsL2 {
		t.Fatalf("expected L2 support when network modes are available")
	}
	if caps.Locality != "in-process" {
		t.Fatalf("locality = %q, want in-process", caps.Locality)
	}
}

func TestPlacementHostSelectsL2CapableHostAndReleasesCapacity(t *testing.T) {
	nonNetwork := &fakeHost{caps: Capabilities{Backend: "plain", MaxVMs: 1}}
	network := &fakeHost{caps: Capabilities{Backend: "net", MaxVMs: 1, SupportsL2: true}}
	placement := NewPlacement(nonNetwork, network)

	inst, err := placement.StartStream(context.Background(), client.CreateInstanceRequest{
		Image:   "alpine",
		Network: &client.NetworkConfig{Enabled: true},
	}, nil)
	if err != nil {
		t.Fatalf("start with network: %v", err)
	}
	if nonNetwork.starts != 0 {
		t.Fatalf("non-L2 host starts = %d, want 0", nonNetwork.starts)
	}
	if network.starts != 1 {
		t.Fatalf("L2 host starts = %d, want 1", network.starts)
	}

	_, err = placement.StartStream(context.Background(), client.CreateInstanceRequest{
		Image:   "alpine",
		Network: &client.NetworkConfig{Enabled: true},
	}, nil)
	if err == nil {
		t.Fatalf("expected capacity error while L2 host is occupied")
	}

	if err := inst.Close(); err != nil {
		t.Fatalf("close hosted instance: %v", err)
	}
	_, err = placement.StartStream(context.Background(), client.CreateInstanceRequest{
		Image:   "alpine",
		Network: &client.NetworkConfig{Enabled: true},
	}, nil)
	if err != nil {
		t.Fatalf("start after release: %v", err)
	}
}

func TestPlacementHostAggregatesCapabilities(t *testing.T) {
	placement := NewPlacement(
		&fakeHost{caps: Capabilities{Backend: "a", MaxVMs: 2, SupportsL2: true}},
		&fakeHost{caps: Capabilities{Backend: "b", MaxVMs: 3, SupportsFSRPC: true}},
	)
	caps := placement.HostCapabilities(context.Background())
	if caps.Backend != "placement" {
		t.Fatalf("backend = %q, want placement", caps.Backend)
	}
	if caps.MaxVMs != 5 {
		t.Fatalf("max VMs = %d, want 5", caps.MaxVMs)
	}
	if !caps.SupportsL2 {
		t.Fatalf("expected L2 support to aggregate")
	}
	if !caps.SupportsFSRPC {
		t.Fatalf("expected FSRPC support to aggregate")
	}
}

type fakeHost struct {
	caps   Capabilities
	starts int
	inst   Instance
}

func (h *fakeHost) HostCapabilities(context.Context) Capabilities {
	return h.caps
}

func (h *fakeHost) Close() error {
	return nil
}

func (h *fakeHost) Start(ctx context.Context, req client.CreateInstanceRequest) (Instance, error) {
	return h.StartStream(ctx, req, nil)
}

func (h *fakeHost) StartStream(context.Context, client.CreateInstanceRequest, func(client.BootEvent) error) (Instance, error) {
	h.starts++
	if h.inst != nil {
		return h.inst, nil
	}
	return &fakeInstance{}, nil
}

func (h *fakeHost) StartBlank(ctx context.Context, req client.StartInstanceRequest) (Instance, error) {
	return h.StartBlankStream(ctx, req, nil)
}

func (h *fakeHost) StartBlankStream(context.Context, client.StartInstanceRequest, func(client.BootEvent) error) (Instance, error) {
	h.starts++
	if h.inst != nil {
		return h.inst, nil
	}
	return &fakeInstance{}, nil
}

func (h *fakeHost) Run(context.Context, client.RunRequest) (client.ExecResponse, error) {
	return client.ExecResponse{}, nil
}

func (h *fakeHost) RunStream(context.Context, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

func (h *fakeHost) RunInInstance(context.Context, Instance, string, client.RunRequest) (client.ExecResponse, error) {
	return client.ExecResponse{}, nil
}

func (h *fakeHost) RunInInstanceStream(context.Context, Instance, string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

func (h *fakeHost) ExecInInstanceStream(context.Context, Instance, string, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

type fakeBackend struct {
	fakeHost
}

type fakeInstance struct {
	closed bool
}

func (i *fakeInstance) AddShare(context.Context, client.ShareMount) error {
	return nil
}

func (i *fakeInstance) AddPortForward(context.Context, client.PortForward) error {
	return nil
}

func (i *fakeInstance) Exec(context.Context, client.ExecRequest) (client.ExecResponse, error) {
	return client.ExecResponse{}, nil
}

func (i *fakeInstance) ExecStream(context.Context, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error {
	return nil
}

func (i *fakeInstance) Wait() error {
	if i.closed {
		return errors.New("closed")
	}
	return nil
}

func (i *fakeInstance) Close() error {
	i.closed = true
	return nil
}
