package netstate

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestManagedNetworkStateRequiresNetwork(t *testing.T) {
	state := State{}
	if err := state.AddPortForward(client.PortForward{}); err == nil {
		t.Fatalf("AddPortForward nil error = %v", err)
	}
	if err := state.AllowServiceProxyPort(8080); err == nil {
		t.Fatalf("AllowServiceProxyPort nil error = %v", err)
	}
	if got := state.IPv4(); got != "" {
		t.Fatalf("nil network IPv4 = %q, want empty", got)
	}
}

func TestManagedNetworkStateIPv4(t *testing.T) {
	if got := (State{IPv4Addr: "10.42.0.99"}).IPv4(); got != "10.42.0.99" {
		t.Fatalf("explicit IPv4 = %q", got)
	}
	if got := (State{Runtime: fakeRuntime{}}).IPv4(); got != "10.42.0.2" {
		t.Fatalf("runtime default IPv4 = %q", got)
	}
}

type fakeRuntime struct{}

func (fakeRuntime) AddPortForward(client.PortForward) error { return nil }

func (fakeRuntime) AllowServiceProxyPort(int) error { return nil }

func (fakeRuntime) GuestAddress() string { return "10.42.0.2" }
