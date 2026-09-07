package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type Capabilities struct {
	Backend         string
	MaxVMs          int
	Locality        string
	SupportsFSRPC   bool
	SupportsL2      bool
	SupportsDisplay bool
}

type Backend interface {
	Start(context.Context, client.CreateInstanceRequest) (Instance, error)
	StartStream(context.Context, client.CreateInstanceRequest, func(client.BootEvent) error) (Instance, error)
	StartBlank(context.Context, client.StartInstanceRequest) (Instance, error)
	StartBlankStream(context.Context, client.StartInstanceRequest, func(client.BootEvent) error) (Instance, error)
	Run(context.Context, client.RunRequest) (client.ExecResponse, error)
	RunStream(context.Context, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
	RunInInstance(context.Context, Instance, string, client.RunRequest) (client.ExecResponse, error)
	RunInInstanceStream(context.Context, Instance, string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
	ExecInInstanceStream(context.Context, Instance, string, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
}

type VMHost interface {
	Backend
	HostCapabilities(context.Context) Capabilities
	Close() error
}

type Instance interface {
	AddShare(context.Context, client.ShareMount) error
	AddPortForward(context.Context, client.PortForward) error
	Exec(context.Context, client.ExecRequest) (client.ExecResponse, error)
	ExecStream(context.Context, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
	Wait() error
	Close() error
}

type virtioFSStatsProvider interface {
	VirtioFSStats() []virtio.FSStats
}

type networkIPv4Provider interface {
	NetworkIPv4() string
}

type serviceProxyPortAllower interface {
	AllowServiceProxyPort(context.Context, int) error
}

type instanceFlushProvider interface {
	Flush(context.Context) error
}

type consoleHistoryProvider interface {
	ConsoleHistory(context.Context) (string, error)
}

type rootSnapshotProvider interface {
	RootSnapshot() (imagefs.Directory, error)
}

type rootSnapshotContextProvider interface {
	RootSnapshotContext(context.Context) (imagefs.Directory, error)
}

type imageSnapshotProvider interface {
	SnapshotImage(string) (imagefs.Directory, error)
}

type imageSnapshotContextProvider interface {
	SnapshotImageContext(context.Context, string) (imagefs.Directory, error)
}

type inProcessVMHost struct {
	backend      Backend
	capabilities func() client.CapabilitiesResponse
}

func NewInProcess(backend Backend, capabilities func() client.CapabilitiesResponse) VMHost {
	if backend == nil {
		backend = UnsupportedBackend{}
	}
	return &inProcessVMHost{backend: backend, capabilities: capabilities}
}

func (h *inProcessVMHost) HostCapabilities(context.Context) Capabilities {
	caps := client.CapabilitiesResponse{}
	if h.capabilities != nil {
		caps = h.capabilities()
	}
	return Capabilities{
		Backend:         caps.Backend,
		MaxVMs:          caps.MaxInstances,
		Locality:        "in-process",
		SupportsFSRPC:   false,
		SupportsL2:      len(caps.NetworkModes) > 0,
		SupportsDisplay: caps.SupportsDisplay,
	}
}

func (h *inProcessVMHost) Close() error {
	return nil
}

func (h *inProcessVMHost) Start(ctx context.Context, req client.CreateInstanceRequest) (Instance, error) {
	return h.backend.Start(ctx, req)
}

func (h *inProcessVMHost) StartStream(ctx context.Context, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	return h.backend.StartStream(ctx, req, onEvent)
}

func (h *inProcessVMHost) StartBlank(ctx context.Context, req client.StartInstanceRequest) (Instance, error) {
	return h.backend.StartBlank(ctx, req)
}

func (h *inProcessVMHost) StartBlankStream(ctx context.Context, req client.StartInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	return h.backend.StartBlankStream(ctx, req, onEvent)
}

func (h *inProcessVMHost) Run(ctx context.Context, req client.RunRequest) (client.ExecResponse, error) {
	return h.backend.Run(ctx, req)
}

func (h *inProcessVMHost) RunStream(ctx context.Context, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return h.backend.RunStream(ctx, req, inputs, onEvent)
}

func (h *inProcessVMHost) RunInInstance(ctx context.Context, inst Instance, runningImage string, req client.RunRequest) (client.ExecResponse, error) {
	return h.backend.RunInInstance(ctx, inst, runningImage, req)
}

func (h *inProcessVMHost) RunInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return h.backend.RunInInstanceStream(ctx, inst, runningImage, req, inputs, onEvent)
}

func (h *inProcessVMHost) ExecInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return h.backend.ExecInInstanceStream(ctx, inst, runningImage, req, inputs, onEvent)
}

type placementVMHost struct {
	mu      sync.Mutex
	hosts   []VMHost
	running map[VMHost]int
}

func NewPlacement(hosts ...VMHost) VMHost {
	filtered := make([]VMHost, 0, len(hosts))
	for _, host := range hosts {
		if host != nil {
			filtered = append(filtered, host)
		}
	}
	if len(filtered) == 0 {
		filtered = append(filtered, NewInProcess(UnsupportedBackend{}, nil))
	}
	return &placementVMHost{hosts: filtered, running: map[VMHost]int{}}
}

func (h *placementVMHost) HostCapabilities(ctx context.Context) Capabilities {
	caps := Capabilities{
		Backend:  "placement",
		Locality: "mixed",
	}
	allLimited := true
	for _, host := range h.hosts {
		hostCaps := host.HostCapabilities(ctx)
		if hostCaps.MaxVMs <= 0 {
			allLimited = false
		} else if allLimited {
			caps.MaxVMs += hostCaps.MaxVMs
		}
		caps.SupportsFSRPC = caps.SupportsFSRPC || hostCaps.SupportsFSRPC
		caps.SupportsL2 = caps.SupportsL2 || hostCaps.SupportsL2
		caps.SupportsDisplay = caps.SupportsDisplay || hostCaps.SupportsDisplay
	}
	if !allLimited {
		caps.MaxVMs = 0
	}
	return caps
}

func (h *placementVMHost) Close() error {
	var errs []error
	for _, host := range h.hosts {
		if err := host.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *placementVMHost) Start(ctx context.Context, req client.CreateInstanceRequest) (Instance, error) {
	return h.StartStream(ctx, req, nil)
}

func (h *placementVMHost) StartStream(ctx context.Context, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	host, err := h.reserveHost(ctx, placementRequirements{
		requiresL2:      networkEnabled(req.Network) && !isBuiltinBSDImageName(req.Image),
		requiresDisplay: req.Display != nil,
	})
	if err != nil {
		return nil, err
	}
	inst, err := host.StartStream(ctx, req, onEvent)
	if err != nil {
		h.releaseHost(host)
		return nil, err
	}
	return &hostedInstance{Instance: inst, host: host, release: func() { h.releaseHost(host) }}, nil
}

func (h *placementVMHost) StartBlank(ctx context.Context, req client.StartInstanceRequest) (Instance, error) {
	return h.StartBlankStream(ctx, req, nil)
}

func (h *placementVMHost) StartBlankStream(ctx context.Context, req client.StartInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	host, err := h.reserveHost(ctx, placementRequirements{
		requiresL2:      networkEnabled(req.Network) && !isBuiltinBSDImageName(req.Image),
		requiresDisplay: req.Display != nil,
	})
	if err != nil {
		return nil, err
	}
	inst, err := host.StartBlankStream(ctx, req, onEvent)
	if err != nil {
		h.releaseHost(host)
		return nil, err
	}
	return &hostedInstance{Instance: inst, host: host, release: func() { h.releaseHost(host) }}, nil
}

func (h *placementVMHost) Run(ctx context.Context, req client.RunRequest) (client.ExecResponse, error) {
	host, err := h.firstHost(ctx)
	if err != nil {
		return client.ExecResponse{}, err
	}
	return host.Run(ctx, req)
}

func (h *placementVMHost) RunStream(ctx context.Context, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	host, err := h.firstHost(ctx)
	if err != nil {
		return err
	}
	return host.RunStream(ctx, req, inputs, onEvent)
}

func (h *placementVMHost) RunInInstance(ctx context.Context, inst Instance, runningImage string, req client.RunRequest) (client.ExecResponse, error) {
	host, inner, err := h.instanceHost(ctx, inst)
	if err != nil {
		return client.ExecResponse{}, err
	}
	return host.RunInInstance(ctx, inner, runningImage, req)
}

func (h *placementVMHost) RunInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	host, inner, err := h.instanceHost(ctx, inst)
	if err != nil {
		return err
	}
	return host.RunInInstanceStream(ctx, inner, runningImage, req, inputs, onEvent)
}

func (h *placementVMHost) ExecInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	host, inner, err := h.instanceHost(ctx, inst)
	if err != nil {
		return err
	}
	return host.ExecInInstanceStream(ctx, inner, runningImage, req, inputs, onEvent)
}

type placementRequirements struct {
	requiresL2      bool
	requiresDisplay bool
}

func (h *placementVMHost) reserveHost(ctx context.Context, req placementRequirements) (VMHost, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var skipped []string
	for _, host := range h.hosts {
		caps := host.HostCapabilities(ctx)
		if req.requiresL2 && !caps.SupportsL2 {
			skipped = append(skipped, firstNonEmpty(caps.Locality, caps.Backend, "unknown"))
			continue
		}
		if req.requiresDisplay && !caps.SupportsDisplay {
			skipped = append(skipped, firstNonEmpty(caps.Locality, caps.Backend, "unknown"))
			continue
		}
		if caps.MaxVMs <= 0 || h.running[host] < caps.MaxVMs {
			h.running[host]++
			return host, nil
		}
	}
	if req.requiresL2 && len(skipped) != 0 {
		return nil, fmt.Errorf("no VM host with coordinator L2 capacity available")
	}
	if req.requiresDisplay && len(skipped) != 0 {
		return nil, fmt.Errorf("no VM host with graphical display support available")
	}
	return nil, fmt.Errorf("no VM host capacity available")
}

func networkEnabled(cfg *client.NetworkConfig) bool {
	return cfg != nil && cfg.Enabled
}

func (h *placementVMHost) releaseHost(host VMHost) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running[host] > 0 {
		h.running[host]--
	}
}

func (h *placementVMHost) firstHost(context.Context) (VMHost, error) {
	if len(h.hosts) == 0 {
		return nil, fmt.Errorf("no VM hosts configured")
	}
	return h.hosts[0], nil
}

type hostedInstance struct {
	Instance
	host    VMHost
	release func()
	once    sync.Once
}

func IsHostedInstance(inst Instance) bool {
	_, ok := inst.(*hostedInstance)
	return ok
}

func (i *hostedInstance) Wait() error {
	err := i.Instance.Wait()
	i.once.Do(i.release)
	return err
}

func (i *hostedInstance) Close() error {
	err := i.Instance.Close()
	i.once.Do(i.release)
	return err
}

func (i *hostedInstance) Flush(ctx context.Context) error {
	flusher, ok := i.Instance.(instanceFlushProvider)
	if !ok {
		return fmt.Errorf("root filesystem cannot be flushed")
	}
	return flusher.Flush(ctx)
}

func (i *hostedInstance) ConsoleHistory(ctx context.Context) (string, error) {
	provider, ok := i.Instance.(consoleHistoryProvider)
	if !ok {
		return "", fmt.Errorf("console history is not available")
	}
	return provider.ConsoleHistory(ctx)
}

func (i *hostedInstance) RootSnapshot() (imagefs.Directory, error) {
	snapshotter, ok := i.Instance.(rootSnapshotProvider)
	if !ok {
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	return snapshotter.RootSnapshot()
}

func (i *hostedInstance) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	snapshotter, ok := i.Instance.(rootSnapshotContextProvider)
	if !ok {
		return nil, fmt.Errorf("root filesystem does not support cancelable snapshots")
	}
	return snapshotter.RootSnapshotContext(ctx)
}

func (i *hostedInstance) SnapshotImage(imageName string) (imagefs.Directory, error) {
	snapshotter, ok := i.Instance.(imageSnapshotProvider)
	if !ok {
		return nil, fmt.Errorf("image %q cannot be snapshotted", imageName)
	}
	return snapshotter.SnapshotImage(imageName)
}

func (i *hostedInstance) SnapshotImageContext(ctx context.Context, imageName string) (imagefs.Directory, error) {
	snapshotter, ok := i.Instance.(imageSnapshotContextProvider)
	if !ok {
		return nil, fmt.Errorf("image %q does not support cancelable snapshots", imageName)
	}
	return snapshotter.SnapshotImageContext(ctx, imageName)
}

func (i *hostedInstance) NetworkIPv4() string {
	provider, ok := i.Instance.(networkIPv4Provider)
	if !ok {
		return ""
	}
	return provider.NetworkIPv4()
}

func (i *hostedInstance) VirtioFSStats() []virtio.FSStats {
	provider, ok := i.Instance.(virtioFSStatsProvider)
	if !ok {
		return nil
	}
	return provider.VirtioFSStats()
}

func (i *hostedInstance) Desktop() *virtio.Desktop {
	provider, ok := i.Instance.(interface{ Desktop() *virtio.Desktop })
	if !ok {
		return nil
	}
	return provider.Desktop()
}

func (i *hostedInstance) AllowServiceProxyPort(ctx context.Context, port int) error {
	allower, ok := i.Instance.(serviceProxyPortAllower)
	if !ok {
		return fmt.Errorf("network does not support service proxy port updates")
	}
	return allower.AllowServiceProxyPort(ctx, port)
}

func (i *hostedInstance) SetBalloonMB(target uint64) error {
	controller, ok := i.Instance.(interface{ SetBalloonMB(uint64) error })
	if !ok {
		return fmt.Errorf("dynamic ballooning is unsupported")
	}
	return controller.SetBalloonMB(target)
}

func (i *hostedInstance) BalloonState() (targetMB, actualMB uint64, driverReady, supported bool) {
	provider, ok := i.Instance.(interface {
		BalloonState() (uint64, uint64, bool, bool)
	})
	if !ok {
		return 0, 0, false, false
	}
	return provider.BalloonState()
}

func (i *hostedInstance) BackingUsage() (uint64, uint64, uint64, error) {
	provider, ok := i.Instance.(interface {
		BackingUsage() (uint64, uint64, uint64, error)
	})
	if !ok {
		return 0, 0, 0, nil
	}
	return provider.BackingUsage()
}

func (i *hostedInstance) BackingMetadataUsage() (uint64, uint64) {
	provider, ok := i.Instance.(interface{ BackingMetadataUsage() (uint64, uint64) })
	if !ok {
		return 0, 0
	}
	return provider.BackingMetadataUsage()
}

func (i *hostedInstance) BackingCombinedUsage() (uint64, uint64) {
	provider, ok := i.Instance.(interface{ BackingCombinedUsage() (uint64, uint64) })
	if ok {
		return provider.BackingCombinedUsage()
	}
	data, dataHigh, _, _ := i.BackingUsage()
	metadata, metadataHigh := i.BackingMetadataUsage()
	current := data + metadata
	if current < data {
		current = ^uint64(0)
	}
	return current, max(current, dataHigh, metadataHigh)
}

func (i *hostedInstance) BackingSnapshot() virtio.FSBackingUsageSnapshot {
	if provider, ok := i.Instance.(interface {
		BackingSnapshot() virtio.FSBackingUsageSnapshot
	}); ok {
		return provider.BackingSnapshot()
	}
	data, dataHigh, physical, err := i.BackingUsage()
	metadata, metadataHigh := i.BackingMetadataUsage()
	combined := data + metadata
	if combined < data {
		combined = ^uint64(0)
	}
	return virtio.FSBackingUsageSnapshot{
		DataBytes: data, DataHighWaterBytes: dataHigh,
		MetadataBytes: metadata, MetadataHighWaterBytes: metadataHigh,
		CombinedBytes: combined, CombinedHighWaterBytes: max(combined, dataHigh, metadataHigh),
		PhysicalBytes: physical, ReclaimError: err,
	}
}

func (i *hostedInstance) PersistentFSStatus() []virtio.PersistentFSStatus {
	provider, ok := i.Instance.(interface {
		PersistentFSStatus() []virtio.PersistentFSStatus
	})
	if !ok {
		return nil
	}
	return provider.PersistentFSStatus()
}

func (h *placementVMHost) instanceHost(ctx context.Context, inst Instance) (VMHost, Instance, error) {
	if hosted, ok := inst.(*hostedInstance); ok {
		return hosted.host, hosted.Instance, nil
	}
	host, err := h.firstHost(ctx)
	if err != nil {
		return nil, nil, err
	}
	return host, inst, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func isBuiltinBSDImageName(image string) bool {
	switch strings.ToLower(strings.TrimSpace(image)) {
	case "@openbsd", "openbsd", "@freebsd", "freebsd", "@netbsd", "netbsd":
		return true
	default:
		return false
	}
}
