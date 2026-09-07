// Package runtime owns the desktop application's in-process VM and image store.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vm"
	"github.com/tinyrange/crumblecracker/internal/display"
	"github.com/tinyrange/crumblecracker/internal/protocol"
	"path/filepath"
	"sync"
	"time"
)

type Options struct {
	CacheDir         string
	OpenGLShareGroup func() (context, pixelFormat uintptr)
	OnDisplay        func(string, display.Session)
	CVMFSMounts      []CVMFSHostMount
}
type Runtime struct {
	kernel       *alpine.Manager
	images       *oci.Store
	vms          *vm.Manager
	onDisplay    func(string, display.Session)
	cvmfsMonitor *cvmfsMonitor
	cvmfsMounts  []preparedCVMFSHostMount
}

func New(opts Options) (*Runtime, error) {
	if opts.CacheDir == "" {
		return nil, fmt.Errorf("runtime cache directory is required")
	}
	cache, err := filepath.Abs(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	s := &Runtime{kernel: alpine.NewManager(filepath.Join(cache, "runtime", "kernel")), images: oci.NewStoreWithSharedCache(filepath.Join(cache, "images"), filepath.Join(cache, "oci")), onDisplay: opts.OnDisplay}
	if len(opts.CVMFSMounts) > 0 {
		limit, err := cvmfsHostCacheLimit(opts.CVMFSMounts)
		if err != nil {
			return nil, err
		}
		root := filepath.Join(cache, "_cvmfs_cache")
		s.cvmfsMonitor = newCVMFSMonitor(root, limit)
		s.cvmfsMounts, err = prepareCVMFSHostMounts(opts.CVMFSMounts, root, s.cvmfsMonitor)
		if err != nil {
			return nil, err
		}
	}
	s.vms = vm.NewRuntimeManager(s.kernel, s.images, filepath.Join(cache, "runtime", "guestinit"), opts.OpenGLShareGroup)
	s.vms.SetHostMountProvider(func(context.Context, string) ([]virtio.ShareMount, error) {
		mounts := make([]virtio.ShareMount, 0, len(s.cvmfsMounts))
		for _, m := range s.cvmfsMounts {
			mounts = append(mounts, virtio.ShareMount{GuestPath: m.guestPath, Backend: virtio.NewImageFS(m.root, ""), Writable: false, CacheMode: "aggressive"})
		}
		return mounts, nil
	})
	return s, nil
}
func (s *Runtime) ShutdownContext(ctx context.Context) error { return s.vms.ShutdownAll(ctx) }
func (s *Runtime) ShutdownInstanceWithIDContext(ctx context.Context, id string) error {
	return s.vms.ShutdownInstance(ctx, id)
}
func (s *Runtime) InstanceStatusOfContext(ctx context.Context, id string) (client.InstanceState, error) {
	if err := ctx.Err(); err != nil {
		return client.InstanceState{}, err
	}
	return s.vms.StatusOf(id), nil
}
func (s *Runtime) VMSupportedContext(ctx context.Context) (client.VMSupportedResponse, error) {
	if err := ctx.Err(); err != nil {
		return client.VMSupportedResponse{}, err
	}
	err := vm.Supports()
	r := client.VMSupportedResponse{Supported: err == nil}
	if err != nil {
		r.Error = err.Error()
	}
	return r, nil
}
func (s *Runtime) PlanImagePullContext(ctx context.Context, name string, req client.PullImageRequest) (client.ImagePullPlan, error) {
	source, err := req.SourceString()
	if err != nil {
		return client.ImagePullPlan{}, err
	}
	return s.images.PlanPull(ctx, name, source, oci.PullOptions{Architecture: req.Architecture, KeepCompressedLayers: req.KeepCompressed})
}
func (s *Runtime) PullImageStreamContext(ctx context.Context, name string, req client.PullImageRequest, onEvent func(client.ProgressEvent) error) error {
	source, err := req.SourceString()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var callbackErr error
	report := func(event client.ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		if callbackErr == nil && onEvent != nil {
			callbackErr = onEvent(event)
			if callbackErr != nil {
				cancel()
			}
		}
	}
	_, err = s.images.Pull(ctx, name, source, oci.PullOptions{Architecture: req.Architecture, Prefetch: req.Prefetch, PrefetchWorkers: req.PrefetchWorkers, Refresh: req.Refresh, KeepCompressedLayers: req.KeepCompressed, Report: report})
	mu.Lock()
	defer mu.Unlock()
	return errors.Join(callbackErr, err)
}
func (s *Runtime) ActivateStagedImageContext(ctx context.Context, name, staged string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.images.ActivateStaged(name, staged)
	return err
}
func (s *Runtime) CreateInstanceStreamWithIDContext(ctx context.Context, id string, req client.CreateInstanceRequest, onEvent func(client.BootEvent) error) (client.InstanceState, error) {
	timeout := time.Duration(req.TimeoutSeconds * float64(time.Second))
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := s.images.Get(req.Image); err != nil {
		return client.InstanceState{}, err
	}
	if err := vm.Supports(); err != nil {
		return client.InstanceState{}, err
	}
	if err := s.kernel.Ensure(ctx); err != nil {
		return client.InstanceState{}, err
	}
	state, err := s.vms.StartInstanceStream(ctx, id, req, onEvent)
	if err == nil && s.onDisplay != nil {
		if session, ok := s.vms.Display(state.ID); ok {
			s.onDisplay(state.ID, session)
		}
	}
	return state, err
}
func (s *Runtime) RunStreamInContext(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
	ctx, cancel := runContext(ctx, req)
	defer cancel()
	return s.vms.RunStreamIn(ctx, id, req, nil, onEvent)
}
func (s *Runtime) RunInContext(ctx context.Context, id string, req client.RunRequest) (client.ExecResponse, error) {
	ctx, cancel := runContext(ctx, req)
	defer cancel()
	return s.vms.RunIn(ctx, id, req)
}

func runContext(ctx context.Context, req client.RunRequest) (context.Context, context.CancelFunc) {
	if req.TimeoutSeconds > 0 {
		return context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds*float64(time.Second)))
	}
	return context.WithCancel(ctx)
}
