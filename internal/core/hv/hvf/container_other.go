//go:build !darwin || !arm64

package hvf

import (
	"context"
	"fmt"

	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type ContainerRunRequest = vmruntime.RunRequest
type DirectoryShare = vmruntime.DirectoryShare
type ContainerRunResult = vmruntime.RunResult

type exitTimingBucket struct {
	Count      int64 `json:"count"`
	TotalNanos int64 `json:"total_nanos"`
	MaxNanos   int64 `json:"max_nanos"`
}

type ContainerSession struct{}

func (s *ContainerSession) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	_ = ctx
	_ = req
	return client.ExecResponse{}, fmt.Errorf("hvf container runner is unsupported on this host")
}

func StartContainer(ctx context.Context, req ContainerRunRequest) (*ContainerSession, error) {
	return StartContainerStream(ctx, req, nil)
}

func StartContainerStream(ctx context.Context, req ContainerRunRequest, onEvent func(client.BootEvent) error) (*ContainerSession, error) {
	_ = ctx
	_ = req
	_ = onEvent
	return nil, fmt.Errorf("hvf container runner is unsupported on this host")
}

func (s *ContainerSession) Wait() error {
	return fmt.Errorf("hvf container runner is unsupported on this host")
}

func (s *ContainerSession) ConsoleHistory(context.Context) (string, error) {
	return "", nil
}

func (s *ContainerSession) Close() error {
	return nil
}

func (s *ContainerSession) AddPortForward(ctx context.Context, forward client.PortForward) error {
	_, _ = ctx, forward
	return fmt.Errorf("hvf container runner is unsupported on this host")
}

func RunContainer(ctx context.Context, req ContainerRunRequest) (ContainerRunResult, error) {
	_ = ctx
	_ = req
	return ContainerRunResult{}, fmt.Errorf("hvf container runner is unsupported on this host")
}

func ExitTimingSnapshot() map[string]exitTimingBucket {
	return nil
}
