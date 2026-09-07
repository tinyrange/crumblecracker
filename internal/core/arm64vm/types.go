package arm64vm

import "github.com/tinyrange/crumblecracker/internal/core/vmruntime"

// DirectoryShare describes a host directory exposed inside the guest.
type DirectoryShare = vmruntime.DirectoryShare

// RunRequest is the backend-neutral request shape for the managed arm64 guest runtime.
type RunRequest = vmruntime.RunRequest

// RunResult is the backend-neutral result shape for one-shot guest execution.
type RunResult = vmruntime.RunResult
