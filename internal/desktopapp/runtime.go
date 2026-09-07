package desktopapp

import (
	"context"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

// desktopRuntime is the set of runtime operations used by the desktop products.
type desktopRuntime interface {
	VMSupportedContext(context.Context) (client.VMSupportedResponse, error)
	PlanImagePullContext(context.Context, string, client.PullImageRequest) (client.ImagePullPlan, error)
	PullImageStreamContext(context.Context, string, client.PullImageRequest, func(client.ProgressEvent) error) error
	ActivateStagedImageContext(context.Context, string, string) error
	CreateInstanceStreamWithIDContext(context.Context, string, client.CreateInstanceRequest, func(client.BootEvent) error) (client.InstanceState, error)
	ShutdownInstanceWithIDContext(context.Context, string) error
	InstanceStatusOfContext(context.Context, string) (client.InstanceState, error)
	RunStreamInContext(context.Context, string, client.RunRequest, func(client.ExecEvent) error) error
	RunInContext(context.Context, string, client.RunRequest) (client.ExecResponse, error)
	CVMFSStatusContext(context.Context) (client.CVMFSStatusResponse, error)
	ProbeCVMFSMirrorsContext(context.Context, client.CVMFSMirrorProbeRequest) (client.CVMFSMirrorProbeResponse, error)
	SelectCVMFSMirrorContext(context.Context, client.CVMFSMirrorSelectionRequest) error
}
