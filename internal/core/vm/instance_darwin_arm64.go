//go:build darwin && arm64

package vm

import (
	"context"
	"fmt"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/hv/hvf"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	hvfhost "github.com/tinyrange/crumblecracker/internal/core/vm/host/hvf"
	hostmanaged "github.com/tinyrange/crumblecracker/internal/core/vm/host/managed"
	"github.com/tinyrange/crumblecracker/internal/core/vm/mounts"
	"github.com/tinyrange/crumblecracker/internal/core/vm/netstate"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func (i *darwinInstance) SetBalloonMB(target uint64) error {
	if i == nil || i.session == nil {
		return fmt.Errorf("running instance has no managed session")
	}
	return i.session.SetBalloonMB(target)
}

func (i *darwinInstance) BalloonState() (targetMB, actualMB uint64, driverReady, supported bool) {
	if i == nil || i.session == nil {
		return 0, 0, false, false
	}
	target, actual, ready := i.session.BalloonState()
	return target, actual, ready, true
}

type darwinInstance struct {
	*managedInstanceCore
	session     *hvf.ContainerSession
	network     *darwinNetworkRuntime
	imageName   string
	defaultUser string
}

func newDarwinInstance(session *hvf.ContainerSession, network *darwinNetworkRuntime, imageName, defaultUser string) *darwinInstance {
	inst := &darwinInstance{
		session:     session,
		network:     network,
		imageName:   imageName,
		defaultUser: defaultUser,
	}
	inst.managedInstanceCore = newDarwinManagedCore(session, defaultUser)
	return inst
}

func newDarwinManagedCore(session *hvf.ContainerSession, defaultUser string) *managedInstanceCore {
	if session == nil {
		return nil
	}
	metadata := session.ManagedMetadata()
	return hostmanaged.NewCore(hostmanaged.Config{
		OSName:         "Linux",
		Session:        session,
		Root:           metadata.Root,
		BaseEnv:        metadata.BaseEnv,
		DefaultUser:    defaultUser,
		WorkDir:        metadata.WorkDir,
		Capabilities:   managedguest.LinuxProfile.Caps,
		Env:            mergeDarwinManagedEnv,
		MissingRootErr: "running instance does not have a default image root filesystem",
		MarkResolved:   true,
	})
}

func (i *darwinInstance) ManagedCapabilities() guestCapabilities {
	return i.managedCore().ManagedCapabilities()
}

func (i *darwinInstance) managedCore() *managedInstanceCore {
	if i == nil {
		return nil
	}
	if i.managedInstanceCore != nil {
		return i.managedInstanceCore
	}
	return newDarwinManagedCore(i.session, i.defaultUser)
}

func (i *darwinInstance) AddShare(ctx context.Context, share client.ShareMount) error {
	if i == nil {
		return mounts.AddDelegatedRuntimeShare(ctx, nil, share, "runtime shares")
	}
	return mounts.AddDelegatedRuntimeShare(ctx, i.session, share, "runtime shares")
}

func (i *darwinInstance) AddShares(ctx context.Context, shares []client.ShareMount) error {
	if i == nil || i.session == nil {
		return fmt.Errorf("instance does not support atomic multi-share mutation")
	}
	return i.session.AddShares(ctx, shares)
}

func (i *darwinInstance) AddImage(ctx context.Context, mountPath string, image *oci.Image) error {
	if i == nil {
		return mounts.AddDelegatedRuntimeImage(ctx, nil, mountPath, image)
	}
	return mounts.AddDelegatedRuntimeImage(ctx, i.session, mountPath, image)
}

func (i *darwinInstance) AddPortForward(ctx context.Context, forward client.PortForward) error {
	if i == nil {
		return netstate.AddManagedNetworkPortForwardWithFallback(ctx, nil, forward, nil)
	}
	var runtime *networkRuntime
	if i.network != nil {
		runtime = i.network.networkRuntime
	}
	return netstate.AddManagedNetworkPortForwardWithFallback(ctx, runtime, forward, i.session)
}

func (i *darwinInstance) AllowServiceProxyPort(ctx context.Context, port int) error {
	if i == nil || i.network == nil {
		return netstate.AllowManagedNetworkServiceProxyPort(ctx, nil, port)
	}
	return netstate.AllowManagedNetworkServiceProxyPort(ctx, i.network.networkRuntime, port)
}

func (i *darwinInstance) VirtioFSStats() []virtio.FSStats {
	if i == nil || i.session == nil {
		return nil
	}
	return i.session.VirtioFSStats()
}

func (i *darwinInstance) BackingUsage() (uint64, uint64, uint64, error) {
	if i == nil || i.session == nil {
		return 0, 0, 0, nil
	}
	return i.session.BackingUsage()
}

func (i *darwinInstance) BackingMetadataUsage() (uint64, uint64) {
	if i == nil || i.session == nil {
		return 0, 0
	}
	return i.session.BackingMetadataUsage()
}

func (i *darwinInstance) BackingCombinedUsage() (uint64, uint64) {
	usage := i.BackingSnapshot()
	return usage.CombinedBytes, usage.CombinedHighWaterBytes
}

func (i *darwinInstance) BackingSnapshot() virtio.FSBackingUsageSnapshot {
	if i == nil || i.session == nil {
		return virtio.FSBackingUsageSnapshot{}
	}
	return i.session.BackingSnapshot()
}

func (i *darwinInstance) PersistentFSStatus() []virtio.PersistentFSStatus {
	if i == nil || i.session == nil {
		return nil
	}
	return i.session.PersistentFSStatus()
}

func (i *darwinInstance) Desktop() *virtio.Desktop {
	if i == nil || i.session == nil {
		return nil
	}
	return i.session.Desktop()
}

func (i *darwinInstance) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	return i.managedCore().Exec(ctx, req)
}

func (i *darwinInstance) ExecStream(
	ctx context.Context,
	req client.ExecRequest,
	inputs <-chan client.ExecInput,
	onEvent func(client.ExecEvent) error,
) error {
	return i.managedCore().ExecStream(ctx, req, inputs, onEvent)
}

func (i *darwinInstance) Flush(ctx context.Context) error {
	return i.managedCore().Flush(ctx)
}

func (i *darwinInstance) ConsoleHistory(ctx context.Context) (string, error) {
	return i.managedCore().ConsoleHistory(ctx)
}

func (i *darwinInstance) RootSnapshot() (imagefs.Directory, error) {
	return i.RootSnapshotContext(context.Background())
}

func (i *darwinInstance) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	if i == nil || i.session == nil {
		return mounts.RootSnapshot(nil, "")
	}
	if !i.ManagedCapabilities().RootSnapshot {
		return mounts.RootSnapshotWithCapabilities("Linux", i.ManagedCapabilities(), i.session, "")
	}
	return i.session.RootSnapshotContext(ctx)
}

func (i *darwinInstance) SnapshotImage(imageName string) (imagefs.Directory, error) {
	return i.SnapshotImageContext(context.Background(), imageName)
}

func (i *darwinInstance) SnapshotImageContext(ctx context.Context, imageName string) (imagefs.Directory, error) {
	if i == nil || i.session == nil {
		return mounts.RootSnapshot(nil, "")
	}
	if strings.TrimSpace(i.imageName) == imageName {
		return i.RootSnapshotContext(ctx)
	}
	return mounts.ImageSnapshotContextWithCapabilities(ctx, "Linux", i.ManagedCapabilities(), i.session, imageName, hvfhost.ImageMountPath(imageName))
}

func (i *darwinInstance) Wait() error {
	if i == nil {
		return nil
	}
	return i.managedCore().Wait()
}

func (i *darwinInstance) Close() error {
	if i == nil {
		return nil
	}
	if i.network == nil {
		return hostmanaged.CloseSession(i.session)
	}
	return hostmanaged.CloseSessionWithNetwork(i.session, i.network)
}

func (i *darwinInstance) NetworkIPv4() string {
	if i == nil || i.network == nil {
		return ""
	}
	return netstate.IPv4(i.network.networkRuntime, "")
}

func darwinContainerSession(inst Instance) (*hvf.ContainerSession, bool) {
	if session, ok := inst.(*hvf.ContainerSession); ok {
		return session, true
	}
	if wrapped, ok := inst.(*darwinInstance); ok && wrapped.session != nil {
		return wrapped.session, true
	}
	return nil, false
}

func networkDeviceDarwin(network *darwinNetworkRuntime) *virtio.Net {
	if network == nil {
		return nil
	}
	return network.dev
}

func mergeDarwinManagedEnv(base, overrides []string, replace bool) []string {
	if replace {
		return vmruntime.WithDefaultEnv(overrides)
	}
	return vmruntime.WithDefaultEnv(vmruntime.MergeEnv(base, overrides))
}
