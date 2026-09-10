package desktopapp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
	appruntime "github.com/tinyrange/crumblecracker/internal/runtime"
	"github.com/tinyrange/crumblecracker/internal/version"
)

type headlessPrepared struct {
	image headlessImage
	name  string
}
type headlessNativeRequest struct {
	ctx     context.Context
	api     *appruntime.Runtime
	id      string
	request headlessGlassRequest
	ready   func(int, int)
	done    chan error
}
type headlessRuntimeDriver struct {
	mu         sync.Mutex
	pullGate   chan struct{}
	cache      string
	config     Config
	pullAPI    *appruntime.Runtime
	prepared   map[string]headlessPrepared
	references map[string]string
	machines   map[string]*appruntime.Runtime
	native     chan headlessNativeRequest
}

func headlessBuildVersion() string { return version.Current().Version }
func newHeadlessRuntimeDriver(cache string, config Config) (*headlessRuntimeDriver, error) {
	api, err := appruntime.New(appruntime.Options{CacheDir: cache})
	if err != nil {
		return nil, err
	}
	return &headlessRuntimeDriver{cache: cache, config: config, pullAPI: api, pullGate: make(chan struct{}, 1), prepared: map[string]headlessPrepared{}, references: map[string]string{}, machines: map[string]*appruntime.Runtime{}, native: make(chan headlessNativeRequest)}, nil
}
func (d *headlessRuntimeDriver) Check(ctx context.Context) headlessVirt {
	backend := map[string]string{"darwin": "hvf", "linux": "kvm", "windows": "whp"}[runtime.GOOS]
	supported := backend != "" && (runtime.GOARCH == "arm64" || runtime.GOARCH == "amd64")
	if backend == "" {
		backend = "none"
	}
	r := headlessVirt{Supported: supported, Backend: backend}
	state, err := d.pullAPI.VMSupportedContext(ctx)
	if err == nil && state.Supported {
		r.Accessible = true
		return r
	}
	code := "hypervisor_unavailable"
	message := "The current process cannot access the host hypervisor"
	if !supported {
		code = "unsupported_platform"
	} else if err != nil {
		code = "probe_failed"
	} else if strings.Contains(strings.ToLower(state.Error), "permission") {
		code = "permission_denied"
	}
	r.Reason = &headlessVirtReason{code, message}
	return r
}
func (d *headlessRuntimeDriver) resolutionPath(reference string) string {
	sum := sha256.Sum256([]byte(reference + "\x00" + runtime.GOARCH))
	return filepath.Join(d.cache, "headless", "images", fmt.Sprintf("%x.json", sum[:]))
}
func (d *headlessRuntimeDriver) cachedResolution(reference string) (headlessPrepared, bool) {
	resolved := reference
	if !strings.Contains(reference, "@sha256:") {
		data, err := os.ReadFile(d.resolutionPath(reference))
		if err != nil {
			return headlessPrepared{}, false
		}
		var saved struct {
			Reference string `json:"reference"`
			Resolved  string `json:"resolved"`
		}
		if json.Unmarshal(data, &saved) != nil || saved.Reference != reference {
			return headlessPrepared{}, false
		}
		resolved = saved.Resolved
	}
	if !strings.Contains(resolved, "@sha256:") {
		return headlessPrepared{}, false
	}
	if err := d.pullAPI.ValidateRegistryReference(resolved); err != nil {
		return headlessPrepared{}, false
	}
	name := pulledImageName(resolved, runtime.GOARCH)
	state, err := d.pullAPI.ImageState(name)
	if err != nil || state.Status != "downloaded" || state.ResolvedSource != resolved {
		return headlessPrepared{}, false
	}
	_, digest, _ := strings.Cut(resolved, "@")
	return headlessPrepared{name: name, image: headlessImage{ID: headlessID("img_"), Reference: reference, Digest: digest, Platform: "linux/" + runtime.GOARCH, CacheHit: true}}, true
}

func (d *headlessRuntimeDriver) Pull(ctx context.Context, req headlessPullRequest, report func(headlessTransfer)) (headlessImage, error) {
	if err := d.pullAPI.ValidateRegistryReference(req.Reference); err != nil {
		return headlessImage{}, headlessFailure("invalid_request", "Invalid OCI reference")
	}
	select {
	case d.pullGate <- struct{}{}:
		defer func() { <-d.pullGate }()
	case <-ctx.Done():
		return headlessImage{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var imageResult headlessImage
	var localName string
	var imageErr, kernelErr error
	var kernel headlessKernel
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		zero := int64(0)
		kernelCached := d.pullAPI.KernelStatus().Status == "downloaded"
		seen := map[string]bool{}
		state, err := d.pullAPI.PrepareKernel(ctx, func(e client.ProgressEvent) {
			if e.PlanningComplete {
				report(headlessTransfer{Branch: "kernel", PlanningComplete: true})
			}
			if t := e.Transfer; t != nil {
				report(headlessTransfer{ID: "kernel:" + t.ID, Kind: t.Kind, Name: t.ID, Reference: t.ID, State: t.State, Completed: t.Completed, Total: headlessPtr(t.Total), NetworkBytes: t.NetworkBytes, Branch: "kernel"})
				seen["kernel:"+t.ID] = true
				return
			}
			if e.Artifact == "" {
				return
			}
			id := "kernel:" + e.Artifact
			seen[id] = true
			state := e.Status
			if state == "downloaded" {
				state = "preparing"
			}
			var total *int64
			if e.BytesTotal >= 0 {
				total = headlessPtr(e.BytesTotal)
			}
			kind := "kernel"
			if strings.Contains(e.Artifact, "dev") || strings.Contains(e.Artifact, "APKINDEX") {
				kind = "dependency"
			}
			report(headlessTransfer{ID: id, Kind: kind, Name: e.Artifact, Reference: e.Artifact, State: state, Completed: e.BytesDownloaded, Total: total, NetworkBytes: e.BytesDownloaded, Branch: "kernel"})
		})
		if err != nil {
			kernelErr = err
			cancel()
			return
		}
		if len(seen) == 0 {
			report(headlessTransfer{ID: "kernel:cached", Kind: "kernel", Name: "Linux kernel " + state.Version, Reference: state.Source, State: "cached", Total: &zero, Branch: "kernel"})
		}
		report(headlessTransfer{Branch: "kernel", PlanningComplete: true})
		kernel = headlessKernel{ID: "kernel_" + state.Version, Version: state.Version, Platform: req.Platform, CacheHit: kernelCached}
	}()
	go func() {
		defer wg.Done()
		d.mu.Lock()
		previousID := d.references[req.Reference]
		previous, exists := d.prepared[previousID]
		d.mu.Unlock()
		if !exists && req.Policy == "if_missing" {
			previous, exists = d.cachedResolution(req.Reference)
		}
		if exists && req.Policy == "if_missing" {
			if _, err := d.pullAPI.ImageState(previous.name); err == nil {
				imageResult = previous.image
				imageResult.CacheHit = true
				localName = previous.name
				report(headlessTransfer{ID: "image:cached", Kind: "image_manifest", Name: req.Reference, Reference: req.Reference, State: "cached", Total: headlessPtr(int64(0)), Branch: "image", PlanningComplete: true})
				return
			}
		}
		report(headlessTransfer{ID: "image:resolve", Kind: "image_manifest", Name: req.Reference, Reference: req.Reference, State: "resolving", Branch: "image"})
		cacheHit := false
		emitImage := func(e client.ProgressEvent) {
			if e.Status == "available" || e.Status == "restored" {
				cacheHit = true
			}
			if t := e.Transfer; t != nil {
				var total *int64
				if t.Total >= 0 {
					total = headlessPtr(t.Total)
				}
				digest := ""
				if strings.HasPrefix(t.ID, "sha256:") {
					digest = t.ID
				}
				label := t.ID
				if t.Kind == "image_layer" {
					label = "Neurodesktop layer " + t.ID
				}
				report(headlessTransfer{ID: "image:" + t.ID, Kind: t.Kind, Name: label, Reference: req.Reference, Digest: digest, State: t.State, Completed: t.Completed, Total: total, NetworkBytes: t.NetworkBytes, Branch: "image"})
			}
			if e.PlanningComplete {
				report(headlessTransfer{Branch: "image", PlanningComplete: true})
			}
		}
		plan, err := d.pullAPI.PlanImagePullWithProgressContext(ctx, pulledImageName(req.Reference, runtime.GOARCH), client.PullImageRequest{Source: req.Reference, Architecture: runtime.GOARCH}, emitImage)
		if err != nil {
			imageErr = err
			cancel()
			return
		}
		if free, err := hostFreeBytes(d.cache); err == nil && free < estimatedDiskRequirement(plan, false) {
			imageErr = headlessFailure("insufficient_disk", "Insufficient disk space to prepare this image")
			cancel()
			return
		}
		// Resolve once, then pull the immutable digest. Tag refreshes can never replace
		// the filesystem currently used by a running VM.
		resolved := plan.ResolvedSource
		if resolved == "" {
			imageErr = fmt.Errorf("registry returned no immutable digest")
			cancel()
			return
		}
		localName = pulledImageName(resolved, runtime.GOARCH)
		state, stateErr := d.pullAPI.ImageState(localName)
		cacheHit = stateErr == nil && state.Status == "downloaded"
		report(headlessTransfer{ID: "image:resolve", Kind: "image_manifest", Name: req.Reference, Reference: resolved, State: "ready", Total: headlessPtr(int64(0)), Branch: "image"})
		// Full layer transfer retains truthful source-byte accounting; cached indexed
		// or enhanced layers are reused by the existing shared OCI store.
		err = d.pullAPI.PullImageStreamContext(ctx, localName, client.PullImageRequest{Source: resolved, Architecture: runtime.GOARCH}, func(e client.ProgressEvent) error {
			emitImage(e)
			return ctx.Err()
		})
		if err != nil {
			imageErr = err
			cancel()
			return
		}
		if cacheHit {
			report(headlessTransfer{ID: "image:cached", Kind: "image_layer", Name: req.Reference, Reference: resolved, State: "cached", Total: headlessPtr(int64(0)), Branch: "image"})
		}
		report(headlessTransfer{Branch: "image", PlanningComplete: true})
		_, digest, _ := strings.Cut(resolved, "@")
		imageResult = headlessImage{ID: headlessID("img_"), Reference: req.Reference, Digest: digest, Platform: req.Platform, CacheHit: cacheHit}
	}()
	wg.Wait()
	if kernelErr != nil && kernelErr != context.Canceled {
		e := asHeadlessError(kernelErr, "kernel_pull_failed")
		e.Details["branch"] = "kernel"
		return headlessImage{}, e
	}
	if imageErr != nil {
		e := asHeadlessError(imageErr, "image_pull_failed")
		e.Details["branch"] = "image"
		return headlessImage{}, e
	}
	if kernelErr != nil {
		return headlessImage{}, kernelErr
	}
	if data, err := json.Marshal(map[string]string{"reference": req.Reference, "resolved": strings.Split(req.Reference, "@")[0] + "@" + imageResult.Digest}); err == nil {
		// Derive the authoritative reference from the installed image; Docker Hub
		// normalization may differ from the reference supplied by the caller.
		if state, err := d.pullAPI.ImageState(localName); err == nil {
			data, _ = json.Marshal(map[string]string{"reference": req.Reference, "resolved": state.ResolvedSource})
		}
		if err := writeFileAtomically(d.resolutionPath(req.Reference), data, 0600); err != nil {
			return headlessImage{}, err
		}
	}
	imageResult.Kernel = kernel
	d.mu.Lock()
	d.prepared[imageResult.ID] = headlessPrepared{imageResult, localName}
	d.references[req.Reference] = imageResult.ID
	d.mu.Unlock()
	return imageResult, nil
}
func (d *headlessRuntimeDriver) resolveVMConfig(req *headlessVMRequest) error {
	_, home, err := configuredPersistentHomeMount(d.cache, req.Name, req.Home.ID, req.Home.Mode == "ephemeral")
	if err == nil {
		req.Home.ID = home
	}
	return err
}

func (d *headlessRuntimeDriver) Start(ctx context.Context, id string, req headlessVMRequest) error {
	if !d.Check(ctx).Accessible {
		return headlessFailure("virt_unavailable", "Virtualization access is unavailable")
	}
	d.mu.Lock()
	prepared, ok := d.prepared[req.ImageID]
	d.mu.Unlock()
	if !ok {
		return headlessFailure("image_not_prepared", "Prepared image is unavailable")
	}
	if !req.Storage.Create {
		info, err := os.Stat(req.Storage.HostPath)
		if err != nil || !info.IsDir() {
			return headlessFailure("invalid_request", "Storage directory does not exist")
		}
	}
	share, err := createStorageShare(req.Storage.HostPath)
	if err != nil {
		return err
	}
	shares := []client.ShareMount{share}
	for _, v := range req.Shares {
		info, err := os.Stat(v.HostPath)
		if err != nil || !info.IsDir() {
			return headlessFailure("invalid_request", "Additional directory does not exist")
		}
		shares = append(shares, client.ShareMount{Source: v.HostPath, Mount: v.GuestPath, Writable: !v.ReadOnly, MapOwner: true, OwnerUID: 1000, OwnerGID: 1000, Cache: "strict"})
	}
	homes, _, err := configuredPersistentHomeMount(d.cache, req.Name, req.Home.ID, req.Home.Mode == "ephemeral")
	if err != nil {
		return err
	}
	opts := appruntime.Options{CacheDir: d.cache}
	if req.CVMFS.Enabled {
		c := d.config.CVMFSHostMount
		mirror := req.CVMFS.Mirror
		if mirror == "auto" {
			mirror = c.Mirror
		}
		opts.CVMFSMounts = []appruntime.CVMFSHostMount{{Mount: c.Mount, Mirror: mirror, Mirrors: c.Mirrors, Repo: c.Repo, Path: c.Path, CacheLimitBytes: req.CVMFS.CacheLimit}}
	}
	api, err := appruntime.New(opts)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.machines[id] = api
	d.mu.Unlock()
	if req.CVMFS.Enabled && req.CVMFS.Mirror == "auto" {
		c := d.config.CVMFSHostMount
		probe, err := api.ProbeCVMFSMirrorsContext(ctx, client.CVMFSMirrorProbeRequest{Repo: c.Repo})
		if err != nil {
			return err
		}
		if err := api.SelectCVMFSMirrorContext(ctx, client.CVMFSMirrorSelectionRequest{Repo: c.Repo, Mirror: probe.SelectedMirror}); err != nil {
			return err
		}
	}
	env := []string{"CCX3_HEADLESS=1"}
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+req.Env[k])
	}
	request := client.CreateInstanceRequest{Image: prepared.name, DefaultUser: req.User, InitSystem: "systemd", Shares: shares, PersistentMounts: homes, Network: &client.NetworkConfig{Enabled: req.Network.Enabled, AllowInternet: req.Network.Internet}, Display: &client.DisplayConfig{Width: uint32(req.Display.Width), Height: uint32(req.Display.Height)}, KernelModules: appKernelModules(runtimeArchitecture()), Env: env, MemoryMB: req.Memory, CPUs: req.CPUs, AMD64Emulation: d.config.AMD64Emulation, TimeoutSeconds: float64(req.BootTimeout)}
	_, err = api.CreatePreparedInstance(ctx, id, request, prepared.image.Kernel.Version)
	return err
}
func (d *headlessRuntimeDriver) machine(id string) *appruntime.Runtime {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.machines[id]
}
func (d *headlessRuntimeDriver) Glass(ctx context.Context, id string, req headlessGlassRequest, ready func(int, int)) error {
	api := d.machine(id)
	if api == nil {
		return headlessFailure("invalid_state", "VM is unavailable")
	}
	response, err := api.RunInContext(ctx, id, client.RunRequest{Command: []string{"systemctl", "start", "neurodesktop-glass.service"}, User: "root"})
	if err != nil {
		return err
	}
	if response.ExitCode != 0 {
		return headlessFailure("desktop_start_failed", "Guest desktop service could not start")
	}
	if err := waitForDesktop(ctx, api, id); err != nil {
		return err
	}
	native := headlessNativeRequest{ctx: ctx, api: api, id: id, request: req, ready: ready, done: make(chan error, 1)}
	select {
	case d.native <- native:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Await window teardown even after cancellation; VM storage/display must not
	// be freed while the main thread still owns a frame lease.
	return <-native.done
}
func (d *headlessRuntimeDriver) Status(ctx context.Context, id string) (string, error) {
	api := d.machine(id)
	if api == nil {
		return "stopped", nil
	}
	state, err := api.InstanceStatusOfContext(ctx, id)
	return state.Status, err
}
func (d *headlessRuntimeDriver) Stop(ctx context.Context, id string) error {
	api := d.machine(id)
	if api == nil {
		return nil
	}
	state, err := api.InstanceStatusOfContext(ctx, id)
	if err != nil {
		return err
	}
	if state.Status == "running" {
		// The command transport can close as the guest powers off. Observe actual
		// guest exit, rather than interpreting a transport EOF as shutdown success.
		fmt.Fprintln(os.Stderr, "NeurodeskAppX: requesting guest poweroff")
		response, commandErr := api.RunInContext(ctx, id, client.RunRequest{Command: []string{"systemctl", "start", "poweroff.target", "--no-block"}, User: "root"})
		if commandErr != nil {
			fmt.Fprintln(os.Stderr, "NeurodeskAppX: guest poweroff control connection closed; waiting for exit")
		} else if response.ExitCode != 0 {
			return headlessFailure("shutdown_failed", "Guest rejected the poweroff request")
		}

		fmt.Fprintln(os.Stderr, "NeurodeskAppX: guest accepted poweroff; waiting for hypervisor exit")
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			state, err = api.InstanceStatusOfContext(ctx, id)
			if err != nil {
				return err
			}
			if state.Status != "running" {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
	fmt.Fprintln(os.Stderr, "NeurodeskAppX: guest exited; releasing runtime resources")
	if err := api.ShutdownContext(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.machines, id)
	d.mu.Unlock()
	return nil
}
func (d *headlessRuntimeDriver) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	ids := make([]string, 0, len(d.machines))
	for id := range d.machines {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		if err := d.Stop(ctx, id); err != nil {
			return err
		}
	}
	return d.pullAPI.ShutdownContext(ctx)
}

// releaseOwned is the bounded last resort after orphan cleanup fails. It only
// touches runtimes created by this child, and never claims guest-graceful success.
func (d *headlessRuntimeDriver) releaseOwned(ctx context.Context) error {
	d.mu.Lock()
	machines := make([]*appruntime.Runtime, 0, len(d.machines))
	for _, api := range d.machines {
		machines = append(machines, api)
	}
	d.mu.Unlock()
	var result error
	for _, api := range machines {
		result = errors.Join(result, api.ShutdownContext(ctx))
	}
	return result
}

func headlessStoragePath(value string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	return filepath.Abs(value)
}
