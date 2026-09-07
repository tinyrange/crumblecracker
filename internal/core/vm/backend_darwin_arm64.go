//go:build darwin && arm64

package vm

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/guestinit"
	"github.com/tinyrange/crumblecracker/internal/core/hv/hvf"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/kernel/ubuntu"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	managedruntime "github.com/tinyrange/crumblecracker/internal/core/managed/runtime"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vm/execplan"
	hvfhost "github.com/tinyrange/crumblecracker/internal/core/vm/host/hvf"
	"github.com/tinyrange/crumblecracker/internal/core/vm/mounts"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

var debugTiming = strings.TrimSpace(os.Getenv("CCX3_DEBUG_TIMING")) != ""

func timingLog(format string, args ...any) {
	if !debugTiming {
		return
	}
	fmt.Fprintf(os.Stderr, "ccx3 timing: "+format+"\n", args...)
}

type runtimeBackend struct {
	kernel           *alpine.Manager
	images           *oci.Store
	guestInitCache   string
	openGLShareGroup func() (context, pixelFormat uintptr)
}

type runtimeKernelProvider interface {
	ReadKernel() ([]byte, error)
	PlanModuleLoad([]string, map[string]string) ([]alpine.Module, error)
}

type runtimeKernelMetadataProvider interface {
	ReadKernelMetadata() (alpine.KernelMetadata, error)
}

func readRuntimeKernelProviderMetadata(provider runtimeKernelProvider) (alpine.KernelMetadata, error) {
	metadataProvider, ok := provider.(runtimeKernelMetadataProvider)
	if !ok {
		return alpine.KernelMetadata{}, nil
	}
	return metadataProvider.ReadKernelMetadata()
}

func NewRuntimeBackend(kernel *alpine.Manager, images *oci.Store, guestInitCache string) Backend {
	return &runtimeBackend{kernel: kernel, images: images, guestInitCache: guestInitCache}
}

func (b *runtimeBackend) kernelProvider(flavor string) runtimeKernelProvider {
	if path, ok := customKernelPath(flavor); ok {
		return customKernelProvider{path: path, modules: b.kernel}
	}
	if normalizeRuntimeKernel(flavor) == "ubuntu" && b.images != nil {
		return ubuntu.NewManager(filepath.Join(b.images.Root(), "_kernels", "ubuntu"))
	}
	return b.kernel
}

func normalizeRuntimeKernel(flavor string) string {
	flavor = strings.ToLower(strings.TrimSpace(flavor))
	switch flavor {
	case "", "default", "alpine":
		return ""
	default:
		return flavor
	}
}

func runtimeKernelRequirements(flavor string, image *oci.Image, network bool, extra []string) ([]string, map[string]string) {
	if normalizeRuntimeKernel(flavor) == "ubuntu" {
		return ubuntuRuntimeKernelRequirements(extra)
	}
	return alpineRuntimeKernelRequirements(network, extra)
}

func alpineRuntimeKernelRequirements(network bool, extra []string) ([]string, map[string]string) {
	configVars := []string{"CONFIG_VIRTIO_MMIO", "CONFIG_VIRTIO_BALLOON", "CONFIG_FUSE_FS", "CONFIG_VIRTIO_FS", "CONFIG_VSOCKETS", "CONFIG_VIRTIO_VSOCKETS", "CONFIG_HW_RANDOM", "CONFIG_HW_RANDOM_VIRTIO"}
	if network {
		configVars = append(configVars, "CONFIG_VIRTIO_NET")
	}
	configVars = append(configVars, extra...)
	moduleMap := map[string]string{
		"CONFIG_VIRTIO_MMIO":      "kernel/drivers/virtio/virtio_mmio.ko.gz",
		"CONFIG_VIRTIO_BALLOON":   "kernel/drivers/virtio/virtio_balloon.ko.gz",
		"CONFIG_FUSE_FS":          "kernel/fs/fuse/fuse.ko.gz",
		"CONFIG_VIRTIO_FS":        "kernel/fs/fuse/virtiofs.ko.gz",
		"CONFIG_VSOCKETS":         "kernel/net/vmw_vsock/vsock.ko.gz",
		"CONFIG_VIRTIO_VSOCKETS":  "kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.gz",
		"CONFIG_HW_RANDOM":        "kernel/drivers/char/hw_random/rng-core.ko.gz",
		"CONFIG_HW_RANDOM_VIRTIO": "kernel/drivers/char/hw_random/virtio-rng.ko.gz",
		"CONFIG_VIRTIO_NET":       "kernel/drivers/net/virtio_net.ko.gz",
		"CONFIG_DRM_VIRTIO_GPU":   "kernel/drivers/gpu/drm/virtio/virtio-gpu.ko.gz",
		"CONFIG_VIRTIO_INPUT":     "kernel/drivers/virtio/virtio_input.ko.gz",
		"CONFIG_INPUT_EVDEV":      "kernel/drivers/input/evdev.ko.gz",
		"CONFIG_BINFMT_MISC":      "kernel/fs/binfmt_misc.ko.gz",
	}
	return configVars, moduleMap
}

func ubuntuRuntimeKernelRequirements(extra []string) ([]string, map[string]string) {
	configVars := []string{
		"CONFIG_VIRTIO_MMIO",
		"CONFIG_VIRTIO_BALLOON",
		"CONFIG_FUSE_FS",
		"CONFIG_VIRTIO_FS",
		"CONFIG_VSOCKETS",
		"CONFIG_VIRTIO_VSOCKETS",
		"CONFIG_HW_RANDOM",
		"CONFIG_HW_RANDOM_VIRTIO",
		"CONFIG_VIRTIO_NET",
		"CONFIG_OVERLAY_FS",
		"CONFIG_NF_TABLES",
		"CONFIG_IP_NF_IPTABLES",
		"CONFIG_BINFMT_MISC",
		"MODULE:autofs4",
	}
	configVars = append(configVars, extra...)
	moduleMap := map[string]string{
		"CONFIG_VIRTIO_FS":        "kernel/fs/fuse/virtiofs.ko.zst",
		"CONFIG_VIRTIO_BALLOON":   "kernel/drivers/virtio/virtio_balloon.ko.zst",
		"CONFIG_VSOCKETS":         "kernel/net/vmw_vsock/vsock.ko.zst",
		"CONFIG_VIRTIO_VSOCKETS":  "kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.zst",
		"CONFIG_HW_RANDOM_VIRTIO": "kernel/drivers/char/hw_random/virtio-rng.ko.zst",
		"CONFIG_VIRTIO_NET":       "kernel/drivers/net/virtio_net.ko.zst",
		"CONFIG_DRM_VIRTIO_GPU":   "kernel/drivers/gpu/drm/virtio/virtio-gpu.ko.zst",
		"CONFIG_VIRTIO_INPUT":     "kernel/drivers/virtio/virtio_input.ko.zst",
		"CONFIG_INPUT_EVDEV":      "kernel/drivers/input/evdev.ko.zst",
		"CONFIG_OVERLAY_FS":       "kernel/fs/overlayfs/overlay.ko.zst",
		"CONFIG_NF_TABLES":        "kernel/net/netfilter/nf_tables.ko.zst",
		"CONFIG_IP_NF_IPTABLES":   "kernel/net/ipv4/netfilter/ip_tables.ko.zst",
		"CONFIG_BINFMT_MISC":      "kernel/fs/binfmt_misc.ko.zst",
		"MODULE:autofs4":          "kernel/fs/autofs/autofs4.ko.zst",
	}
	return configVars, moduleMap
}

func (b *runtimeBackend) Start(ctx context.Context, req client.CreateInstanceRequest) (Instance, error) {
	return b.StartStream(ctx, req, nil)
}

func (b *runtimeBackend) StartBlank(ctx context.Context, req client.StartInstanceRequest) (Instance, error) {
	return b.StartBlankStream(ctx, req, nil)
}

func (b *runtimeBackend) StartStream(ctx context.Context, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (Instance, error) {

	start := time.Now()
	network, err := newDarwinARM64NetworkRuntime(req.ID, req.Network)
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
	runReq, err := b.buildStartRequest(ctx, req, network)
	if err != nil {
		return nil, err
	}
	timingLog("runtime.Start buildStartRequest took=%s image=%q", time.Since(start), req.Image)
	started, err := (managedruntime.Service{}).Start(ctx, managedruntime.StartRequest{
		Profile:     managedguest.LinuxProfile,
		Host:        hvf.Host{},
		Spec:        darwinLinuxMachineSpec(req.MemoryMB, req.CPUs, req.Dmesg),
		Attachments: hvf.LinuxManagedAttachments{RunRequest: runReq},
	}, onEvent)
	if err != nil {
		return nil, err
	}
	containerSession, ok := started.Session.(*hvf.ContainerSession)
	if !ok {
		_ = started.Session.Close()
		return nil, fmt.Errorf("hvf host returned %T, want *hvf.ContainerSession", started.Session)
	}
	timingLog("runtime.Start hvf.StartContainer took=%s image=%q", time.Since(start), req.Image)
	return newDarwinInstance(containerSession, network, strings.TrimSpace(req.Image), req.DefaultUser), nil
}

func (b *runtimeBackend) StartBlankStream(
	ctx context.Context,
	req client.StartInstanceRequest,
	onEvent func(client.BootEvent) error,
) (Instance, error) {

	start := time.Now()
	network, err := newDarwinARM64NetworkRuntime(req.ID, req.Network)
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
	runReq, err := b.buildBlankStartRequest(ctx, req, network)
	if err != nil {
		return nil, err
	}
	timingLog("runtime.StartBlank buildBlankStartRequest took=%s", time.Since(start))
	started, err := (managedruntime.Service{}).Start(ctx, managedruntime.StartRequest{
		Profile:     managedguest.LinuxProfile,
		Host:        hvf.Host{},
		Spec:        darwinLinuxMachineSpec(req.MemoryMB, req.CPUs, req.Dmesg),
		Attachments: hvf.LinuxManagedAttachments{RunRequest: runReq},
	}, onEvent)
	if err != nil {
		return nil, err
	}
	containerSession, ok := started.Session.(*hvf.ContainerSession)
	if !ok {
		_ = started.Session.Close()
		return nil, fmt.Errorf("hvf host returned %T, want *hvf.ContainerSession", started.Session)
	}
	timingLog("runtime.StartBlank hvf.StartContainer took=%s", time.Since(start))
	return newDarwinInstance(containerSession, network, strings.TrimSpace(req.Image), req.DefaultUser), nil
}

func (b *runtimeBackend) Run(ctx context.Context, req client.RunRequest) (client.ExecResponse, error) {
	network, err := newDarwinARM64NetworkRuntime(req.ID, req.Network)
	if err != nil {
		return client.ExecResponse{}, err
	}
	if network != nil {
		defer network.Close()
	}
	runReq, err := b.buildRunRequest(ctx, req, network)
	if err != nil {
		return client.ExecResponse{}, err
	}
	result, err := hvf.RunContainer(ctx, runReq)
	if err != nil {
		return client.ExecResponse{}, err
	}
	return client.ExecResponse{
		ExitCode: result.ExitCode,
		Output:   result.Output,
	}, nil
}

func (b *runtimeBackend) RunStream(ctx context.Context, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	inst, err := b.StartStream(ctx, client.CreateInstanceRequest{
		Image:          req.Image,
		InitSystem:     req.InitSystem,
		Kernel:         req.Kernel,
		Shares:         append([]client.ShareMount(nil), req.Shares...),
		Network:        req.Network,
		KernelModules:  append([]string(nil), req.KernelModules...),
		MemoryMB:       req.MemoryMB,
		BalloonMB:      req.BalloonMB,
		CPUs:           req.CPUs,
		NestedVirt:     req.NestedVirt,
		Dmesg:          req.Dmesg,
		TimeoutSeconds: req.TimeoutSeconds,
	}, nil)
	if err != nil {
		return err
	}
	defer inst.Close()
	return inst.ExecStream(ctx, runExecRequest(req), inputs, onEvent)
}

func (b *runtimeBackend) RunInInstance(
	ctx context.Context,
	inst Instance,
	runningImage string,
	req client.RunRequest,
) (client.ExecResponse, error) {
	targetImage := strings.TrimSpace(req.Image)
	if sameRuntimeImage(targetImage, runningImage) {
		if err := mounts.AddRuntimeShares(ctx, inst, req.Shares); err != nil {
			return client.ExecResponse{}, err
		}
		return inst.Exec(ctx, runningVMExecRequest(req))
	}

	if err := execplan.CheckAlternateImageExec(inst); err != nil {
		return client.ExecResponse{}, err
	}

	session, ok := darwinContainerSession(inst)
	if !ok {
		return client.ExecResponse{}, fmt.Errorf("running instance does not support image mounts")
	}

	image, err := b.images.Open(targetImage)
	if err != nil {
		return client.ExecResponse{}, err
	}
	image = withRuntimeMountDirs(image)
	mountPath := hvfhost.ImageMountPath(targetImage)
	if err := mounts.MountAlternateImageWithShares(ctx, inst, session, mountPath, image, req.Shares); err != nil {
		return client.ExecResponse{}, err
	}

	execReq, err := execplan.ResolveRunRequest(req, mountPath, execplan.Resolver{
		Root:           image.RootFS,
		BaseEnv:        image.Config.Env,
		DefaultWorkDir: image.Config.WorkingDir,
		Env: func(base, overrides []string, _ bool) []string {
			return mergeRuntimeEnv(append([]string(nil), base...), overrides)
		},
	})
	if err != nil {
		return client.ExecResponse{}, err
	}
	return inst.Exec(ctx, execReq)
}

func (b *runtimeBackend) RunInInstanceStream(
	ctx context.Context,
	inst Instance,
	runningImage string,
	req client.RunRequest,
	inputs <-chan client.ExecInput,
	onEvent func(client.ExecEvent) error,
) error {
	targetImage := strings.TrimSpace(req.Image)
	if sameRuntimeImage(targetImage, runningImage) {
		if err := mounts.AddRuntimeShares(ctx, inst, req.Shares); err != nil {
			return err
		}
		return inst.ExecStream(ctx, runningVMExecRequest(req), inputs, onEvent)
	}

	if err := execplan.CheckAlternateImageExec(inst); err != nil {
		return err
	}

	session, ok := darwinContainerSession(inst)
	if !ok {
		return fmt.Errorf("running instance does not support image mounts")
	}

	image, err := b.images.Open(targetImage)
	if err != nil {
		return err
	}
	image = withRuntimeMountDirs(image)
	mountPath := hvfhost.ImageMountPath(targetImage)
	if err := mounts.MountAlternateImageWithShares(ctx, inst, session, mountPath, image, req.Shares); err != nil {
		return err
	}

	execReq, err := execplan.ResolveRunRequest(req, mountPath, execplan.Resolver{
		Root:           image.RootFS,
		BaseEnv:        image.Config.Env,
		DefaultWorkDir: image.Config.WorkingDir,
		Env: func(base, overrides []string, _ bool) []string {
			return mergeRuntimeEnv(append([]string(nil), base...), overrides)
		},
	})
	if err != nil {
		return err
	}
	return inst.ExecStream(ctx, execReq, inputs, onEvent)
}

func (b *runtimeBackend) ExecInInstanceStream(ctx context.Context, inst Instance, runningImage string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	targetImage := strings.TrimSpace(req.Image)
	req.Image = ""
	if sameRuntimeImage(targetImage, runningImage) {
		return inst.ExecStream(ctx, req, inputs, onEvent)
	}

	if err := execplan.CheckAlternateImageExec(inst); err != nil {
		return err
	}

	session, ok := darwinContainerSession(inst)
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
	image = withRuntimeMountDirs(image)
	mountPath := hvfhost.ImageMountPath(targetImage)
	if err := mounts.MountAlternateImageWithShares(ctx, inst, session, mountPath, image, nil); err != nil {
		return err
	}
	req.RootDir = rootDirWithinMount(mountPath, req.RootDir)
	return inst.ExecStream(ctx, req, inputs, onEvent)
}

func (b *runtimeBackend) buildBaseRequest(ctx context.Context, imageName string, initSystem string, kernelFlavor string, kernelModules []string, memoryMB uint64, balloonMB uint64, cpus int, nestedVirt bool, amd64Emulation bool, dmesg bool, network *darwinNetworkRuntime) (vmruntime.RunRequest, error) {
	start := time.Now()
	if b.kernel == nil || b.images == nil {
		return vmruntime.RunRequest{}, fmt.Errorf("runtime backend is not configured")
	}
	image, err := b.images.Open(imageName)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	image = withRuntimeMountDirs(image)
	timing.Since(ctx, "backend.image_open", start)
	timingLog("buildBaseRequest image open took=%s image=%q", time.Since(start), imageName)
	start = time.Now()
	kernelProvider := b.kernelProvider(kernelFlavor)
	kernel, err := kernelProvider.ReadKernel()
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	timing.Since(ctx, "backend.read_kernel", start)
	timingLog("buildBaseRequest ReadKernel took=%s image=%q", time.Since(start), imageName)
	start = time.Now()
	configVars, moduleMap := runtimeKernelRequirements(kernelFlavor, image, network != nil, kernelModules)
	if WantsAMD64Emulation(image, amd64Emulation) {
		configVars = append(configVars, "CONFIG_BINFMT_MISC")
	}
	modules, err := kernelProvider.PlanModuleLoad(configVars, moduleMap)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	kernelMetadata, err := readRuntimeKernelProviderMetadata(kernelProvider)
	if err != nil {
		return vmruntime.RunRequest{}, fmt.Errorf("read kernel metadata: %w", err)
	}
	timing.Since(ctx, "backend.plan_module_load", start)
	timingLog("buildBaseRequest PlanModuleLoad took=%s image=%q modules=%d", time.Since(start), imageName, len(modules))
	start = time.Now()
	qemuX8664, err := PrepareAMD64EmulatorForGuest(ctx, image, amd64Emulation, b.kernel.ExtractPackageFile)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	timing.Since(ctx, "backend.prepare_amd64_emulator", start)
	timingLog("buildBaseRequest loadAMD64Emulator took=%s image=%q emulator_path=%q", time.Since(start), imageName, qemuX8664)
	start = time.Now()
	guestInitCache := b.guestInitCache
	if guestInitCache == "" {
		guestInitCache = filepath.Join(b.images.Root(), "_guestinit")
	}
	initBin, err := guestinit.Build(ctx, guestInitCache)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	timing.Since(ctx, "backend.guestinit_build", start)
	timingLog("buildBaseRequest guestinit.Build took=%s image=%q init_bytes=%d", time.Since(start), imageName, len(initBin))
	return vmruntime.RunRequest{
		Kernel:            kernel,
		KernelRelease:     kernelMetadata.Release,
		ModuleSymvers:     kernelMetadata.ModuleSymvers,
		Init:              initBin,
		AMD64EmulatorPath: qemuX8664,
		Modules:           modules,
		Image:             image,
		InitSystem:        initSystem,
		MemoryMB:          memoryMB,
		BalloonMB:         balloonMB,
		CPUs:              cpus,
		NestedVirt:        nestedVirt,
		Dmesg:             dmesg,
		Network:           network.guestInitConfig(),
		NetDevice:         networkDeviceDarwin(network),
		UnixTime:          time.Now().Unix(),
	}, ctx.Err()
}

func blankRuntimeRootFS() imagefs.Directory {
	overlay := imagefs.NewOverlay(nil)
	for _, dir := range []string{"/dev", "/proc", "/sys", "/run", "/.ccx3", "/.ccx3/images"} {
		_ = overlay.AddDir(dir, fs.ModeDir|0o755)
	}
	_ = overlay.AddDir("/tmp", fs.ModeDir|0o1777)
	return overlay.Root()
}

func darwinLinuxMachineSpec(memoryMB uint64, cpus int, dmesg bool) machine.Spec {
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

func withRuntimeMountDirs(image *oci.Image) *oci.Image {
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

func (b *runtimeBackend) buildStartRequest(ctx context.Context, req client.CreateInstanceRequest, networks ...*darwinNetworkRuntime) (vmruntime.RunRequest, error) {
	start := time.Now()
	var network *darwinNetworkRuntime
	if len(networks) > 0 {
		network = networks[0]
	}
	kernelModules := displayKernelModules(req.Display, req.KernelModules)
	runReq, err := b.buildBaseRequest(ctx, req.Image, req.InitSystem, req.Kernel, kernelModules, req.MemoryMB, req.BalloonMB, req.CPUs, req.NestedVirt, req.AMD64Emulation, req.Dmesg, network)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	shareStart := time.Now()
	if runReq.RootFS == nil {
		runReq.Shares = append(runReq.Shares, mounts.ConvertShareMounts(req.Shares)...)
	}
	persistentMounts, err := mounts.BuildPersistentImageMounts(filepath.Join(b.images.Root(), "_homes"), runReq.Image, req.PersistentMounts)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	runReq.Mounts = append(runReq.Mounts, persistentMounts...)
	runReq.Mounts = append(runReq.Mounts, hostMountsFromContext(ctx)...)
	runReq.Env = append([]string(nil), req.Env...)
	b.configureDisplayRequest(&runReq, req.Display)
	timing.Since(ctx, "backend.convert_share_mounts", shareStart)
	runReq.Persistent = true
	applyStartupSnapshotOptions(&runReq, req.SnapshotDir, req.RestoreSnapshot)
	timing.Since(ctx, "backend.build_start_request", start)
	return runReq, nil
}

func (b *runtimeBackend) buildBlankStartRequest(ctx context.Context, req client.StartInstanceRequest, network *darwinNetworkRuntime) (vmruntime.RunRequest, error) {
	if strings.TrimSpace(req.RestoreSnapshot) != "" {
		return b.buildBlankRestoreRequest(ctx, req, network)
	}
	if b.kernel == nil || b.images == nil {
		return vmruntime.RunRequest{}, fmt.Errorf("runtime backend is not configured")
	}
	var image *oci.Image
	imageName := strings.TrimSpace(req.Image)
	if imageName != "" {
		var err error
		image, err = b.images.Open(imageName)
		if err != nil {
			return vmruntime.RunRequest{}, err
		}
		image = withRuntimeMountDirs(image)
	}
	kernelProvider := b.kernelProvider(req.Kernel)
	kernel, err := kernelProvider.ReadKernel()
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	configVars, moduleMap := runtimeKernelRequirements(req.Kernel, image, network != nil, displayKernelModules(req.Display, req.KernelModules))
	if WantsAMD64Emulation(image, req.AMD64Emulation) {
		configVars = append(configVars, "CONFIG_BINFMT_MISC")
	}
	modules, err := kernelProvider.PlanModuleLoad(configVars, moduleMap)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	kernelMetadata, err := readRuntimeKernelProviderMetadata(kernelProvider)
	if err != nil {
		return vmruntime.RunRequest{}, fmt.Errorf("read kernel metadata: %w", err)
	}
	qemuX8664, err := PrepareAMD64EmulatorForGuest(ctx, image, req.AMD64Emulation, b.kernel.ExtractPackageFile)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	guestInitCache := b.guestInitCache
	if guestInitCache == "" {
		guestInitCache = filepath.Join(b.images.Root(), "_guestinit")
	}
	initBin, err := guestinit.Build(ctx, guestInitCache)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	var rootFS virtio.FSBackend
	shares := mounts.ConvertShareMounts(req.Shares)
	if rootFS == nil && image == nil {
		rootFS = virtio.NewImageFS(blankRuntimeRootFS(), "")
	} else if rootFS != nil {
		shares = nil
	}
	var persistentMounts []virtio.ShareMount
	if len(req.PersistentMounts) != 0 {
		if b.images == nil {
			return vmruntime.RunRequest{}, fmt.Errorf("persistent mounts require an image store")
		}
		persistentMounts, err = mounts.BuildPersistentImageMounts(filepath.Join(b.images.Root(), "_homes"), image, req.PersistentMounts)
		if err != nil {
			return vmruntime.RunRequest{}, err
		}
	}
	runReq := vmruntime.RunRequest{
		Kernel:            kernel,
		KernelRelease:     kernelMetadata.Release,
		ModuleSymvers:     kernelMetadata.ModuleSymvers,
		Init:              initBin,
		AMD64EmulatorPath: qemuX8664,
		Modules:           modules,
		Image:             image,
		InitSystem:        req.InitSystem,
		Env:               append([]string(nil), req.Env...),
		RootFS:            rootFS,
		Shares:            shares,
		Mounts:            append(persistentMounts, hostMountsFromContext(ctx)...),
		MemoryMB:          req.MemoryMB,
		BalloonMB:         req.BalloonMB,
		CPUs:              req.CPUs,
		NestedVirt:        req.NestedVirt,
		Dmesg:             req.Dmesg,
		Persistent:        true,
		Network:           network.guestInitConfig(),
		NetDevice:         networkDeviceDarwin(network),
		SnapshotDir:       strings.TrimSpace(req.SnapshotDir),
		RestoreSnapshot:   strings.TrimSpace(req.RestoreSnapshot),
		UnixTime:          time.Now().Unix(),
	}
	b.configureDisplayRequest(&runReq, req.Display)
	return runReq, ctx.Err()
}

func (b *runtimeBackend) buildBlankRestoreRequest(ctx context.Context, req client.StartInstanceRequest, network *darwinNetworkRuntime) (vmruntime.RunRequest, error) {
	var image *oci.Image
	imageName := strings.TrimSpace(req.Image)
	if imageName != "" {
		if b.images == nil {
			return vmruntime.RunRequest{}, fmt.Errorf("runtime backend is not configured")
		}
		var err error
		image, err = b.images.Open(imageName)
		if err != nil {
			return vmruntime.RunRequest{}, err
		}
		image = withRuntimeMountDirs(image)
	}
	var rootFS virtio.FSBackend
	shares := mounts.ConvertShareMounts(req.Shares)
	if rootFS == nil {
		if image != nil {
			rootFS = virtio.NewImageFS(image.RootFS, image.RootFSDir)
		} else {
			rootFS = virtio.NewImageFS(blankRuntimeRootFS(), "")
		}
	} else {
		shares = nil
	}
	var persistentMounts []virtio.ShareMount
	var err error
	if len(req.PersistentMounts) != 0 {
		if b.images == nil {
			return vmruntime.RunRequest{}, fmt.Errorf("persistent mounts require an image store")
		}
		persistentMounts, err = mounts.BuildPersistentImageMounts(filepath.Join(b.images.Root(), "_homes"), image, req.PersistentMounts)
		if err != nil {
			return vmruntime.RunRequest{}, err
		}
	}
	runReq := vmruntime.RunRequest{
		Image:           image,
		InitSystem:      req.InitSystem,
		Env:             append([]string(nil), req.Env...),
		RootFS:          rootFS,
		Shares:          shares,
		Mounts:          append(persistentMounts, hostMountsFromContext(ctx)...),
		MemoryMB:        req.MemoryMB,
		BalloonMB:       req.BalloonMB,
		CPUs:            req.CPUs,
		NestedVirt:      req.NestedVirt,
		Dmesg:           req.Dmesg,
		Persistent:      true,
		Network:         network.guestInitConfig(),
		NetDevice:       networkDeviceDarwin(network),
		SnapshotDir:     strings.TrimSpace(req.SnapshotDir),
		RestoreSnapshot: strings.TrimSpace(req.RestoreSnapshot),
		UnixTime:        time.Now().Unix(),
	}
	b.configureDisplayRequest(&runReq, req.Display)
	return runReq, ctx.Err()
}

func (b *runtimeBackend) configureDisplayRequest(runReq *vmruntime.RunRequest, display *client.DisplayConfig) {
	runReq.DisplayWidth = displayWidthDarwin(display)
	runReq.DisplayHeight = displayHeightDarwin(display)
	if display == nil || !display.Accelerated3D || runReq.DisplayWidth == 0 || runReq.DisplayHeight == 0 {
		return
	}
	runReq.Accelerated3D = true
	if b.openGLShareGroup != nil {
		runReq.OpenGLShareContext, runReq.OpenGLSharePixelFormat = b.openGLShareGroup()
	}
}

func applyStartupSnapshotOptions(req *vmruntime.RunRequest, snapshotDir, restoreSnapshot string) {
	req.SnapshotDir = strings.TrimSpace(snapshotDir)
	req.RestoreSnapshot = strings.TrimSpace(restoreSnapshot)
}

func displayKernelModules(display *client.DisplayConfig, modules []string) []string {
	out := append([]string(nil), modules...)
	if display != nil {
		out = append(out, "CONFIG_DRM_VIRTIO_GPU", "CONFIG_VIRTIO_INPUT", "CONFIG_INPUT_EVDEV")
	}
	return out
}

func displayWidthDarwin(display *client.DisplayConfig) uint32 {
	if display == nil {
		return 0
	}
	return display.Width
}

func displayHeightDarwin(display *client.DisplayConfig) uint32 {
	if display == nil {
		return 0
	}
	return display.Height
}

func (b *runtimeBackend) buildRunRequest(ctx context.Context, req client.RunRequest, network *darwinNetworkRuntime) (vmruntime.RunRequest, error) {
	runReq, err := b.buildBaseRequest(ctx, req.Image, req.InitSystem, req.Kernel, req.KernelModules, req.MemoryMB, req.BalloonMB, req.CPUs, req.NestedVirt, false, req.Dmesg, network)
	if err != nil {
		return vmruntime.RunRequest{}, err
	}
	runReq.Shares = append(runReq.Shares, mounts.ConvertShareMounts(req.Shares)...)
	runReq.Command = append([]string(nil), req.Command...)
	runReq.Env = append([]string(nil), req.Env...)
	runReq.WorkDir = req.WorkDir
	runReq.User = req.User
	return runReq, nil
}

func mergeRuntimeEnv(base []string, extra []string) []string {
	if len(extra) == 0 {
		return append([]string(nil), base...)
	}
	index := map[string]int{}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range extra {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		if idx, ok := index[key]; ok {
			out[idx] = kv
			continue
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	return out
}
