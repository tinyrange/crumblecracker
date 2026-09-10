package desktopapp

import (
	"context"
	"errors"
	"fmt"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// These DTOs are the public v1 contract, independent of the embedded runtime.
type headlessError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details"`
	Retryable bool           `json:"retryable"`
}

func (e *headlessError) Error() string { return e.Message }
func headlessFailure(code, message string) *headlessError {
	return &headlessError{Code: code, Message: message, Details: map[string]any{}, Retryable: code == "timeout" || code == "shutdown_timeout" || code == "image_pull_failed" || code == "kernel_pull_failed" || code == "insufficient_disk" || code == "resource_in_use"}
}

type headlessPullRequest struct {
	Reference string `json:"reference"`
	Platform  string `json:"platform"`
	Policy    string `json:"policy"`
	Timeout   int    `json:"timeout_seconds"`
}
type headlessImage struct {
	ID        string         `json:"image_id"`
	Reference string         `json:"reference"`
	Digest    string         `json:"digest"`
	Platform  string         `json:"platform"`
	CacheHit  bool           `json:"cache_hit"`
	Kernel    headlessKernel `json:"kernel"`
}
type headlessKernel struct {
	ID       string `json:"kernel_id"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	CacheHit bool   `json:"cache_hit"`
}
type headlessHome struct {
	Mode string `json:"mode"`
	ID   string `json:"id,omitempty"`
}
type headlessStorage struct {
	HostPath string `json:"host_path"`
	Create   bool   `json:"create"`
}
type headlessShare struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only"`
}
type headlessNetwork struct {
	Enabled  bool `json:"enabled"`
	Internet bool `json:"allow_internet"`
}
type headlessDisplay struct {
	Width  int  `json:"width"`
	Height int  `json:"height"`
	GPU    bool `json:"gpu_acceleration"`
}
type headlessCVMFS struct {
	Enabled    bool   `json:"enabled"`
	Mirror     string `json:"mirror,omitempty"`
	CacheLimit int64  `json:"cache_limit_bytes,omitempty"`
}
type headlessVMRequest struct {
	ImageID     string            `json:"image_id"`
	Name        string            `json:"name"`
	Memory      uint64            `json:"memory_mib"`
	CPUs        int               `json:"cpus"`
	User        string            `json:"user"`
	Home        headlessHome      `json:"home"`
	Storage     headlessStorage   `json:"storage"`
	Shares      []headlessShare   `json:"shares"`
	Network     headlessNetwork   `json:"network"`
	Display     headlessDisplay   `json:"display"`
	CVMFS       headlessCVMFS     `json:"cvmfs"`
	BootTimeout int               `json:"boot_timeout_seconds"`
	Env         map[string]string `json:"env"`
}
type headlessGlassRequest struct {
	Title   string `json:"title"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Timeout int    `json:"timeout_seconds"`
}
type headlessGlass struct {
	ID     string `json:"glass_id"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
type headlessVM struct {
	ID            string            `json:"vm_id"`
	Name          string            `json:"name"`
	ImageID       string            `json:"image_id"`
	State         string            `json:"state"`
	Config        headlessVMRequest `json:"config"`
	Glass         *headlessGlass    `json:"glass"`
	Active        *string           `json:"active_operation_id"`
	LastError     *headlessError    `json:"last_error"`
	ownsResources bool
	cancel        context.CancelFunc
	done          chan struct{}
}
type headlessVirt struct {
	Supported  bool                `json:"supported"`
	Accessible bool                `json:"accessible"`
	Backend    string              `json:"backend"`
	Reason     *headlessVirtReason `json:"reason"`
}
type headlessVirtReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type headlessOperation struct {
	ID       string            `json:"operation_id"`
	Kind     string            `json:"kind"`
	Resource *string           `json:"resource_id"`
	State    string            `json:"state"`
	Phase    string            `json:"phase"`
	Progress *headlessProgress `json:"progress"`
	Created  time.Time         `json:"created_at"`
	Finished *time.Time        `json:"finished_at"`
	Result   any               `json:"result"`
	Error    *headlessError    `json:"error"`
}
type headlessAcceptance struct {
	ID       string  `json:"operation_id"`
	Kind     string  `json:"kind"`
	State    string  `json:"state"`
	Resource *string `json:"resource_id"`
}
type headlessDriver interface {
	Check(context.Context) headlessVirt
	Pull(context.Context, headlessPullRequest, func(headlessTransfer)) (headlessImage, error)
	Start(context.Context, string, headlessVMRequest) error
	Glass(context.Context, string, headlessGlassRequest, func(int, int)) error
	Stop(context.Context, string) error
	Status(context.Context, string) (string, error)
	Shutdown(context.Context) error
}

var headlessName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var headlessEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var headlessUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func headlessDefaults(config Config, storage string) headlessVMRequest {
	return headlessVMRequest{Name: config.DefaultVMName, Memory: config.DefaultMemoryMB, CPUs: config.DefaultCPUs, User: config.DefaultUser,
		Home: headlessHome{Mode: "persistent"}, Storage: headlessStorage{HostPath: storage, Create: true}, Shares: []headlessShare{}, Network: headlessNetwork{true, true},
		Display: headlessDisplay{1440, 900, false}, CVMFS: headlessCVMFS{Enabled: config.CVMFSHostMount != nil}, BootTimeout: 600, Env: map[string]string{}}
}
func (r *headlessVMRequest) validate(config Config) error {
	invalid := func(field string) error {
		e := headlessFailure("invalid_request", "Invalid "+field)
		e.Details["field"] = field
		return e
	}
	if r.ImageID == "" {
		return invalid("image_id")
	}
	if !headlessName.MatchString(r.Name) {
		return invalid("name")
	}
	if r.Memory == 0 || r.CPUs <= 0 {
		return invalid("memory_mib/cpus")
	}
	if r.CPUs > runtime.NumCPU() || (hostMemoryMB() > 0 && r.Memory > hostMemoryMB()) {
		e := headlessFailure("unsupported_configuration", "Requested resources exceed host capacity")
		e.Details["cpu_limit"] = runtime.NumCPU()
		e.Details["memory_mib_limit"] = hostMemoryMB()
		return e
	}
	if !headlessEnvName.MatchString(r.User) {
		return invalid("user")
	}
	if r.Home.Mode != "persistent" && r.Home.Mode != "ephemeral" {
		return invalid("home.mode")
	}
	if r.Home.Mode == "ephemeral" && r.Home.ID != "" {
		return invalid("home.id")
	}
	if r.Home.ID != "" && !headlessName.MatchString(r.Home.ID) {
		return invalid("home.id")
	}
	if !filepath.IsAbs(r.Storage.HostPath) {
		return invalid("storage.host_path")
	}
	r.Storage.HostPath = filepath.Clean(r.Storage.HostPath)
	if !r.Network.Enabled && r.Network.Internet {
		return invalid("network.allow_internet")
	}
	if !headlessSize(r.Display.Width, r.Display.Height) {
		return invalid("display")
	}
	// The existing accelerated renderer needs a presentation context during boot.
	// Advertise this limitation rather than creating a hidden window.
	if r.Display.GPU {
		return headlessFailure("unsupported_configuration", "GPU acceleration is unavailable for windowless boot")
	}
	if r.BootTimeout < 1 || r.BootTimeout > 3600 {
		return invalid("boot_timeout_seconds")
	}
	for name, value := range r.Env {
		if !headlessEnvName.MatchString(name) || strings.ContainsRune(value, 0) || strings.HasPrefix(name, "CCX3_") || strings.HasPrefix(name, "VMSH_") {
			return invalid("env." + name)
		}
	}
	if r.CVMFS.Enabled {
		if config.CVMFSHostMount == nil || !r.Network.Enabled {
			return headlessFailure("unsupported_configuration", "Host CVMFS requires product support and networking")
		}
		if r.CVMFS.Mirror == "" {
			r.CVMFS.Mirror = "auto"
		}
		if r.CVMFS.CacheLimit == 0 {
			r.CVMFS.CacheLimit = config.CVMFSHostMount.CacheLimitBytes
		}
		if r.CVMFS.CacheLimit < 1 {
			return invalid("cvmfs.cache_limit_bytes")
		}
		valid := r.CVMFS.Mirror == "auto"
		for _, m := range config.CVMFSHostMount.Mirrors {
			valid = valid || m == r.CVMFS.Mirror
		}
		if !valid {
			return invalid("cvmfs.mirror")
		}
		for key, value := range map[string]string{"CVMFS_DISABLE": "true", "NEURODESKTOP_CVMFS_STARTUP_MODE": "external"} {
			if supplied, ok := r.Env[key]; ok && supplied != value {
				return headlessFailure("unsupported_configuration", "env."+key+" conflicts with host-managed CVMFS")
			}
			r.Env[key] = value
		}
	} else {
		if r.CVMFS.Mirror != "" || r.CVMFS.CacheLimit != 0 {
			return invalid("cvmfs")
		}
		if r.Env["NEURODESKTOP_CVMFS_STARTUP_MODE"] == "eager" && !r.Network.Enabled {
			return headlessFailure("unsupported_configuration", "Guest CVMFS requires networking")
		}
	}
	reserved := []string{"/proc", "/sys", "/dev", "/run", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot", config.GuestStorageMount, "/cvmfs"}
	if r.Home.Mode == "persistent" {
		reserved = append(reserved, "/home")
	}
	var destinations []string
	for i := range r.Shares {
		share := &r.Shares[i]
		if !filepath.IsAbs(share.HostPath) || !path.IsAbs(share.GuestPath) || path.Clean(share.GuestPath) != share.GuestPath || strings.ContainsRune(share.GuestPath, 0) {
			return invalid("shares")
		}
		// A child mount beneath /home is supported; hiding the home itself is not.
		for _, root := range reserved {
			if root == "/home" {
				if withinGuest(root, share.GuestPath) {
					return invalid("shares.guest_path")
				}
				continue
			}
			if withinGuest(root, share.GuestPath) || withinGuest(share.GuestPath, root) {
				return invalid("shares.guest_path")
			}
		}
		for _, other := range destinations {
			if withinGuest(other, share.GuestPath) || withinGuest(share.GuestPath, other) {
				return invalid("shares.guest_path")
			}
		}
		destinations = append(destinations, share.GuestPath)
		share.HostPath = filepath.Clean(share.HostPath)
	}
	return nil
}
func withinGuest(value, root string) bool {
	return value == root || strings.HasPrefix(value, strings.TrimSuffix(root, "/")+"/")
}
func headlessSize(w, h int) bool { return w >= 320 && h >= 320 && w <= 8192 && h <= 8192 }
func asHeadlessError(err error, code string) *headlessError {
	var e *headlessError
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.Canceled) {
		return headlessFailure("operation_cancelled", "Operation cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return headlessFailure("timeout", "Operation timed out")
	}
	if errors.Is(err, virtio.ErrPersistentStoreInUse) {
		return headlessFailure("resource_in_use", "Persistent home is already used by another VM")
	}
	if errors.Is(err, syscall.ENOSPC) {
		return headlessFailure("insufficient_disk", "Insufficient disk space")
	}
	// Backend errors can contain registry URLs or guest environment values.
	return headlessFailure(code, fmt.Sprintf("%s", strings.ReplaceAll(code, "_", " ")))
}
