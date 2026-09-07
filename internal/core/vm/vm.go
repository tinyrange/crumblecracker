package vm

import (
	"context"
	"errors"
	"fmt"
	"image"
	"net"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/hv"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/rfb"
	"github.com/tinyrange/crumblecracker/internal/core/shmem"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/display"
	"github.com/tinyrange/crumblecracker/internal/protocol"

	vmhost "github.com/tinyrange/crumblecracker/internal/core/vm/host"
	"github.com/tinyrange/crumblecracker/internal/core/vm/mounts"
)

const DefaultInstanceID = "default"
const maxExitTombstones = 64
const minimumGuestMemoryMB = 128
const maxLifecycleDiagnosticBytes = 4096

var ErrManagerClosing = errors.New("VM manager is shutting down")

type Backend = vmhost.Backend

type VMHost = vmhost.VMHost

type VMHandle interface {
	Instance
}

type Instance = vmhost.Instance

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

type instanceBalloonController interface {
	SetBalloonMB(uint64) error
}

type instanceBalloonStateProvider interface {
	BalloonState() (targetMB, actualMB uint64, driverReady, supported bool)
}

type instanceBackingUsageProvider interface {
	BackingUsage() (uint64, uint64, uint64, error)
}

type instanceBackingMetadataUsageProvider interface {
	BackingMetadataUsage() (uint64, uint64)
}

type instanceBackingCombinedUsageProvider interface {
	BackingCombinedUsage() (uint64, uint64)
}

type instanceBackingSnapshotProvider interface {
	BackingSnapshot() virtio.FSBackingUsageSnapshot
}

type instancePersistentFSStatusProvider interface {
	PersistentFSStatus() []virtio.PersistentFSStatus
}

type instanceDesktopProvider interface {
	Desktop() *virtio.Desktop
}

type Manager struct {
	mu            sync.Mutex
	host          VMHost
	supports      func() error
	capabilities  func() client.CapabilitiesResponse
	running       map[string]*Machine
	starting      map[string]*managerStart
	networkLeases map[string]managerNetworkLease
	exited        map[string]client.InstanceState
	reservations  map[string]resourceReservation
	maxMemoryMB   uint64
	maxCPUs       int
	closing       bool
	sharedMemory  *shmem.Registry
	hostMounts    func(context.Context, string) ([]virtio.ShareMount, error)
}

// SetHostMountProvider configures read-only host-managed filesystems that are
// attached to every newly started instance. It must be called before starts
// are accepted.
func (m *Manager) SetHostMountProvider(provider func(context.Context, string) ([]virtio.ShareMount, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostMounts = provider
}

type resourceReservation struct {
	memoryMB uint64
	cpus     int
}

type managerStart struct {
	cancel     context.CancelFunc
	done       chan struct{}
	cleanupErr error
}

type Machine struct {
	lifecycleMu              sync.Mutex
	balloonMu                sync.Mutex
	backingMu                sync.Mutex
	snapshotMu               sync.Mutex
	snapshotCancel           context.CancelFunc
	backingHighWater         uint64
	backingDataHighWater     uint64
	backingMetadataHighWater uint64
	id                       string
	image                    string
	initSystem               string
	kernel                   string
	memoryMB                 uint64
	balloonMB                uint64
	cpus                     int
	nestedVirt               bool
	sharedMemoryConfig       *client.SharedMemoryConfig
	startedAt                time.Time
	instance                 Instance
	lastErr                  error
	exitedAt                 time.Time
	stopping                 bool
	stop                     *machineStopOperation
	display                  *client.DisplayState
	vncListener              net.Listener
	vncServer                *rfb.Server
	sharedMemory             *shmem.Attachment
}

type machineStopOperation struct {
	done      chan struct{}
	err       error
	observers int
}

type managerNetworkLease struct {
	ip  net.IP
	mac net.HardwareAddr
}

func NewManager() *Manager {
	return NewManagerWithBackend(vmhost.UnsupportedBackend{})
}

func NewManagerWithBackend(backend Backend) *Manager {
	m := newManagerBudgets(&Manager{supports: Supports, capabilities: HostCapabilities})
	m.host = vmhost.NewInProcess(backend, HostCapabilities)
	return m
}

func NewManagerWithHost(host VMHost) *Manager {
	if host == nil {
		host = vmhost.NewInProcess(vmhost.UnsupportedBackend{}, HostCapabilities)
	}
	return newManagerBudgets(&Manager{host: host, supports: Supports, capabilities: HostCapabilities})
}

func newManagerBudgets(m *Manager) *Manager {
	// Guest memory is sparsely committed and vCPUs are scheduler work, so
	// configured totals are not useful host-capacity ceilings. The vmsh host
	// observes real memory pressure and dynamically balloons guests instead.
	// Explicit test/embedding budgets can still set these fields when desired.
	if m.sharedMemory == nil {
		m.sharedMemory = shmem.NewRegistry()
	}
	return m
}

func NewManagerWithHosts(hosts ...VMHost) *Manager {
	return NewManagerWithHost(vmhost.NewPlacement(hosts...))
}

func validateSharedMemoryRequest(config *client.SharedMemoryConfig) error {
	if config == nil {
		return nil
	}
	return shmem.ValidateConfig(shmem.Config{Domain: config.Domain, PhysAddr: config.PhysAddr})
}

func cloneSharedMemoryConfig(config *client.SharedMemoryConfig) *client.SharedMemoryConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func (m *Manager) attachSharedMemory(config *client.SharedMemoryConfig) (*shmem.Attachment, error) {
	if config == nil {
		return nil, nil
	}
	return m.sharedMemory.Attach(shmem.Config{Domain: config.Domain, PhysAddr: config.PhysAddr})
}

func Supports() error {
	return hv.Supports()
}

func HostCapabilities() client.CapabilitiesResponse {
	host := runtime.GOOS + "/" + runtime.GOARCH
	caps := client.CapabilitiesResponse{
		Host:                   host,
		Backend:                backendName(),
		MaxInstances:           64,
		SnapshotClasses:        []string{},
		NetworkModes:           networkModesForHost(runtime.GOOS, runtime.GOARCH),
		ShareConsistency:       []string{"host-backed"},
		ResourceLimits:         resourceLimitsForHost(runtime.GOOS, runtime.GOARCH),
		SupportsMultiImageExec: true,
		RequiresPrivilegedCCX3: false,
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		caps.MaxInstances = 1
		caps.Notes = append(caps.Notes, "macOS HVF currently limits ccx3 to one running instance")
	}
	if (runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")) ||
		(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") ||
		(runtime.GOOS == "windows" && runtime.GOARCH == "amd64") {
		caps.SupportsDisplay = true
	}
	if runtime.GOOS == "windows" && runtime.GOARCH == "arm64" {
		caps.MaxInstances = 1
		caps.Notes = append(caps.Notes, "Windows WHP currently supports one vCPU per instance")
	} else if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		caps.MaxInstances = 1
	}
	if supported, err := hv.NestedVirtualizationSupported(); err == nil && supported {
		caps.SupportsNestedVirt = true
		caps.ResourceLimits = append(caps.ResourceLimits, "nested_virtualization")
	}
	if err := Supports(); err != nil {
		caps.VMSupported = false
		caps.VMError = err.Error()
	} else {
		caps.VMSupported = true
	}
	return caps
}

func resourceLimitsForHost(goos, goarch string) []string {
	limits := []string{"memory_mb"}
	switch {
	case goos == "linux" && goarch == "amd64":
		limits = append(limits, "cpus")
	case goos == "darwin" && goarch == "arm64":
		limits = append(limits, "cpus")
	case goos == "windows" && goarch == "amd64":
		limits = append(limits, "cpus")
	}
	return limits
}

func networkModesForHost(goos, goarch string) []string {
	switch {
	case goos == "linux" && goarch == "amd64":
		return []string{"user"}
	case goos == "darwin" && goarch == "arm64":
		return []string{"user"}
	case goos == "windows" && (goarch == "amd64" || goarch == "arm64"):
		return []string{"user"}
	default:
		return []string{}
	}
}

func backendName() string {
	switch {
	case runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"):
		return "kvm"
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "hvf"
	case runtime.GOOS == "windows" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"):
		return "whp"
	default:
		return "unsupported"
	}
}

func (m *Manager) Start(ctx context.Context, req client.CreateInstanceRequest) (client.InstanceState, error) {
	return m.StartStream(ctx, req, nil)
}

func (m *Manager) StartStream(ctx context.Context, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (client.InstanceState, error) {
	id := instanceID(req.ID)
	return m.StartInstanceStream(ctx, id, req, onEvent)
}

func normalizeDisplayConfig(config *client.DisplayConfig) (*client.DisplayConfig, error) {
	if config == nil {
		return nil, nil
	}
	normalized := *config
	if normalized.Width == 0 {
		normalized.Width = 1280
	}
	if normalized.Height == 0 {
		normalized.Height = 720
	}
	if normalized.Width > 8192 || normalized.Height > 8192 {
		return nil, fmt.Errorf("display dimensions %dx%d exceed 8192x8192", normalized.Width, normalized.Height)
	}
	address := strings.TrimSpace(normalized.VNCListen)
	if address == "" {
		normalized.VNCListen = ""
		normalized.VNCPassword = ""
		return &normalized, nil
	}
	if len(normalized.VNCPassword) > 8 {
		return nil, fmt.Errorf("VNC passwords are limited to 8 bytes")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid VNC listen address %q: %w", address, err)
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("VNC v1 only supports a loopback listen address, got %q", address)
	}
	normalized.VNCListen = net.JoinHostPort(host, port)
	return &normalized, nil
}

func startVNCServer(instance Instance, id string, config *client.DisplayConfig) (*client.DisplayState, net.Listener, *rfb.Server, error) {
	if config == nil {
		return nil, nil, nil, nil
	}
	provider, ok := instance.(instanceDesktopProvider)
	if !ok || provider.Desktop() == nil {
		return nil, nil, nil, fmt.Errorf("VM backend does not support graphical displays")
	}
	state := &client.DisplayState{
		Width:  config.Width,
		Height: config.Height,
	}
	if config.VNCListen == "" {
		return state, nil, nil, nil
	}
	listener, err := net.Listen("tcp", config.VNCListen)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listen for VNC on %s: %w", config.VNCListen, err)
	}
	server := &rfb.Server{Desktop: provider.Desktop(), Name: "cc " + id}
	if config.VNCPassword != "" {
		server.Security = rfb.PasswordSecurity(config.VNCPassword)
	}
	go func() {
		_ = server.Serve(listener)
	}()
	state.VNCAddress = listener.Addr().String()
	return state, listener, server, nil
}

// Display returns a direct frontend session for a running graphical VM.
func (m *Manager) Display(id string) (display.Session, bool) {
	id = instanceID(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	machine := m.running[id]
	if machine == nil || machine.stopping {
		return nil, false
	}
	provider, ok := machine.instance.(instanceDesktopProvider)
	if !ok || provider.Desktop() == nil {
		return nil, false
	}
	return desktopSession{desktop: provider.Desktop()}, true
}

type desktopSession struct {
	desktop *virtio.Desktop
}

func (s desktopSession) Size() (int, int) {
	return s.desktop.Framebuffer.Size()
}

func (s desktopSession) Snapshot(request image.Rectangle, since uint64, incremental bool) display.FramebufferUpdate {
	update, err := s.desktop.Snapshot(request, since, incremental)
	if err != nil {
		return display.FramebufferUpdate{}
	}
	return display.FramebufferUpdate{
		Width: update.Width, Height: update.Height, Generation: update.Generation,
		Rect: update.Rect, Pixels: update.Pixels,
	}
}

func (s desktopSession) AcquireOpenGLFrame(since uint64) (display.OpenGLFrame, bool, error) {
	frame, available, err := s.desktop.AcquireNativeFrame(since)
	if err != nil || !available {
		return display.OpenGLFrame{}, available, err
	}
	return display.NewOpenGLFrame(
		frame.Width, frame.Height, frame.Generation, frame.Damage,
		frame.Texture, frame.ProducerFence, frame.ReleaseFrame,
	), true, nil
}

func (s desktopSession) Changed() <-chan struct{} {
	return s.desktop.Framebuffer.Changed()
}

func (s desktopSession) Cursor() display.CursorUpdate {
	if s.desktop.GPU == nil || s.desktop.GPU.Cursor() == nil {
		return display.CursorUpdate{}
	}
	update := s.desktop.GPU.Cursor().Snapshot()
	return display.CursorUpdate{
		Width: update.Width, Height: update.Height, HotX: update.HotX, HotY: update.HotY,
		Visible: update.Visible, Generation: update.Generation, Pixels: update.Pixels,
	}
}

func (s desktopSession) Resize(width, height int) error {
	return s.desktop.Resize(width, height)
}

func (s desktopSession) Key(code uint16, down bool) error {
	if s.desktop.Keyboard == nil {
		return nil
	}
	return s.desktop.Keyboard.Key(code, down)
}

func (s desktopSession) Pointer(x, y uint32, buttons, previousButtons uint8) error {
	if s.desktop.Pointer == nil {
		return nil
	}
	return s.desktop.Pointer.PointerEvent(x, y, buttons, previousButtons)
}

func (s desktopSession) Scroll(deltaX120, deltaY120 int32) error {
	if s.desktop.Pointer == nil {
		return nil
	}
	return s.desktop.Pointer.ScrollEvent(deltaX120, deltaY120)
}

func (s desktopSession) SetClipboard(text string) {
	if s.desktop.Clipboard != nil {
		s.desktop.Clipboard.SetFromFrontend(text)
	}
}

func (s desktopSession) GuestClipboard() (string, uint64) {
	if s.desktop.Clipboard == nil {
		return "", 0
	}
	return s.desktop.Clipboard.GuestSnapshot()
}

func (m *Manager) StartInstanceStream(ctx context.Context, id string, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (client.InstanceState, error) {
	id = instanceID(id)
	req.ID = id
	if req.Image == "" {
		return client.InstanceState{}, fmt.Errorf("image is required")
	}
	if err := normalizeResources(&req.MemoryMB, &req.BalloonMB, &req.CPUs); err != nil {
		return client.InstanceState{}, err
	}
	display, err := normalizeDisplayConfig(req.Display)
	if err != nil {
		return client.InstanceState{}, err
	}
	req.Display = display
	if display != nil && (strings.TrimSpace(req.SnapshotDir) != "" || strings.TrimSpace(req.RestoreSnapshot) != "") {
		return client.InstanceState{}, fmt.Errorf("display-enabled VMs do not support startup snapshots")
	}
	if req.SharedMemory != nil && (strings.TrimSpace(req.SnapshotDir) != "" || strings.TrimSpace(req.RestoreSnapshot) != "") {
		return client.InstanceState{}, fmt.Errorf("shared-memory VMs do not support startup snapshots")
	}
	if err := validateSharedMemoryRequest(req.SharedMemory); err != nil {
		return client.InstanceState{}, err
	}
	canonicalShares, err := mounts.CanonicalRuntimeShares(req.Shares)
	if err != nil {
		return client.InstanceState{}, err
	}
	req.Shares = canonicalShares
	if err := m.supports(); err != nil {
		return client.InstanceState{}, err
	}
	maxVMs := m.host.HostCapabilities(ctx).MaxVMs

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return client.InstanceState{}, ErrManagerClosing
	}
	if m.running == nil {
		m.running = make(map[string]*Machine)
	}
	if m.starting == nil {
		m.starting = make(map[string]*managerStart)
	}
	if m.running[id] != nil {
		snapshot := m.statusSnapshotLocked(id)
		m.mu.Unlock()
		return m.resolveStatusSnapshot(snapshot), fmt.Errorf("VM %q is already running", id)
	}
	if _, ok := m.starting[id]; ok {
		snapshot := m.statusSnapshotLocked(id)
		m.mu.Unlock()
		return m.resolveStatusSnapshot(snapshot), fmt.Errorf("VM %q is already starting", id)
	}
	if err := m.checkCapacityLocked(maxVMs, req.MemoryMB, req.CPUs); err != nil {
		m.mu.Unlock()
		return client.InstanceState{}, err
	}
	delete(m.exited, id)
	if m.reservations == nil {
		m.reservations = make(map[string]resourceReservation)
	}
	m.reservations[id] = resourceReservation{memoryMB: req.MemoryMB, cpus: req.CPUs}
	startCtx, cancelStart := context.WithCancel(ctx)
	start := &managerStart{cancel: cancelStart, done: make(chan struct{})}
	m.starting[id] = start
	req.Network = m.ensureNetworkLeaseLocked(id, req.Image, req.Network)
	m.mu.Unlock()

	attachment, err := m.attachSharedMemory(req.SharedMemory)
	if err != nil {
		m.finishStart(id, start)
		return client.InstanceState{}, err
	}
	if attachment != nil {
		startCtx = shmem.WithAttachment(startCtx, attachment)
	}
	startCtx, err = m.prepareHostMountContext(startCtx, id)
	if err != nil {
		_ = attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, err
	}
	inst, err := m.host.StartStream(startCtx, req, onEvent)
	if err != nil {
		_ = attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, err
	}
	if attachment != nil && !attachment.Claimed() {
		closeErr := inst.Close()
		releaseErr := attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, errors.Join(fmt.Errorf("VM backend does not support shared memory"), closeErr, releaseErr)
	}
	displayState, vncListener, vncServer, err := startVNCServer(inst, id, display)
	if err != nil {
		closeErr := inst.Close()
		releaseErr := attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, errors.Join(err, closeErr, releaseErr)
	}

	machine := &Machine{
		id:                 id,
		image:              req.Image,
		initSystem:         req.InitSystem,
		kernel:             req.Kernel,
		memoryMB:           req.MemoryMB,
		balloonMB:          req.BalloonMB,
		cpus:               req.CPUs,
		nestedVirt:         req.NestedVirt,
		sharedMemoryConfig: cloneSharedMemoryConfig(req.SharedMemory),
		startedAt:          time.Now().UTC(),
		instance:           inst,
		display:            displayState,
		vncListener:        vncListener,
		vncServer:          vncServer,
		sharedMemory:       attachment,
	}

	m.mu.Lock()
	if m.closing || m.starting[id] != start {
		if m.starting[id] == start {
			delete(m.starting, id)
		}
		delete(m.reservations, id)
		delete(m.networkLeases, id)
		m.mu.Unlock()
		cancelStart()
		if vncListener != nil {
			_ = vncListener.Close()
		}
		_ = vncServer.Close()
		start.cleanupErr = inst.Close()
		start.cleanupErr = errors.Join(start.cleanupErr, attachment.Release())
		close(start.done)
		return client.InstanceState{}, errors.Join(ErrManagerClosing, start.cleanupErr)
	}
	if m.running == nil {
		m.running = make(map[string]*Machine)
	}
	delete(m.starting, id)
	delete(m.reservations, id)
	m.running[id] = machine
	m.mu.Unlock()
	cancelStart()
	close(start.done)

	go m.watch(machine)

	return m.StatusOf(id), nil
}

func (m *Manager) StartBlank(ctx context.Context, req client.StartInstanceRequest) (client.InstanceState, error) {
	return m.StartBlankStream(ctx, req, nil)
}

func (m *Manager) StartBlankStream(
	ctx context.Context,
	req client.StartInstanceRequest,
	onEvent func(client.BootEvent) error,
) (client.InstanceState, error) {
	id := instanceID(req.ID)
	return m.StartBlankInstanceStream(ctx, id, req, onEvent)
}

func (m *Manager) StartBlankInstanceStream(
	ctx context.Context,
	id string,
	req client.StartInstanceRequest,
	onEvent func(client.BootEvent) error,
) (client.InstanceState, error) {
	id = instanceID(id)
	req.ID = id
	if err := normalizeResources(&req.MemoryMB, &req.BalloonMB, &req.CPUs); err != nil {
		return client.InstanceState{}, err
	}
	display, err := normalizeDisplayConfig(req.Display)
	if err != nil {
		return client.InstanceState{}, err
	}
	req.Display = display
	if display != nil && (strings.TrimSpace(req.SnapshotDir) != "" || strings.TrimSpace(req.RestoreSnapshot) != "") {
		return client.InstanceState{}, fmt.Errorf("display-enabled VMs do not support startup snapshots")
	}
	if req.SharedMemory != nil && (strings.TrimSpace(req.SnapshotDir) != "" || strings.TrimSpace(req.RestoreSnapshot) != "") {
		return client.InstanceState{}, fmt.Errorf("shared-memory VMs do not support startup snapshots")
	}
	if err := validateSharedMemoryRequest(req.SharedMemory); err != nil {
		return client.InstanceState{}, err
	}
	canonicalShares, err := mounts.CanonicalRuntimeShares(req.Shares)
	if err != nil {
		return client.InstanceState{}, err
	}
	req.Shares = canonicalShares
	if err := m.supports(); err != nil {
		return client.InstanceState{}, err
	}
	maxVMs := m.host.HostCapabilities(ctx).MaxVMs

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return client.InstanceState{}, ErrManagerClosing
	}
	if m.running == nil {
		m.running = make(map[string]*Machine)
	}
	if m.starting == nil {
		m.starting = make(map[string]*managerStart)
	}
	if m.running[id] != nil {
		snapshot := m.statusSnapshotLocked(id)
		m.mu.Unlock()
		return m.resolveStatusSnapshot(snapshot), fmt.Errorf("VM %q is already running", id)
	}
	if _, ok := m.starting[id]; ok {
		snapshot := m.statusSnapshotLocked(id)
		m.mu.Unlock()
		return m.resolveStatusSnapshot(snapshot), fmt.Errorf("VM %q is already starting", id)
	}
	if err := m.checkCapacityLocked(maxVMs, req.MemoryMB, req.CPUs); err != nil {
		m.mu.Unlock()
		return client.InstanceState{}, err
	}
	delete(m.exited, id)
	if m.reservations == nil {
		m.reservations = make(map[string]resourceReservation)
	}
	m.reservations[id] = resourceReservation{memoryMB: req.MemoryMB, cpus: req.CPUs}
	startCtx, cancelStart := context.WithCancel(ctx)
	start := &managerStart{cancel: cancelStart, done: make(chan struct{})}
	m.starting[id] = start
	req.Network = m.ensureNetworkLeaseLocked(id, req.Image, req.Network)
	m.mu.Unlock()

	shares := append([]client.ShareMount(nil), req.Shares...)
	snapshotStartup := strings.TrimSpace(req.SnapshotDir) != "" || strings.TrimSpace(req.RestoreSnapshot) != ""
	startupShares := snapshotStartup
	if !startupShares {
		req.Shares = nil
	}
	attachment, err := m.attachSharedMemory(req.SharedMemory)
	if err != nil {
		m.finishStart(id, start)
		return client.InstanceState{}, err
	}
	if attachment != nil {
		startCtx = shmem.WithAttachment(startCtx, attachment)
	}
	startCtx, err = m.prepareHostMountContext(startCtx, id)
	if err != nil {
		_ = attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, err
	}
	inst, err := m.host.StartBlankStream(startCtx, req, onEvent)
	if err != nil {
		_ = attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, err
	}
	if attachment != nil && !attachment.Claimed() {
		closeErr := inst.Close()
		releaseErr := attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, errors.Join(fmt.Errorf("VM backend does not support shared memory"), closeErr, releaseErr)
	}
	if !startupShares {
		if err := addInstanceShares(startCtx, inst, shares); err != nil {
			start.cleanupErr = errors.Join(inst.Close(), attachment.Release())
			m.finishStart(id, start)
			return client.InstanceState{}, errors.Join(err, start.cleanupErr)
		}
	}
	displayState, vncListener, vncServer, err := startVNCServer(inst, id, display)
	if err != nil {
		closeErr := inst.Close()
		releaseErr := attachment.Release()
		m.finishStart(id, start)
		return client.InstanceState{}, errors.Join(err, closeErr, releaseErr)
	}

	machine := &Machine{
		id:                 id,
		image:              req.Image,
		initSystem:         req.InitSystem,
		kernel:             req.Kernel,
		memoryMB:           req.MemoryMB,
		balloonMB:          req.BalloonMB,
		cpus:               req.CPUs,
		nestedVirt:         req.NestedVirt,
		sharedMemoryConfig: cloneSharedMemoryConfig(req.SharedMemory),
		startedAt:          time.Now().UTC(),
		instance:           inst,
		display:            displayState,
		vncListener:        vncListener,
		vncServer:          vncServer,
		sharedMemory:       attachment,
	}

	m.mu.Lock()
	if m.closing || m.starting[id] != start {
		if m.starting[id] == start {
			delete(m.starting, id)
		}
		delete(m.reservations, id)
		delete(m.networkLeases, id)
		m.mu.Unlock()
		cancelStart()
		if vncListener != nil {
			_ = vncListener.Close()
		}
		_ = vncServer.Close()
		start.cleanupErr = inst.Close()
		start.cleanupErr = errors.Join(start.cleanupErr, attachment.Release())
		close(start.done)
		return client.InstanceState{}, errors.Join(ErrManagerClosing, start.cleanupErr)
	}
	if m.running == nil {
		m.running = make(map[string]*Machine)
	}
	delete(m.starting, id)
	delete(m.reservations, id)
	m.running[id] = machine
	m.mu.Unlock()
	cancelStart()
	close(start.done)

	go m.watch(machine)

	return m.StatusOf(id), nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	return m.ShutdownInstance(ctx, DefaultInstanceID)
}

func (m *Manager) ShutdownAll(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	results := make(chan managerShutdownResult, len(m.running))
	pending := 0
	pendingStops := make(map[string]*machineStopOperation, len(m.running))
	var errs []error
	for id, machine := range m.running {
		stop := m.beginMachineStopLocked(machine)
		pendingStops[id] = stop
		pending++
		go func() {
			results <- managerShutdownResult{id: id, err: waitMachineStop(context.Background(), stop)}
		}()
	}
	starts := make([]*managerStart, 0, len(m.starting))
	for _, start := range m.starting {
		starts = append(starts, start)
	}
	m.mu.Unlock()
	for _, start := range starts {
		start.cancel()
	}

	for pending > 0 {
		if ctx == nil {
			result := <-results
			pending--
			delete(pendingStops, result.id)
			if result.err != nil {
				errs = append(errs, fmt.Errorf("shutdown VM %q: %w", result.id, result.err))
			}
			continue
		}
		select {
		case result := <-results:
			pending--
			delete(pendingStops, result.id)
			if result.err != nil {
				errs = append(errs, fmt.Errorf("shutdown VM %q: %w", result.id, result.err))
			}
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			for id, stop := range pendingStops {
				select {
				case <-stop.done:
					if stop.err != nil {
						errs = append(errs, fmt.Errorf("shutdown VM %q: %w", id, stop.err))
					}
				default:
				}
			}
			return errors.Join(errs...)
		}
	}
	for _, start := range starts {
		if ctx == nil {
			<-start.done
		} else {
			select {
			case <-start.done:
			case <-ctx.Done():
				errs = append(errs, ctx.Err())
				return errors.Join(errs...)
			}
		}
		if start.cleanupErr != nil {
			errs = append(errs, fmt.Errorf("clean up canceled VM start: %w", start.cleanupErr))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

type managerShutdownResult struct {
	id  string
	err error
}

func (m *Manager) ShutdownInstance(ctx context.Context, id string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	id = instanceID(id)

	m.mu.Lock()
	machine := m.running[id]
	if machine == nil {
		if _, exited := m.exited[id]; exited {
			delete(m.exited, id)
			m.mu.Unlock()
			return nil
		}
		m.mu.Unlock()
		return fmt.Errorf("no VM %q is running", id)
	}
	stop := m.beginMachineStopLocked(machine)
	m.mu.Unlock()
	return waitMachineStop(ctx, stop)
}

func (m *Manager) beginMachineStopLocked(machine *Machine) *machineStopOperation {
	if machine.stop != nil {
		select {
		case <-machine.stop.done:
			if machine.stop.err == nil {
				machine.stop.observers++
				return machine.stop
			}
		default:
			machine.stop.observers++
			return machine.stop
		}
	}
	stop := &machineStopOperation{done: make(chan struct{}), observers: 1}
	machine.stop = stop
	machine.stopping = true
	machine.snapshotMu.Lock()
	if machine.snapshotCancel != nil {
		machine.snapshotCancel()
	}
	machine.snapshotMu.Unlock()
	go m.runMachineStop(machine, stop)
	return stop
}

func (m *Manager) runMachineStop(machine *Machine, stop *machineStopOperation) {
	machine.lifecycleMu.Lock()
	// Closing the instance is the recovery mechanism for a backend balloon
	// request which has stopped making progress. Do not wait behind balloonMu:
	// Close must be allowed to tear down the device and make that request return.
	var vncErr error
	if machine.vncListener != nil {
		vncErr = machine.vncListener.Close()
	}
	if errors.Is(vncErr, net.ErrClosed) {
		vncErr = nil
	}
	err := errors.Join(vncErr, machine.vncServer.Close(), machine.instance.Close(), machine.sharedMemory.Release())
	machine.lifecycleMu.Unlock()
	m.mu.Lock()
	stop.err = err
	if err == nil {
		if m.running != nil && m.running[machine.id] == machine {
			delete(m.running, machine.id)
		}
		delete(m.networkLeases, machine.id)
		m.recordExitLocked(machine, nil)
	} else if machine.stop == stop {
		machine.stopping = false
	}
	close(stop.done)
	m.mu.Unlock()
}

func waitMachineStop(ctx context.Context, stop *machineStopOperation) error {
	if ctx == nil {
		<-stop.done
		return stop.err
	}
	select {
	case <-stop.done:
		return stop.err
	default:
	}
	select {
	case <-stop.done:
		return stop.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Run(ctx context.Context, req client.RunRequest) (client.ExecResponse, error) {
	return m.RunIn(ctx, req.ID, req)
}

func (m *Manager) RunIn(ctx context.Context, id string, req client.RunRequest) (client.ExecResponse, error) {
	id = instanceID(id)
	shares, err := mounts.CanonicalRuntimeShares(req.Shares)
	if err != nil {
		return client.ExecResponse{}, err
	}
	req.Shares = shares
	m.mu.Lock()
	machine := m.running[id]
	m.mu.Unlock()
	if machine != nil {
		return m.host.RunInInstance(ctx, machine.instance, machine.image, req)
	}
	if req.Image == "" {
		return client.ExecResponse{}, fmt.Errorf("image is required")
	}
	if err := m.supports(); err != nil {
		return client.ExecResponse{}, err
	}
	return m.host.Run(ctx, req)
}

func (m *Manager) Stream(ctx context.Context, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return m.StreamIn(ctx, req.ID, req, inputs, onEvent)
}

func (m *Manager) RunStream(ctx context.Context, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return m.RunStreamIn(ctx, req.ID, req, inputs, onEvent)
}

func (m *Manager) RunStreamIn(ctx context.Context, id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	id = instanceID(id)
	shares, err := mounts.CanonicalRuntimeShares(req.Shares)
	if err != nil {
		return err
	}
	req.Shares = shares
	m.mu.Lock()
	machine := m.running[id]
	stopping := machine != nil && machine.stopping
	m.mu.Unlock()
	if machine == nil {
		if req.Image == "" {
			return fmt.Errorf("image is required")
		}
		if err := m.supports(); err != nil {
			return err
		}
		return m.host.RunStream(ctx, req, inputs, onEvent)
	}
	if stopping {
		return stoppedVMError(id)
	}
	req.ID = guestExecID()
	targetImage := strings.TrimSpace(req.Image)
	if !sameRuntimeImage(targetImage, machine.image) {
		err := m.host.RunInInstanceStream(ctx, machine.instance, machine.image, req, inputs, onEvent)
		if err != nil && m.instanceIsStopping(id, machine) {
			return stoppedVMError(id)
		}
		return err
	}
	if err := validateRuntimeShares(req.Shares); err != nil {
		return err
	}
	locked, unlock, lockErr := m.lockMachineOperation(id)
	if lockErr != nil {
		return lockErr
	}
	if locked != machine {
		unlock()
		return stoppedVMError(id)
	}
	if err := addInstanceShares(ctx, machine.instance, req.Shares); err != nil {
		unlock()
		if m.instanceIsStopping(id, machine) {
			return stoppedVMError(id)
		}
		return err
	}
	unlock()
	err = machine.instance.ExecStream(ctx, runningVMExecRequest(req), inputs, onEvent)
	if err != nil && m.instanceIsStopping(id, machine) {
		return stoppedVMError(id)
	}
	return err
}

func addInstanceShares(ctx context.Context, instance vmhost.Instance, shares []client.ShareMount) error {
	if len(shares) > 1 {
		batch, ok := instance.(interface {
			AddShares(context.Context, []client.ShareMount) error
		})
		if !ok {
			return fmt.Errorf("instance does not support atomic multi-share mutation")
		}
		return batch.AddShares(ctx, shares)
	}
	for _, share := range shares {
		if err := instance.AddShare(ctx, share); err != nil {
			return err
		}
	}
	return nil
}

type hostMountContextKey struct{}

func withHostMounts(ctx context.Context, mounts []virtio.ShareMount) context.Context {
	if len(mounts) == 0 {
		return ctx
	}
	return context.WithValue(ctx, hostMountContextKey{}, append([]virtio.ShareMount(nil), mounts...))
}

func hostMountsFromContext(ctx context.Context) []virtio.ShareMount {
	mounts, _ := ctx.Value(hostMountContextKey{}).([]virtio.ShareMount)
	return append([]virtio.ShareMount(nil), mounts...)
}

func (m *Manager) prepareHostMountContext(ctx context.Context, id string) (context.Context, error) {
	m.mu.Lock()
	provider := m.hostMounts
	m.mu.Unlock()
	if provider == nil {
		return ctx, nil
	}
	mounts, err := provider(ctx, id)
	if err != nil {
		return ctx, fmt.Errorf("prepare host-managed filesystems: %w", err)
	}
	return withHostMounts(ctx, mounts), nil
}
func validateRuntimeShares(shares []client.ShareMount) error {
	seen := make(map[string]client.ShareMount, len(shares))
	for _, share := range shares {
		canonical, err := mounts.CanonicalRuntimeShare(share)
		if err != nil {
			return err
		}
		mount := canonical.Mount
		if existing, ok := seen[mount]; ok {
			if existing != canonical {
				return fmt.Errorf("share mount %q is specified more than once with different settings", mount)
			}
			continue
		}
		seen[mount] = canonical
	}
	return nil
}

func (m *Manager) StreamIn(ctx context.Context, id string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	id = instanceID(id)
	m.mu.Lock()
	machine := m.running[id]
	stopping := machine != nil && machine.stopping
	m.mu.Unlock()
	if machine == nil {
		return fmt.Errorf("no VM %q is running", id)
	}
	if stopping {
		return stoppedVMError(id)
	}
	req.ID = guestExecID()
	targetImage := strings.TrimSpace(req.Image)
	if vmhost.IsHostedInstance(machine.instance) || targetImage != "" {
		err := m.host.ExecInInstanceStream(ctx, machine.instance, machine.image, req, inputs, onEvent)
		if err != nil && m.instanceIsStopping(id, machine) {
			return stoppedVMError(id)
		}
		return err
	}
	err := machine.instance.ExecStream(ctx, req, inputs, onEvent)
	if err != nil && m.instanceIsStopping(id, machine) {
		return stoppedVMError(id)
	}
	return err
}

func (m *Manager) instanceIsStopping(id string, machine *Machine) bool {
	if machine == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return machine.stopping || m.running[id] != machine
}

func stoppedVMError(id string) error {
	return fmt.Errorf("VM %q stopped", id)
}

func (m *Manager) AddPortForward(ctx context.Context, forward client.PortForward) error {
	return m.AddPortForwardTo(ctx, DefaultInstanceID, forward)
}

func (m *Manager) AddShareTo(ctx context.Context, id string, share client.ShareMount) error {
	machine, unlock, err := m.lockMachineOperation(id)
	if err != nil {
		return err
	}
	defer unlock()
	return machine.instance.AddShare(ctx, share)
}

func (m *Manager) AddPortForwardTo(ctx context.Context, id string, forward client.PortForward) error {
	machine, unlock, err := m.lockMachineOperation(id)
	if err != nil {
		return err
	}
	defer unlock()
	return machine.instance.AddPortForward(ctx, forward)
}

func (m *Manager) AllowServiceProxyPortTo(ctx context.Context, id string, port int) error {
	machine, unlock, err := m.lockMachineOperation(id)
	if err != nil {
		return err
	}
	defer unlock()
	allower, ok := machine.instance.(serviceProxyPortAllower)
	if !ok {
		return fmt.Errorf("VM %q network does not support service proxy port updates", machine.id)
	}
	return allower.AllowServiceProxyPort(ctx, port)
}

func (m *Manager) FlushInstance(ctx context.Context, id string) error {
	machine, unlock, err := m.lockMachineOperation(id)
	if err != nil {
		return err
	}
	defer unlock()
	flusher, ok := machine.instance.(instanceFlushProvider)
	if !ok {
		return fmt.Errorf("VM %q root filesystem cannot be flushed", machine.id)
	}
	return flusher.Flush(ctx)
}

func (m *Manager) ConsoleHistory(ctx context.Context, id string) (string, error) {
	machine, unlock, err := m.lockMachineOperation(id)
	if err != nil {
		return "", err
	}
	defer unlock()
	provider, ok := machine.instance.(consoleHistoryProvider)
	if !ok {
		return "", fmt.Errorf("VM %q console history is not available", machine.id)
	}
	return provider.ConsoleHistory(ctx)
}

func (m *Manager) SnapshotRootFS(ctx context.Context, id string, imageName string) (imagefs.Directory, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	imageName = strings.TrimSpace(imageName)
	machine, unlock, err := m.lockMachineOperation(id)
	if err != nil {
		return nil, "", err
	}
	defer unlock()
	snapshotCtx, snapshotCancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.running[machine.id] != machine || machine.stopping {
		m.mu.Unlock()
		snapshotCancel()
		return nil, "", stoppedVMError(machine.id)
	}
	machine.snapshotMu.Lock()
	machine.snapshotCancel = snapshotCancel
	machine.snapshotMu.Unlock()
	m.mu.Unlock()
	defer func() {
		snapshotCancel()
		machine.snapshotMu.Lock()
		machine.snapshotCancel = nil
		machine.snapshotMu.Unlock()
	}()
	flusher, ok := machine.instance.(instanceFlushProvider)
	if !ok {
		return nil, "", fmt.Errorf("VM %q root filesystem cannot be flushed", machine.id)
	}
	if err := flusher.Flush(snapshotCtx); err != nil {
		return nil, "", err
	}
	if imageName != "" {
		if snapshotter, ok := machine.instance.(imageSnapshotContextProvider); ok {
			root, err := snapshotter.SnapshotImageContext(snapshotCtx, imageName)
			if err != nil {
				return nil, "", err
			}
			return root, imageName, nil
		}
		return nil, "", fmt.Errorf("VM %q image %q cannot be snapshotted", machine.id, imageName)
	}
	contextSnapshotter, ok := machine.instance.(rootSnapshotContextProvider)
	if !ok {
		return nil, "", fmt.Errorf("VM %q root filesystem does not support cancelable snapshots", machine.id)
	}
	root, err := contextSnapshotter.RootSnapshotContext(snapshotCtx)
	if err != nil {
		return nil, "", err
	}
	return root, machine.image, nil
}

func (m *Manager) lockMachineOperation(id string) (*Machine, func(), error) {
	id = instanceID(id)
	m.mu.Lock()
	machine := m.running[id]
	stopping := machine != nil && machine.stopping
	m.mu.Unlock()
	if machine == nil {
		return nil, nil, fmt.Errorf("no VM %q is running", id)
	}
	if stopping {
		return nil, nil, stoppedVMError(id)
	}
	machine.lifecycleMu.Lock()
	m.mu.Lock()
	valid := m.running[id] == machine && !machine.stopping
	m.mu.Unlock()
	if !valid {
		machine.lifecycleMu.Unlock()
		return nil, nil, stoppedVMError(id)
	}
	return machine, machine.lifecycleMu.Unlock, nil
}

func (m *Manager) Status() client.InstanceState {
	return m.StatusOf(DefaultInstanceID)
}

func (m *Manager) StatusOf(id string) client.InstanceState {
	id = instanceID(id)
	m.mu.Lock()
	snapshot := m.statusSnapshotLocked(id)
	m.mu.Unlock()
	return m.resolveStatusSnapshot(snapshot)
}

func (m *Manager) VirtioFSStats(id string) []virtio.FSStats {
	id = instanceID(id)
	m.mu.Lock()
	if m.running == nil || m.running[id] == nil || m.running[id].instance == nil {
		m.mu.Unlock()
		return nil
	}
	provider, ok := m.running[id].instance.(virtioFSStatsProvider)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return provider.VirtioFSStats()
}

func (m *Manager) Statuses() []client.InstanceState {
	m.mu.Lock()
	if len(m.running) == 0 && len(m.starting) == 0 && len(m.exited) == 0 {
		m.mu.Unlock()
		return nil
	}
	ids := make([]string, 0, len(m.running)+len(m.starting)+len(m.exited))
	for id := range m.running {
		ids = append(ids, id)
	}
	for id := range m.starting {
		if m.running[id] == nil {
			ids = append(ids, id)
		}
	}
	for id := range m.exited {
		if m.running[id] == nil {
			if _, starting := m.starting[id]; !starting {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	snapshots := make([]managerStatusSnapshot, 0, len(ids))
	for _, id := range ids {
		snapshots = append(snapshots, m.statusSnapshotLocked(id))
	}
	m.mu.Unlock()
	out := make([]client.InstanceState, 0, len(snapshots))
	for _, snapshot := range snapshots {
		out = append(out, m.resolveStatusSnapshot(snapshot))
	}
	return out
}

func (m *Manager) Capabilities() client.CapabilitiesResponse {
	var caps client.CapabilitiesResponse
	if m.capabilities == nil {
		caps = HostCapabilities()
	} else {
		caps = m.capabilities()
	}
	m.mu.Lock()
	memory, cpus := m.resourceUsageLocked()
	m.mu.Unlock()
	if m.maxCPUs > 0 && (caps.MaxInstances <= 0 || caps.MaxInstances > m.maxCPUs) {
		caps.MaxInstances = m.maxCPUs
	}
	caps.MemoryCapacityMB, caps.MemoryReservedMB = m.maxMemoryMB, memory
	caps.CPUCapacity, caps.CPUReserved = m.maxCPUs, cpus
	return caps
}

type managerStatusSnapshot struct {
	id                      string
	machine                 *Machine
	state                   client.InstanceState
	provider                networkIPv4Provider
	backingProvider         instanceBackingUsageProvider
	backingMetadataProvider instanceBackingMetadataUsageProvider
	backingCombinedProvider instanceBackingCombinedUsageProvider
	backingSnapshotProvider instanceBackingSnapshotProvider
	persistentFSProvider    instancePersistentFSStatusProvider
	balloonProvider         instanceBalloonStateProvider
	displayFramebuffer      *virtio.Framebuffer
}

func (m *Manager) statusSnapshotLocked(id string) managerStatusSnapshot {
	id = instanceID(id)
	if m.running == nil || m.running[id] == nil {
		if m.closing {
			return managerStatusSnapshot{id: id, state: client.InstanceState{ID: id, Status: "stopped"}}
		}
		if m.starting != nil {
			if _, ok := m.starting[id]; ok {
				return managerStatusSnapshot{id: id, state: client.InstanceState{ID: id, Status: "starting"}}
			}
		}
		if state, ok := m.exited[id]; ok {
			return managerStatusSnapshot{id: id, state: state}
		}
		return managerStatusSnapshot{id: id, state: client.InstanceState{ID: id, Status: "stopped"}}
	}
	machine := m.running[id]
	state := client.InstanceState{
		ID:           id,
		Status:       "running",
		Image:        machine.image,
		InitSystem:   machine.initSystem,
		Kernel:       machine.kernel,
		MemoryMB:     machine.memoryMB,
		BalloonMB:    machine.balloonMB,
		CPUs:         machine.cpus,
		NestedVirt:   machine.nestedVirt,
		SharedMemory: cloneSharedMemoryConfig(machine.sharedMemoryConfig),
		StartedAt:    machine.startedAt.Format(time.RFC3339Nano),
		Display:      machine.display,
	}
	snapshot := managerStatusSnapshot{id: id, machine: machine, state: state}
	if machine.stopping {
		snapshot.state.Status = "stopping"
		return snapshot
	}
	if provider, ok := machine.instance.(networkIPv4Provider); ok {
		snapshot.provider = provider
	}
	if provider, ok := machine.instance.(instanceBackingUsageProvider); ok {
		snapshot.backingProvider = provider
	}
	if provider, ok := machine.instance.(instanceBackingMetadataUsageProvider); ok {
		snapshot.backingMetadataProvider = provider
	}
	if provider, ok := machine.instance.(instanceBackingCombinedUsageProvider); ok {
		snapshot.backingCombinedProvider = provider
	}
	if provider, ok := machine.instance.(instanceBackingSnapshotProvider); ok {
		snapshot.backingSnapshotProvider = provider
	}
	if provider, ok := machine.instance.(instancePersistentFSStatusProvider); ok {
		snapshot.persistentFSProvider = provider
	}
	if provider, ok := machine.instance.(instanceBalloonStateProvider); ok {
		snapshot.balloonProvider = provider
	} else {
		snapshot.state.BalloonMB = 0
		snapshot.state.BalloonStatus = "unsupported"
	}
	if provider, ok := machine.instance.(instanceDesktopProvider); ok && provider.Desktop() != nil {
		snapshot.displayFramebuffer = provider.Desktop().Framebuffer
	}
	return snapshot
}

func (m *Manager) resolveStatusSnapshot(snapshot managerStatusSnapshot) client.InstanceState {
	if snapshot.provider == nil && snapshot.backingProvider == nil && snapshot.backingMetadataProvider == nil && snapshot.backingCombinedProvider == nil && snapshot.backingSnapshotProvider == nil && snapshot.persistentFSProvider == nil && snapshot.balloonProvider == nil && snapshot.displayFramebuffer == nil {
		return snapshot.state
	}
	if snapshot.state.Display != nil && snapshot.displayFramebuffer != nil {
		width, height := snapshot.displayFramebuffer.Size()
		display := *snapshot.state.Display
		display.Width = uint32(width)
		display.Height = uint32(height)
		snapshot.state.Display = &display
	}
	if snapshot.provider != nil {
		snapshot.state.NetworkIPv4 = snapshot.provider.NetworkIPv4()
	}
	if snapshot.backingProvider != nil && snapshot.backingSnapshotProvider == nil {
		current, highWater, physical, err := snapshot.backingProvider.BackingUsage()
		snapshot.state.BackingDataBytes = current
		snapshot.machine.backingMu.Lock()
		snapshot.machine.backingDataHighWater = max(snapshot.machine.backingDataHighWater, current, highWater)
		snapshot.state.BackingDataHighWaterBytes = snapshot.machine.backingDataHighWater
		snapshot.machine.backingMu.Unlock()
		snapshot.state.BackingPhysicalBytes = physical
		if err != nil {
			snapshot.state.BackingReclaimError = err.Error()
		}
	}
	if snapshot.backingMetadataProvider != nil && snapshot.backingSnapshotProvider == nil {
		current, highWater := snapshot.backingMetadataProvider.BackingMetadataUsage()
		snapshot.state.BackingMetadataBytes = current
		snapshot.machine.backingMu.Lock()
		snapshot.machine.backingMetadataHighWater = max(snapshot.machine.backingMetadataHighWater, current, highWater)
		snapshot.state.BackingMetadataHighWaterBytes = snapshot.machine.backingMetadataHighWater
		snapshot.machine.backingMu.Unlock()
	}
	if (snapshot.backingProvider != nil || snapshot.backingMetadataProvider != nil) && snapshot.backingSnapshotProvider == nil {
		snapshot.state.BackingBytes = saturatingUint64Add(snapshot.state.BackingDataBytes, snapshot.state.BackingMetadataBytes)
		// Either component peak is a lower bound for the combined usage at
		// that instant. Taking their maximum is safe; adding them would invent
		// a state whose peaks may have occurred at different times.
		observedHighWater := max(snapshot.state.BackingBytes, snapshot.state.BackingDataHighWaterBytes, snapshot.state.BackingMetadataHighWaterBytes)
		snapshot.machine.backingMu.Lock()
		if observedHighWater > snapshot.machine.backingHighWater {
			snapshot.machine.backingHighWater = observedHighWater
		}
		snapshot.state.BackingHighWaterBytes = snapshot.machine.backingHighWater
		snapshot.machine.backingMu.Unlock()
	}
	if snapshot.backingCombinedProvider != nil && snapshot.backingSnapshotProvider == nil {
		current, highWater := snapshot.backingCombinedProvider.BackingCombinedUsage()
		snapshot.state.BackingBytes = current
		snapshot.machine.backingMu.Lock()
		snapshot.machine.backingHighWater = max(snapshot.machine.backingHighWater, current, highWater)
		snapshot.state.BackingHighWaterBytes = snapshot.machine.backingHighWater
		snapshot.machine.backingMu.Unlock()
	}
	if snapshot.backingSnapshotProvider != nil {
		usage := snapshot.backingSnapshotProvider.BackingSnapshot()
		snapshot.state.BackingDataBytes = usage.DataBytes
		snapshot.state.BackingMetadataBytes = usage.MetadataBytes
		snapshot.state.BackingBytes = usage.CombinedBytes
		snapshot.state.BackingPhysicalBytes = usage.PhysicalBytes
		snapshot.state.BackingUsageStale = usage.Stale
		snapshot.state.BackingActiveMutations = usage.ActiveMutations
		snapshot.machine.backingMu.Lock()
		snapshot.machine.backingDataHighWater = max(snapshot.machine.backingDataHighWater, usage.DataBytes, usage.DataHighWaterBytes)
		snapshot.machine.backingMetadataHighWater = max(snapshot.machine.backingMetadataHighWater, usage.MetadataBytes, usage.MetadataHighWaterBytes)
		snapshot.machine.backingHighWater = max(snapshot.machine.backingHighWater, usage.CombinedBytes, usage.CombinedHighWaterBytes)
		snapshot.state.BackingDataHighWaterBytes = snapshot.machine.backingDataHighWater
		snapshot.state.BackingMetadataHighWaterBytes = snapshot.machine.backingMetadataHighWater
		snapshot.state.BackingHighWaterBytes = snapshot.machine.backingHighWater
		snapshot.machine.backingMu.Unlock()
		if usage.ReclaimError != nil {
			snapshot.state.BackingReclaimError = usage.ReclaimError.Error()
		}
	}
	if snapshot.persistentFSProvider != nil {
		for _, status := range snapshot.persistentFSProvider.PersistentFSStatus() {
			state := client.PersistentMountState{
				Name:               status.Name,
				Mount:              status.Mount,
				FormatVersion:      status.FormatVersion,
				LowerID:            status.LowerID,
				PreviousLowerID:    status.PreviousLowerID,
				Sequence:           status.Sequence,
				DurableSequence:    status.DurableSequence,
				UpperLogicalBytes:  status.UpperLogicalBytes,
				UpperDataBytes:     status.UpperDataBytes,
				UpperPhysicalBytes: status.UpperPhysicalBytes,
				WALBytes:           status.WALBytes,
				StagingBytes:       status.StagingBytes,
				TrashBytes:         status.TrashBytes,
				RecoveryStatus:     status.RecoveryStatus,
				QuarantinePath:     status.QuarantinePath,
				DiscardedBytes:     status.DiscardedBytes,
				LastError:          status.LastError,
				HostFreeBytes:      status.HostFreeBytes,
			}
			if !status.LastCheckpoint.IsZero() {
				state.LastCheckpoint = status.LastCheckpoint.Format(time.RFC3339Nano)
			}
			snapshot.state.PersistentMounts = append(snapshot.state.PersistentMounts, state)
		}
	}
	if snapshot.balloonProvider != nil {
		target, actual, ready, supported := snapshot.balloonProvider.BalloonState()
		if !supported {
			snapshot.state.BalloonMB = 0
			snapshot.state.BalloonStatus = "unsupported"
		} else {
			snapshot.state.BalloonMB = target
			snapshot.state.BalloonActualMB = actual
			switch {
			case !ready:
				snapshot.state.BalloonStatus = "driver_unavailable"
			case target == actual:
				snapshot.state.BalloonStatus = "converged"
			case actual < target:
				snapshot.state.BalloonStatus = "inflating"
			default:
				snapshot.state.BalloonStatus = "deflating"
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil || m.running[snapshot.id] != snapshot.machine {
		return m.statusSnapshotLocked(snapshot.id).state
	}
	return snapshot.state
}

func saturatingUint64Add(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

func (m *Manager) watch(machine *Machine) {
	err := machine.instance.Wait()
	err = errors.Join(err, machine.sharedMemory.Release())
	if machine.vncListener != nil {
		_ = machine.vncListener.Close()
	}
	_ = machine.vncServer.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	// An explicit stop owns exit publication. Close implementations commonly
	// cancel their run context, so Wait may report context.Canceled even though
	// the requested shutdown completed successfully. A failed Close keeps the
	// machine registered so cleanup can be retried.
	if machine.stopping || machine.stop != nil && machine.stop.err != nil {
		return
	}
	if m.running != nil && m.running[machine.id] == machine {
		delete(m.running, machine.id)
	}
	delete(m.networkLeases, machine.id)
	m.recordExitLocked(machine, err)
}

func (m *Manager) recordExitLocked(machine *Machine, err error) {
	machine.lastErr = err
	machine.exitedAt = time.Now().UTC()
	if m.exited == nil {
		m.exited = make(map[string]client.InstanceState)
	}
	state := client.InstanceState{ID: machine.id, Status: "stopped", Image: machine.image, InitSystem: machine.initSystem, Kernel: machine.kernel, MemoryMB: machine.memoryMB, BalloonMB: machine.balloonMB, CPUs: machine.cpus, NestedVirt: machine.nestedVirt, SharedMemory: cloneSharedMemoryConfig(machine.sharedMemoryConfig), StartedAt: machine.startedAt.Format(time.RFC3339), ExitedAt: machine.exitedAt.Format(time.RFC3339)}
	if err != nil {
		state.Status = "crashed"
		state.Error = boundedLifecycleDiagnostic(err.Error())
		state.ExitReason = "VM backend exited unexpectedly"
	} else {
		state.ExitReason = "clean shutdown"
	}
	m.exited[machine.id] = state
	for len(m.exited) > maxExitTombstones {
		var oldestID string
		var oldest time.Time
		for id, candidate := range m.exited {
			at, _ := time.Parse(time.RFC3339, candidate.ExitedAt)
			if oldestID == "" || at.Before(oldest) {
				oldestID, oldest = id, at
			}
		}
		delete(m.exited, oldestID)
	}
}

func boundedLifecycleDiagnostic(diagnostic string) string {
	diagnostic = strings.TrimSpace(diagnostic)
	if len(diagnostic) <= maxLifecycleDiagnosticBytes {
		return diagnostic
	}
	const headBytes = 512
	omitted := len(diagnostic) - maxLifecycleDiagnosticBytes
	marker := fmt.Sprintf("\n[... %d diagnostic bytes omitted ...]\n", omitted)
	tailBytes := maxLifecycleDiagnosticBytes - headBytes - len(marker)
	if tailBytes < 0 {
		tailBytes = 0
	}
	return diagnostic[:headBytes] + marker + diagnostic[len(diagnostic)-tailBytes:]
}

func (m *Manager) checkCapacityLocked(maxVMs int, memoryMB uint64, cpus int) error {
	if maxVMs > 0 && len(m.running)+len(m.starting) >= maxVMs {
		return fmt.Errorf("maximum running VM instances reached: %d", maxVMs)
	}
	usedMemory, usedCPUs := m.resourceUsageLocked()
	if m.maxMemoryMB > 0 && (memoryMB > m.maxMemoryMB || usedMemory > m.maxMemoryMB-memoryMB) {
		return fmt.Errorf("VM memory admission rejected: requested=%d MiB reserved=%d MiB capacity=%d MiB", memoryMB, usedMemory, m.maxMemoryMB)
	}
	if m.maxCPUs > 0 && (cpus > m.maxCPUs || usedCPUs > m.maxCPUs-cpus) {
		return fmt.Errorf("VM CPU admission rejected: requested=%d reserved=%d capacity=%d", cpus, usedCPUs, m.maxCPUs)
	}
	return nil
}

func (m *Manager) resourceUsageLocked() (uint64, int) {
	var memory uint64
	var cpus int
	for _, machine := range m.running {
		memory += machine.memoryMB
		cpus += machine.cpus
	}
	for _, reservation := range m.reservations {
		memory += reservation.memoryMB
		cpus += reservation.cpus
	}
	return memory, cpus
}

func (m *Manager) SetInstanceBalloon(id string, targetMB uint64) error {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	machine := m.running[id]
	if machine == nil {
		m.mu.Unlock()
		return fmt.Errorf("VM %q is not running", id)
	}
	if targetMB > machine.memoryMB {
		m.mu.Unlock()
		return fmt.Errorf("balloon target %d MiB exceeds VM memory %d MiB", targetMB, machine.memoryMB)
	}
	if machine.stopping {
		m.mu.Unlock()
		return fmt.Errorf("VM %q is stopping", id)
	}
	controller, ok := machine.instance.(instanceBalloonController)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("VM %q does not support dynamic ballooning", id)
	}
	machine.balloonMu.Lock()
	m.mu.Lock()
	if m.running[id] != machine || machine.stopping {
		m.mu.Unlock()
		machine.balloonMu.Unlock()
		return fmt.Errorf("VM %q is stopping", id)
	}
	m.mu.Unlock()
	err := controller.SetBalloonMB(targetMB)
	machine.balloonMu.Unlock()
	if err != nil {
		return fmt.Errorf("set VM %q balloon target: %w", id, err)
	}
	m.mu.Lock()
	if current := m.running[id]; current == machine {
		current.balloonMB = targetMB
	}
	m.mu.Unlock()
	return nil
}

func normalizeResources(memoryMB, balloonMB *uint64, cpus *int) error {
	if *memoryMB == 0 {
		*memoryMB = 512
	}
	if *cpus == 0 {
		*cpus = 1
	}
	if *cpus < 0 {
		return fmt.Errorf("cpus must be positive")
	}
	if *memoryMB < minimumGuestMemoryMB {
		return fmt.Errorf("memory_mb must be at least %d MiB", minimumGuestMemoryMB)
	}
	maxAllocationMB := uint64(^uint(0)>>1) >> 20
	if *memoryMB > maxAllocationMB {
		return fmt.Errorf("memory_mb %d overflows host allocation size", *memoryMB)
	}
	if *balloonMB > *memoryMB {
		return fmt.Errorf("balloon_mb %d exceeds memory_mb %d", *balloonMB, *memoryMB)
	}
	return nil
}

func (m *Manager) finishStart(id string, start *managerStart) {
	m.mu.Lock()
	if m.starting[id] == start {
		delete(m.starting, id)
	}
	delete(m.reservations, id)
	delete(m.networkLeases, id)
	m.mu.Unlock()
	start.cancel()
	close(start.done)
}

func (m *Manager) ensureNetworkLeaseLocked(id, image string, cfg *client.NetworkConfig) *client.NetworkConfig {
	if cfg == nil {
		return nil
	}
	if !cfg.Enabled {
		return cfg
	}
	if strings.TrimSpace(cfg.GuestIPv4) != "" && strings.TrimSpace(cfg.GuestMAC) != "" {
		return cfg
	}
	id = instanceID(id)
	if m.networkLeases == nil {
		m.networkLeases = make(map[string]managerNetworkLease)
	}
	lease, ok := m.networkLeases[id]
	if !ok {
		used := map[byte]bool{1: true}
		for _, existing := range m.networkLeases {
			if ip4 := existing.ip.To4(); ip4 != nil {
				used[ip4[3]] = true
			}
		}
		host := byte(2)
		for ; host <= 254; host++ {
			if !used[host] {
				break
			}
		}
		lease = managerNetworkLease{
			ip:  net.IPv4(10, 42, 0, host),
			mac: net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, host},
		}
		m.networkLeases[id] = lease
	}
	next := *cfg
	if strings.TrimSpace(next.GuestIPv4) == "" {
		next.GuestIPv4 = lease.ip.String()
	}
	if strings.TrimSpace(next.GuestMAC) == "" {
		next.GuestMAC = lease.mac.String()
	}
	return &next
}

func (m *Manager) releaseNetworkLease(id string) {
	id = instanceID(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.networkLeases, id)
}

func instanceID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return DefaultInstanceID
	}
	return id
}
