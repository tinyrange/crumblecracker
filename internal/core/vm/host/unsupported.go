package host

import (
	"context"
	"fmt"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type UnsupportedBackend struct{}

func (UnsupportedBackend) Start(ctx context.Context, req client.CreateInstanceRequest) (Instance, error) {
	return UnsupportedBackend{}.StartStream(ctx, req, nil)
}

func (UnsupportedBackend) StartStream(ctx context.Context, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	_ = ctx
	_ = req
	_ = onEvent
	return nil, fmt.Errorf("VM backend is not configured")
}

func (UnsupportedBackend) StartBlank(ctx context.Context, req client.StartInstanceRequest) (Instance, error) {
	return UnsupportedBackend{}.StartBlankStream(ctx, req, nil)
}

func (UnsupportedBackend) StartBlankStream(ctx context.Context, req client.StartInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	_ = ctx
	_ = req
	_ = onEvent
	return nil, fmt.Errorf("VM backend is not configured")
}

func (UnsupportedBackend) Run(ctx context.Context, req client.RunRequest) (client.ExecResponse, error) {
	_ = ctx
	_ = req
	return client.ExecResponse{}, fmt.Errorf("VM backend is not configured")
}

func (UnsupportedBackend) RunStream(ctx context.Context, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	_ = ctx
	_ = req
	_ = inputs
	_ = onEvent
	return fmt.Errorf("VM backend is not configured")
}

func (UnsupportedBackend) RunInInstance(ctx context.Context, inst Instance, runningImage string, req client.RunRequest) (client.ExecResponse, error) {
	_ = ctx
	_ = inst
	_ = runningImage
	_ = req
	return client.ExecResponse{}, fmt.Errorf("VM backend is not configured")
}

func (UnsupportedBackend) RunInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	_ = ctx
	_ = inst
	_ = runningImage
	_ = req
	_ = inputs
	_ = onEvent
	return fmt.Errorf("VM backend is not configured")
}

func (UnsupportedBackend) ExecInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	_ = ctx
	_ = inst
	_ = runningImage
	_ = req
	_ = inputs
	_ = onEvent
	return fmt.Errorf("VM backend is not configured")
}

func (UnsupportedBackend) AddPortForward(ctx context.Context, forward client.PortForward) error {
	_, _ = ctx, forward
	return fmt.Errorf("VM backend is not configured")
}
