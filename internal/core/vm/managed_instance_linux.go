//go:build linux

package vm

import (
	"context"
	"fmt"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	hostmanaged "github.com/tinyrange/crumblecracker/internal/core/vm/host/managed"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type managedInstance struct {
	osName         string
	session        managedsession.Session
	closeRuntime   func() error
	root           imagefs.Directory
	baseEnv        []string
	defaultUser    string
	defaultRootDir string
	workDir        string
	network        *linuxNetworkRuntime
	caps           guestCapabilities
	env            func(base, overrides []string, replace bool) []string
	user           func(string) (string, error)
	missingRootErr string
	netOnce        sync.Once
	netMu          sync.RWMutex
}

func (i *managedInstance) core() *managedInstanceCore {
	if i == nil {
		return nil
	}
	return hostmanaged.NewCore(hostmanaged.Config{
		OSName:         i.osName,
		Session:        i.session,
		Root:           i.root,
		BaseEnv:        i.baseEnv,
		DefaultUser:    i.defaultUser,
		DefaultRootDir: i.defaultRootDir,
		WorkDir:        i.workDir,
		Capabilities:   i.caps,
		Env:            i.env,
		User:           i.user,
		MissingRootErr: i.missingRootErr,
	})
}

func (i *managedInstance) ManagedCapabilities() guestCapabilities {
	return i.core().ManagedCapabilities()
}

func (i *managedInstance) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	return i.core().Exec(ctx, req)
}

func (i *managedInstance) ExecStream(ctx context.Context, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return i.core().ExecStream(ctx, req, inputs, onEvent)
}

func (i *managedInstance) execRequest(req client.ExecRequest) (client.ExecRequest, error) {
	return i.core().ExecRequest(req)
}

func (i *managedInstance) AddShare(ctx context.Context, share client.ShareMount) error {
	return i.core().AddShare(ctx, share)
}

func (i *managedInstance) AddPortForward(ctx context.Context, forward client.PortForward) error {
	return i.core().AddPortForward(ctx, forward)
}

func (i *managedInstance) Flush(ctx context.Context) error {
	return i.core().Flush(ctx)
}

func (i *managedInstance) ConsoleHistory(ctx context.Context) (string, error) {
	return i.core().ConsoleHistory(ctx)
}

func (i *managedInstance) Wait() error {
	if i == nil {
		return nil
	}
	defer i.closeNetwork()
	return hostmanaged.WaitSession(i.session)
}

func (i *managedInstance) Close() error {
	if i == nil {
		return nil
	}
	return hostmanaged.CloseSession(i.session, func() error {
		i.closeNetwork()
		return nil
	}, i.closeRuntime)
}

func (i *managedInstance) NetworkIPv4() string {
	if i == nil {
		return ""
	}
	i.netMu.RLock()
	defer i.netMu.RUnlock()
	return networkGuestAddress(i.network)
}

func (i *managedInstance) RootSnapshot() (imagefs.Directory, error) {
	return i.RootSnapshotContext(context.Background())
}

func (i *managedInstance) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i == nil {
		return nil, fmt.Errorf("instance is not running")
	}
	if !i.caps.RootSnapshot {
		return nil, i.unsupported("root snapshots")
	}
	if i.root == nil {
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	return i.root, nil
}

func (i *managedInstance) closeNetwork() {
	if i == nil {
		return
	}
	i.netOnce.Do(func() {
		i.netMu.Lock()
		network := i.network
		i.network = nil
		i.netMu.Unlock()
		if network != nil {
			_ = network.Close()
		}
	})
}

func (i *managedInstance) unsupported(feature string) error {
	return i.core().Unsupported(feature)
}
