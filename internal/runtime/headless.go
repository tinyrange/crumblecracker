package runtime

import (
	"context"
	"fmt"

	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/display"
	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

// PrepareKernel exposes kernel preparation independently of VM creation. The
// caller serializes preparations and can run this concurrently with image pull.
func (s *Runtime) PrepareKernel(ctx context.Context, report func(client.ProgressEvent)) (client.KernelState, error) {
	if err := s.kernel.EnsureWithProgress(ctx, report); err != nil {
		return client.KernelState{}, err
	}
	if _, err := s.kernel.ReadKernel(); err != nil {
		return client.KernelState{}, err
	}
	if _, err := s.kernel.PackagePath(); err != nil {
		return client.KernelState{}, err
	}
	return s.kernel.Status(), nil
}
func (s *Runtime) KernelStatus() client.KernelState                  { return s.kernel.Status() }
func (s *Runtime) ImageState(name string) (client.ImageState, error) { return s.images.Get(name) }
func (s *Runtime) DisplaySession(id string) (display.Session, bool)  { return s.vms.Display(id) }
func (s *Runtime) ValidateRegistryReference(reference string) error {
	_, _, _, err := oci.ParseImageRef(reference)
	return err
}

// CreatePreparedInstance never downloads a kernel. A missing or changed prepared
// kernel is a pull prerequisite failure, not an invisible boot download.
func (s *Runtime) CreatePreparedInstance(ctx context.Context, id string, req client.CreateInstanceRequest, kernelVersion string) (client.InstanceState, error) {
	status := s.kernel.Status()
	if status.Status != "downloaded" || status.Version != kernelVersion {
		return client.InstanceState{}, fmt.Errorf("prepared kernel is unavailable")
	}
	if _, err := s.images.Get(req.Image); err != nil {
		return client.InstanceState{}, err
	}
	state, err := s.vms.StartInstanceStream(ctx, id, req, nil)
	return state, err
}

func (s *Runtime) PlanImagePullWithProgressContext(ctx context.Context, name string, req client.PullImageRequest, report func(client.ProgressEvent)) (client.ImagePullPlan, error) {
	source, err := req.SourceString()
	if err != nil {
		return client.ImagePullPlan{}, err
	}
	return s.images.PlanPull(ctx, name, source, oci.PullOptions{Architecture: req.Architecture, Report: report})
}
