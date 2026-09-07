package netstate

import (
	"context"
	"fmt"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type Runtime interface {
	AddPortForward(client.PortForward) error
	AllowServiceProxyPort(int) error
	GuestAddress() string
}

type PortForwardFallback interface {
	AddPortForward(context.Context, client.PortForward) error
}

type State struct {
	Runtime  Runtime
	IPv4Addr string
}

func AddPortForwardToNetwork(network Runtime, forward client.PortForward) error {
	if network == nil {
		return fmt.Errorf("instance network is not enabled")
	}
	return network.AddPortForward(forward)
}

func AllowServiceProxyPortOnNetwork(network Runtime, port int) error {
	if network == nil {
		return fmt.Errorf("instance network is not enabled")
	}
	return network.AllowServiceProxyPort(port)
}

func (s State) AddPortForward(forward client.PortForward) error {
	return AddPortForwardToNetwork(s.Runtime, forward)
}

func (s State) AddPortForwardWithFallback(ctx context.Context, forward client.PortForward, fallback PortForwardFallback) error {
	if s.Runtime != nil {
		return s.Runtime.AddPortForward(forward)
	}
	if fallback != nil {
		return fallback.AddPortForward(ctx, forward)
	}
	return AddPortForwardToNetwork(nil, forward)
}

func (s State) AllowServiceProxyPort(port int) error {
	return AllowServiceProxyPortOnNetwork(s.Runtime, port)
}

func (s State) IPv4() string {
	if strings.TrimSpace(s.IPv4Addr) != "" {
		return s.IPv4Addr
	}
	if s.Runtime == nil {
		return ""
	}
	return s.Runtime.GuestAddress()
}

func AddManagedNetworkPortForward(ctx context.Context, runtime Runtime, forward client.PortForward) error {
	_ = ctx
	return (State{Runtime: runtime}).AddPortForward(forward)
}

func AddManagedNetworkPortForwardWithFallback(ctx context.Context, runtime Runtime, forward client.PortForward, fallback PortForwardFallback) error {
	return (State{Runtime: runtime}).AddPortForwardWithFallback(ctx, forward, fallback)
}

func AllowManagedNetworkServiceProxyPort(ctx context.Context, runtime Runtime, port int) error {
	_ = ctx
	return (State{Runtime: runtime}).AllowServiceProxyPort(port)
}

func IPv4(runtime Runtime, explicit string) string {
	return (State{Runtime: runtime, IPv4Addr: explicit}).IPv4()
}
