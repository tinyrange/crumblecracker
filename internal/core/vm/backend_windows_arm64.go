//go:build windows && arm64

package vm

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/guestinit"
	"github.com/tinyrange/crumblecracker/internal/core/hv/whp"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	"github.com/tinyrange/crumblecracker/internal/core/managed/rootartifact"
	managedruntime "github.com/tinyrange/crumblecracker/internal/core/managed/runtime"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vm/execplan"
	vmhost "github.com/tinyrange/crumblecracker/internal/core/vm/host"
	hostmanaged "github.com/tinyrange/crumblecracker/internal/core/vm/host/managed"
	"github.com/tinyrange/crumblecracker/internal/core/vm/mounts"
	"github.com/tinyrange/crumblecracker/internal/core/vm/netstate"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const windowsInitReadyMarker = vmruntime.InstanceReadyMarker

type runtimeBackend struct {
	vmhost.UnsupportedBackend
	kernel         *alpine.Manager
	images         *oci.Store
	guestInitCache string
}

func NewRuntimeBackend(kernel *alpine.Manager, images *oci.Store, guestInitCache string) Backend {
	return &runtimeBackend{kernel: kernel, images: images, guestInitCache: guestInitCache}
}

func (b *runtimeBackend) Start(ctx context.Context, req client.CreateInstanceRequest) (Instance, error) {
	return b.StartStream(ctx, req, nil)
}

func (b *runtimeBackend) StartBlank(ctx context.Context, req client.StartInstanceRequest) (Instance, error) {
	return b.StartBlankStream(ctx, req, nil)
}

func (b *runtimeBackend) StartStream(ctx context.Context, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	if len(req.PersistentMounts) != 0 {
		return nil, fmt.Errorf("persistent image mounts are not yet supported on windows/arm64")
	}
	mountState, err := mounts.NewState(req.Shares)
	if err != nil {
		return nil, err
	}

	trace := os.Getenv("CC_WHP_ARM64_TIMING") != "" || os.Getenv("CC_WHP_BSD_TIMING") != ""
	startTime := time.Now()
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +0s: StartStream image=%q\n", req.Image)
	}
	if b == nil || b.kernel == nil || b.images == nil {
		return nil, fmt.Errorf("runtime backend is not configured")
	}
	if req.CPUs > 1 {
		return nil, fmt.Errorf("windows arm64 runtime currently supports only 1 CPU")
	}
	restoreSnapshot := strings.TrimSpace(req.RestoreSnapshot)
	image, err := b.images.Open(req.Image)
	if err != nil {
		return nil, err
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: image opened\n", time.Since(startTime).Round(time.Millisecond))
	}
	if err := ensureWindowsARM64Image(image); err != nil {
		return nil, err
	}
	image = withWindowsRuntimeMountDirs(image)
	qemuX8664, err := PrepareAMD64Emulator(ctx, image, b.kernel.ExtractPackageFile)
	if err != nil {
		return nil, err
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: amd64 emulator prepared=%v\n", time.Since(startTime).Round(time.Millisecond), qemuX8664 != "")
	}
	network, err := newWindowsARM64NetworkRuntime(req.Network)
	if err != nil {
		return nil, err
	}
	if network != nil {
		defer func() {
			if err != nil {
				_ = network.Close()
			}
		}()
	}
	fsdevs, rootFS, err := arm64vm.BuildFSDevices(vmruntime.RunRequest{
		Image:             image,
		AMD64EmulatorPath: qemuX8664,
		Shares:            mounts.ConvertShareMounts(req.Shares),
	}, nil)
	if err != nil {
		return nil, err
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: fs devices built=%d\n", time.Since(startTime).Round(time.Millisecond), len(fsdevs))
	}
	workDir := image.Config.WorkingDir
	if workDir == "" {
		workDir = "/"
	}
	if restoreSnapshot != "" {
		started, err := (managedruntime.Service{}).Start(ctx, managedruntime.StartRequest{
			Profile: managedguest.LinuxProfile,
			Host:    whp.Host{},
			Spec:    windowsLinuxMachineSpec(req.MemoryMB, req.CPUs, req.Dmesg),
			Attachments: whp.LinuxManagedAttachments{
				FSDevices:       fsdevs,
				NetDevice:       windowsNetworkDevice(network),
				SnapshotDir:     strings.TrimSpace(req.SnapshotDir),
				RestoreSnapshot: restoreSnapshot,
			},
		}, onEvent)
		if err != nil {
			return nil, err
		}
		return &windowsInstance{
			managedInstanceCore: newWindowsManagedCore(started.Session, image, vmruntime.WithDefaultEnv(image.Config.Env), workDir),
			image:               image,
			rootFS:              rootFS,
			fsdevs:              fsdevs,
			network:             network,
			dmesg:               req.Dmesg,
			mounts:              mountState,
		}, nil
	}
	modules, err := b.kernel.PlanModuleLoad(windowsRuntimeConfigVars(image), windowsRuntimeModuleMap())
	if err != nil {
		return nil, err
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: planned modules=%d\n", time.Since(startTime).Round(time.Millisecond), len(modules))
	}
	initBin, err := guestinit.BuildForArch(ctx, b.guestInitCache, "arm64")
	if err != nil {
		return nil, fmt.Errorf("build guest init: %w", err)
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: guest init bytes=%d\n", time.Since(startTime).Round(time.Millisecond), len(initBin))
	}
	kernel, err := b.kernel.ReadKernel()
	if err != nil {
		return nil, err
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: kernel read bytes=%d\n", time.Since(startTime).Round(time.Millisecond), len(kernel))
	}
	initCfg := windowsGuestInitConfig(modules, true, req.InitSystem)
	initCfg.RootFSTag = vmruntime.RootFSTag
	if qemuX8664 != "" {
		initCfg.EmulatorTag = vmruntime.EmulatorTag
	}
	initCfg.Env = vmruntime.WithDefaultEnv(image.Config.Env)
	initCfg.WorkDir = workDir
	initCfg.Network = windowsNetworkGuestInitConfig(network)
	if strings.TrimSpace(req.SnapshotDir) != "" {
		initCfg.SnapshotMMIOBase = arm64vm.SnapshotBase
	}
	if err := applyRuntimeKernelMetadata(&initCfg, b.kernel, ""); err != nil {
		return nil, fmt.Errorf("read kernel metadata: %w", err)
	}
	initrd, err := vmruntime.BuildInitramfs(initBin, modules, initCfg)
	if err != nil {
		return nil, fmt.Errorf("build initramfs: %w", err)
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: initramfs bytes=%d\n", time.Since(startTime).Round(time.Millisecond), len(initrd))
	}
	started, err := (managedruntime.Service{}).Start(ctx, managedruntime.StartRequest{
		Profile: managedguest.LinuxProfile,
		Host:    whp.Host{},
		Spec:    windowsLinuxMachineSpec(req.MemoryMB, req.CPUs, req.Dmesg),
		Artifact: rootartifact.Artifact{
			Kernel: kernel,
			Initrd: initrd,
		},
		Attachments: whp.LinuxManagedAttachments{
			FSDevices:       fsdevs,
			NetDevice:       windowsNetworkDevice(network),
			SnapshotDir:     strings.TrimSpace(req.SnapshotDir),
			RestoreSnapshot: strings.TrimSpace(req.RestoreSnapshot),
		},
	}, onEvent)
	if err != nil {
		return nil, err
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 backend +%s: managed service ready\n", time.Since(startTime).Round(time.Millisecond))
	}
	return &windowsInstance{
		managedInstanceCore: newWindowsManagedCore(started.Session, image, vmruntime.WithDefaultEnv(image.Config.Env), workDir),
		image:               image,
		rootFS:              rootFS,
		fsdevs:              fsdevs,
		network:             network,
		dmesg:               req.Dmesg,
		mounts:              mountState,
	}, nil
}

func (b *runtimeBackend) StartBlankStream(ctx context.Context, req client.StartInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {
	if len(req.PersistentMounts) != 0 {
		return nil, fmt.Errorf("persistent image mounts are not yet supported on windows/arm64")
	}
	if strings.TrimSpace(req.Image) != "" {
		return b.StartStream(ctx, client.CreateInstanceRequest{
			ID:              req.ID,
			Image:           req.Image,
			InitSystem:      req.InitSystem,
			Kernel:          req.Kernel,
			Shares:          append([]client.ShareMount(nil), req.Shares...),
			Network:         req.Network,
			KernelModules:   append([]string(nil), req.KernelModules...),
			MemoryMB:        req.MemoryMB,
			BalloonMB:       req.BalloonMB,
			CPUs:            req.CPUs,
			NestedVirt:      req.NestedVirt,
			Dmesg:           req.Dmesg,
			SnapshotDir:     req.SnapshotDir,
			RestoreSnapshot: req.RestoreSnapshot,
			TimeoutSeconds:  req.TimeoutSeconds,
		}, onEvent)
	}
	if b == nil || b.kernel == nil {
		return nil, fmt.Errorf("runtime backend is not configured")
	}
	network, err := newWindowsARM64NetworkRuntime(req.Network)
	if err != nil {
		return nil, err
	}
	rootFSBackend := virtio.NewImageFS(blankWindowsRuntimeRootFS(), "")
	fsdevs, rootFS, err := arm64vm.BuildFSDevices(vmruntime.RunRequest{RootFS: rootFSBackend}, nil)
	if err != nil {
		return nil, err
	}
	restoreSnapshot := strings.TrimSpace(req.RestoreSnapshot)
	if restoreSnapshot != "" {
		started, err := (managedruntime.Service{}).Start(ctx, managedruntime.StartRequest{
			Profile: managedguest.LinuxProfile,
			Host:    whp.Host{},
			Spec:    windowsLinuxMachineSpec(req.MemoryMB, req.CPUs, req.Dmesg),
			Attachments: whp.LinuxManagedAttachments{
				FSDevices:       fsdevs,
				NetDevice:       windowsNetworkDevice(network),
				SnapshotDir:     strings.TrimSpace(req.SnapshotDir),
				RestoreSnapshot: restoreSnapshot,
			},
		}, onEvent)
		if err != nil {
			return nil, err
		}
		return &windowsInstance{
			managedInstanceCore: newWindowsManagedCore(started.Session, nil, vmruntime.WithDefaultEnv(nil), "/"),
			rootFS:              rootFS,
			fsdevs:              fsdevs,
			network:             network,
			dmesg:               req.Dmesg,
		}, nil
	}
	kernel, err := b.kernel.ReadKernel()
	if err != nil {
		return nil, err
	}
	modules, err := b.kernel.PlanModuleLoad(windowsRuntimeConfigVars(nil), windowsRuntimeModuleMap())
	if err != nil {
		return nil, err
	}
	initBin, err := guestinit.BuildForArch(ctx, b.guestInitCache, "arm64")
	if err != nil {
		return nil, fmt.Errorf("build guest init: %w", err)
	}
	initCfg := windowsGuestInitConfig(modules, true, req.InitSystem)
	initCfg.RootFSTag = vmruntime.RootFSTag
	initCfg.Env = vmruntime.WithDefaultEnv(nil)
	initCfg.WorkDir = "/"
	initCfg.Network = windowsNetworkGuestInitConfig(network)
	if strings.TrimSpace(req.SnapshotDir) != "" {
		initCfg.SnapshotMMIOBase = arm64vm.SnapshotBase
	}
	if err := applyRuntimeKernelMetadata(&initCfg, b.kernel, ""); err != nil {
		return nil, fmt.Errorf("read kernel metadata: %w", err)
	}
	initrd, err := vmruntime.BuildInitramfs(initBin, modules, initCfg)
	if err != nil {
		return nil, fmt.Errorf("build initramfs: %w", err)
	}
	started, err := (managedruntime.Service{}).Start(ctx, managedruntime.StartRequest{
		Profile: managedguest.LinuxProfile,
		Host:    whp.Host{},
		Spec:    windowsLinuxMachineSpec(req.MemoryMB, req.CPUs, req.Dmesg),
		Artifact: rootartifact.Artifact{
			Kernel: kernel,
			Initrd: initrd,
		},
		Attachments: whp.LinuxManagedAttachments{
			FSDevices:       fsdevs,
			NetDevice:       windowsNetworkDevice(network),
			SnapshotDir:     strings.TrimSpace(req.SnapshotDir),
			RestoreSnapshot: strings.TrimSpace(req.RestoreSnapshot),
		},
	}, onEvent)
	if err != nil {
		return nil, err
	}
	return &windowsInstance{
		managedInstanceCore: newWindowsManagedCore(started.Session, nil, vmruntime.WithDefaultEnv(nil), "/"),
		rootFS:              rootFS,
		fsdevs:              fsdevs,
		network:             network,
		dmesg:               req.Dmesg,
	}, nil
}

func (b *runtimeBackend) Run(ctx context.Context, req client.RunRequest) (client.ExecResponse, error) {
	inst, err := b.StartStream(ctx, client.CreateInstanceRequest{
		Image:         req.Image,
		InitSystem:    req.InitSystem,
		Kernel:        req.Kernel,
		Shares:        append([]client.ShareMount(nil), req.Shares...),
		Network:       req.Network,
		KernelModules: append([]string(nil), req.KernelModules...),
		MemoryMB:      req.MemoryMB,
		CPUs:          req.CPUs,
		NestedVirt:    req.NestedVirt,
		Dmesg:         req.Dmesg,
	}, nil)
	if err != nil {
		return client.ExecResponse{}, err
	}
	defer inst.Close()
	if len(req.Command) == 0 {
		return client.ExecResponse{ExitCode: 0}, nil
	}
	return inst.Exec(ctx, runExecRequest(req))
}

func (b *runtimeBackend) RunStream(ctx context.Context, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	inst, err := b.StartStream(ctx, client.CreateInstanceRequest{
		Image:         req.Image,
		InitSystem:    req.InitSystem,
		Kernel:        req.Kernel,
		Shares:        append([]client.ShareMount(nil), req.Shares...),
		Network:       req.Network,
		KernelModules: append([]string(nil), req.KernelModules...),
		MemoryMB:      req.MemoryMB,
		CPUs:          req.CPUs,
		NestedVirt:    req.NestedVirt,
		Dmesg:         req.Dmesg,
	}, nil)
	if err != nil {
		return err
	}
	defer inst.Close()
	return inst.ExecStream(ctx, runExecRequest(req), inputs, onEvent)
}

func (b *runtimeBackend) RunInInstance(ctx context.Context, inst Instance, runningImage string, req client.RunRequest) (client.ExecResponse, error) {
	targetImage := strings.TrimSpace(req.Image)
	if targetImage == "" || targetImage == runningImage {
		if err := mounts.AddRuntimeShares(ctx, inst, req.Shares); err != nil {
			return client.ExecResponse{}, err
		}
		return inst.Exec(ctx, runningVMExecRequest(req))
	}

	if err := execplan.CheckAlternateImageExec(inst); err != nil {
		return client.ExecResponse{}, err
	}
	session, ok := inst.(*windowsInstance)
	if !ok {
		return client.ExecResponse{}, fmt.Errorf("running instance does not support image mounts")
	}
	if b == nil || b.images == nil {
		return client.ExecResponse{}, fmt.Errorf("runtime backend is not configured")
	}
	image, err := b.images.Open(targetImage)
	if err != nil {
		return client.ExecResponse{}, err
	}
	if err := ensureWindowsARM64Image(image); err != nil {
		return client.ExecResponse{}, err
	}
	image = withWindowsRuntimeMountDirs(image)
	mountPath := windowsImageMountPath(targetImage)
	if err := mounts.MountAlternateImageWithShares(ctx, inst, session, mountPath, image, req.Shares); err != nil {
		return client.ExecResponse{}, err
	}

	execReq, err := execplan.ResolveRunRequest(req, mountPath, execplan.Resolver{
		Root:           image.RootFS,
		BaseEnv:        image.Config.Env,
		DefaultWorkDir: image.Config.WorkingDir,
		Env:            windowsEffectiveExecEnv,
	})
	if err != nil {
		return client.ExecResponse{}, err
	}
	return inst.Exec(ctx, execReq)
}

func (b *runtimeBackend) RunInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	targetImage := strings.TrimSpace(req.Image)
	if targetImage == "" || targetImage == runningImage {
		if err := mounts.AddRuntimeShares(ctx, inst, req.Shares); err != nil {
			return err
		}
		return inst.ExecStream(ctx, runningVMExecRequest(req), inputs, onEvent)
	}

	if err := execplan.CheckAlternateImageExec(inst); err != nil {
		return err
	}
	session, ok := inst.(*windowsInstance)
	if !ok {
		return fmt.Errorf("running instance does not support image mounts")
	}
	if b == nil || b.images == nil {
		return fmt.Errorf("runtime backend is not configured")
	}
	image, err := b.images.Open(targetImage)
	if err != nil {
		return err
	}
	if err := ensureWindowsARM64Image(image); err != nil {
		return err
	}
	image = withWindowsRuntimeMountDirs(image)
	mountPath := windowsImageMountPath(targetImage)
	if err := mounts.MountAlternateImageWithShares(ctx, inst, session, mountPath, image, req.Shares); err != nil {
		return err
	}

	execReq, err := execplan.ResolveRunRequest(req, mountPath, execplan.Resolver{
		Root:           image.RootFS,
		BaseEnv:        image.Config.Env,
		DefaultWorkDir: image.Config.WorkingDir,
		Env:            windowsEffectiveExecEnv,
	})
	if err != nil {
		return err
	}
	return inst.ExecStream(ctx, execReq, inputs, onEvent)
}

func (b *runtimeBackend) ExecInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	targetImage := strings.TrimSpace(req.Image)
	req.Image = ""
	if targetImage == "" || targetImage == runningImage {
		return inst.ExecStream(ctx, req, inputs, onEvent)
	}

	if err := execplan.CheckAlternateImageExec(inst); err != nil {
		return err
	}
	session, ok := inst.(*windowsInstance)
	if !ok {
		return fmt.Errorf("running instance does not support image mounts")
	}
	if b == nil || b.images == nil {
		return fmt.Errorf("runtime backend is not configured")
	}
	image, err := b.images.Open(targetImage)
	if err != nil {
		return err
	}
	if err := ensureWindowsARM64Image(image); err != nil {
		return err
	}
	image = withWindowsRuntimeMountDirs(image)
	mountPath := windowsImageMountPath(targetImage)
	if err := mounts.MountAlternateImageWithShares(ctx, inst, session, mountPath, image, nil); err != nil {
		return err
	}
	req.RootDir = rootDirWithinMount(mountPath, req.RootDir)
	return inst.ExecStream(ctx, req, inputs, onEvent)
}

func ensureWindowsARM64Image(image *oci.Image) error {
	if image == nil {
		return nil
	}
	arch := strings.TrimSpace(image.Architecture)
	if arch == "" || arch == "arm64" || arch == "amd64" {
		return nil
	}
	name := strings.TrimSpace(image.Name)
	if name == "" {
		name = "image"
	}
	return fmt.Errorf("windows/arm64 runtime supports only arm64 or amd64 images; %s is %s", name, arch)
}

type windowsInstance struct {
	*managedInstanceCore
	image   *oci.Image
	rootFS  virtio.ShareMounter
	fsdevs  []*virtio.FS
	dmesg   bool
	network *windowsNetworkRuntime
	mounts  *mounts.State
}

func (i *windowsInstance) ManagedCapabilities() guestCapabilities {
	return managedguest.LinuxProfile.Caps
}

func newWindowsManagedCore(session managedsession.Session, image *oci.Image, baseEnv []string, workDir string) *managedInstanceCore {
	var root imagefs.Directory
	if image != nil {
		root = image.RootFS
	}
	return hostmanaged.NewCore(hostmanaged.Config{
		OSName:         "Linux",
		Session:        session,
		Root:           root,
		BaseEnv:        baseEnv,
		WorkDir:        workDir,
		Capabilities:   managedguest.LinuxProfile.Caps,
		Env:            windowsEffectiveExecEnv,
		MissingRootErr: "running instance does not have a default image root filesystem",
	})
}

func windowsLinuxMachineSpec(memoryMB uint64, cpus int, dmesg bool) machine.Spec {
	return machine.Spec{
		Guest:    "Linux",
		Arch:     "arm64",
		MemoryMB: memoryMB,
		CPUs:     cpus,
		Dmesg:    dmesg,
		Boot:     machine.BootSpec{Kind: "linux"},
		Control:  machine.ControlSpec{Kind: "vsock", Port: vmruntime.ControlPort},
	}
}

func (i *windowsInstance) core() *managedInstanceCore {
	if i == nil {
		return nil
	}
	return i.managedInstanceCore
}

func (i *windowsInstance) VirtioFSStats() []virtio.FSStats {
	if i == nil {
		return nil
	}
	return virtioFSStats(i.fsdevs)
}

func (i *windowsInstance) BackingUsage() (uint64, uint64, uint64, error) {
	if i == nil {
		return 0, 0, 0, nil
	}
	return virtioFSBackingUsage(i.fsdevs)
}

func (i *windowsInstance) BackingMetadataUsage() (uint64, uint64) {
	if i == nil {
		return 0, 0
	}
	return virtioFSBackingMetadataUsage(i.fsdevs)
}

func (i *windowsInstance) BackingCombinedUsage() (uint64, uint64) {
	if i == nil {
		return 0, 0
	}
	return virtioFSBackingCombinedUsage(i.fsdevs)
}

func (i *windowsInstance) BackingSnapshot() virtio.FSBackingUsageSnapshot {
	if i == nil {
		return virtio.FSBackingUsageSnapshot{}
	}
	return virtioFSBackingSnapshot(i.fsdevs)
}

func (i *windowsInstance) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	return i.core().Exec(ctx, req)
}

func (i *windowsInstance) ExecStream(ctx context.Context, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return i.core().ExecStream(ctx, req, inputs, onEvent)
}

func (i *windowsInstance) ConsoleHistory(ctx context.Context) (string, error) {
	return i.core().ConsoleHistory(ctx)
}

func (i *windowsInstance) AddShare(ctx context.Context, share client.ShareMount) error {
	_ = ctx
	if i == nil || i.rootFS == nil {
		return mounts.AddRuntimeShareMount(nil, nil, nil, share, "shares", nil)
	}
	return i.mounts.AddShare(i.rootFS, share, "shares", func(share client.ShareMount) (virtio.ShareMount, error) {
		return mounts.BuildRuntimeDirectoryShare(share, arm64vm.BuildShareMount)
	})
}

func (i *windowsInstance) AddImage(ctx context.Context, mountPath string, image *oci.Image) error {
	_ = ctx
	if i == nil {
		return mounts.AddImageMount(nil, nil, nil, mountPath, image, nil)
	}
	return i.mounts.AddImage(i.rootFS, mountPath, image, mounts.ImageFSBackend(image))
}

func (i *windowsInstance) AddPortForward(ctx context.Context, forward client.PortForward) error {
	if i == nil || i.network == nil {
		return netstate.AddManagedNetworkPortForward(ctx, nil, forward)
	}
	return netstate.AddManagedNetworkPortForward(ctx, i.network.networkRuntime, forward)
}

func (i *windowsInstance) AllowServiceProxyPort(ctx context.Context, port int) error {
	if i == nil || i.network == nil {
		return netstate.AllowManagedNetworkServiceProxyPort(ctx, nil, port)
	}
	return netstate.AllowManagedNetworkServiceProxyPort(ctx, i.network.networkRuntime, port)
}

func (i *windowsInstance) Wait() error {
	if i == nil {
		return nil
	}
	return i.core().Wait()
}

func (i *windowsInstance) Close() error {
	if i == nil {
		return nil
	}
	var session managedsession.Session
	if core := i.core(); core != nil {
		session = core.Session()
	}
	var network hostmanaged.NetworkCloser
	if i.network != nil {
		network = i.network
	}
	return hostmanaged.CloseSessionWithNetwork(session, network)
}

func (i *windowsInstance) NetworkIPv4() string {
	if i == nil || i.network == nil {
		return ""
	}
	return netstate.IPv4(i.network.networkRuntime, "")
}

func windowsGuestInitConfig(modules []alpine.Module, managedExec bool, initSystem string) vmruntime.GuestInitConfig {
	cfg := vmruntime.GuestInitConfig{
		Modules:           vmruntime.ModulePaths(modules),
		InitSystem:        strings.TrimSpace(initSystem),
		ReadyMarker:       windowsInitReadyMarker,
		BeginMarker:       vmruntime.CommandBeginMarker,
		OutputMarkerPref:  vmruntime.CommandOutputMarker,
		ErrorMarkerPref:   vmruntime.CommandErrorMarker,
		ControlMarkerPref: vmruntime.CommandControlMarker,
		UsageMarkerPref:   vmruntime.CommandUsageMarker,
		ExitMarkerPrefix:  vmruntime.CommandExitMarkerPref,
		UnixTime:          time.Now().Unix(),
	}
	if managedExec {
		cfg.VsockPort = vmruntime.ControlPort
	}
	return cfg
}

func windowsRuntimeConfigVars(image *oci.Image) []string {
	vars := []string{"CONFIG_VIRTIO_MMIO", "CONFIG_FUSE_FS", "CONFIG_VIRTIO_FS", "CONFIG_VSOCKETS", "CONFIG_VIRTIO_VSOCKETS", "CONFIG_HW_RANDOM", "CONFIG_HW_RANDOM_VIRTIO", "CONFIG_VIRTIO_NET", "CONFIG_OVERLAY_FS"}
	if NeedsAMD64Emulation(image) {
		vars = append(vars, "CONFIG_BINFMT_MISC")
	}
	return vars
}

func windowsRuntimeModuleMap() map[string]string {
	return map[string]string{
		"CONFIG_VIRTIO_MMIO":      "kernel/drivers/virtio/virtio_mmio.ko.gz",
		"CONFIG_FUSE_FS":          "kernel/fs/fuse/fuse.ko.gz",
		"CONFIG_VIRTIO_FS":        "kernel/fs/fuse/virtiofs.ko.gz",
		"CONFIG_VSOCKETS":         "kernel/net/vmw_vsock/vsock.ko.gz",
		"CONFIG_VIRTIO_VSOCKETS":  "kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.gz",
		"CONFIG_HW_RANDOM":        "kernel/drivers/char/hw_random/rng-core.ko.gz",
		"CONFIG_HW_RANDOM_VIRTIO": "kernel/drivers/char/hw_random/virtio-rng.ko.gz",
		"CONFIG_VIRTIO_NET":       "kernel/drivers/net/virtio_net.ko.gz",
		"CONFIG_OVERLAY_FS":       "kernel/fs/overlayfs/overlay.ko.gz",
		"CONFIG_BINFMT_MISC":      "kernel/fs/binfmt_misc.ko.gz",
	}
}

func withWindowsRuntimeMountDirs(image *oci.Image) *oci.Image {
	if image == nil || image.RootFS == nil {
		return image
	}
	overlay := imagefs.NewOverlay(image.RootFS)
	for _, dir := range []string{"/dev", "/proc", "/sys", "/run"} {
		_ = overlay.AddDir(dir, fs.ModeDir|0o755)
	}
	_ = overlay.AddDir("/tmp", fs.ModeDir|0o1777)
	cloned := *image
	cloned.RootFS = overlay.Root()
	return &cloned
}

func blankWindowsRuntimeRootFS() imagefs.Directory {
	overlay := imagefs.NewOverlay(nil)
	for _, dir := range []string{"/dev", "/proc", "/sys", "/run", "/.ccx3", "/.ccx3/images"} {
		_ = overlay.AddDir(dir, fs.ModeDir|0o755)
	}
	_ = overlay.AddDir("/tmp", fs.ModeDir|0o1777)
	return overlay.Root()
}

func windowsEffectiveExecEnv(base, overrides []string, replace bool) []string {
	if replace {
		return vmruntime.WithDefaultEnv(overrides)
	}
	return vmruntime.WithDefaultEnv(vmruntime.MergeEnv(base, overrides))
}

func windowsImageMountPath(image string) string {
	replacer := strings.NewReplacer("/", "_", ":", "_", "@", "_", " ", "_")
	return path.Join("/.ccx3", "images", replacer.Replace(image))
}
