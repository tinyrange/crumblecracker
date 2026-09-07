package client

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type ServerHello struct {
	Addr      string `json:"addr,omitempty"`
	Scheme    string `json:"scheme,omitempty"`
	Kind      string `json:"kind,omitempty"`
	TokenPath string `json:"token_path,omitempty"`
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type WatchdogActivityState struct {
	CVMFS WatchdogActivityCounter `json:"cvmfs"`
}

type WatchdogActivityCounter struct {
	Events           uint64  `json:"events"`
	Bytes            int64   `json:"bytes"`
	LastActivityUnix int64   `json:"last_activity_unix,omitempty"`
	SecondsSinceLast float64 `json:"seconds_since_last,omitempty"`
}

type WatchdogLeaseRequest struct {
	LeaseID        string  `json:"lease_id,omitempty"`
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty"`
}

type WatchdogLeaseResponse struct {
	LeaseID        string  `json:"lease_id"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
}

type BootEvent struct {
	Kind    string        `json:"kind"`
	Message string        `json:"message,omitempty"`
	Data    string        `json:"data,omitempty"`
	Error   string        `json:"error,omitempty"`
	State   InstanceState `json:"state,omitempty"`
}

type KernelState struct {
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
}

type ImageMetadataState struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	SourceKind   string   `json:"source_kind,omitempty"`
	Architecture string   `json:"architecture,omitempty"`
	Env          []string `json:"env,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type EmulatorState struct {
	Status   string `json:"status"`
	Path     string `json:"path,omitempty"`
	Required bool   `json:"required"`
	Error    string `json:"error,omitempty"`
}

type DownloadRequest struct {
	Source string `json:"source,omitempty"`
}

type ProgressEvent struct {
	Status             string  `json:"status"`
	Artifact           string  `json:"artifact,omitempty"`
	Progress           float64 `json:"progress,omitempty"`
	DownloadProgress   float64 `json:"download_progress,omitempty"`
	IndexProgress      float64 `json:"index_progress,omitempty"`
	BytesDownloaded    int64   `json:"bytes_downloaded,omitempty"`
	BytesTotal         int64   `json:"bytes_total,omitempty"`
	FilesDownloaded    int64   `json:"files_downloaded,omitempty"`
	FilesTotal         int64   `json:"files_total,omitempty"`
	RateBytesPerSecond float64 `json:"rate_bytes_per_second,omitempty"`
	ETASeconds         float64 `json:"eta_seconds,omitempty"`
	Blob               string  `json:"blob,omitempty"`
	Error              string  `json:"error,omitempty"`
}

type ImageState struct {
	Name           string `json:"name"`
	Source         string `json:"source,omitempty"`
	ResolvedSource string `json:"resolved_source,omitempty"`
	SourceKind     string `json:"source_kind,omitempty"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
}

type ImagePullPlan struct {
	Name            string `json:"name"`
	Source          string `json:"source"`
	Architecture    string `json:"architecture,omitempty"`
	Installed       bool   `json:"installed"`
	Available       bool   `json:"available"`
	BytesTotal      int64  `json:"bytes_total,omitempty"`
	BytesCached     int64  `json:"bytes_cached,omitempty"`
	BytesToDownload int64  `json:"bytes_to_download,omitempty"`
	LayersTotal     int64  `json:"layers_total,omitempty"`
	LayersCached    int64  `json:"layers_cached,omitempty"`
	ResolvedSource  string `json:"resolved_source,omitempty"`
}

type SaveImageRequest struct {
	Name  string `json:"name"`
	Image string `json:"image,omitempty"`
}

type PullImageRequest struct {
	Source          string       `json:"-"`
	SourceRef       *ImageSource `json:"-"`
	Architecture    string       `json:"architecture,omitempty"`
	CacheDir        string       `json:"cache_dir,omitempty"`
	Prefetch        bool         `json:"prefetch,omitempty"`
	PrefetchWorkers int          `json:"prefetch_workers,omitempty"`
	Refresh         bool         `json:"refresh,omitempty"`
	KeepCompressed  bool         `json:"keep_compressed,omitempty"`
	ActivateFrom    string       `json:"activate_from,omitempty"`
}

type ImageSource struct {
	Type    string   `json:"type"`
	Format  string   `json:"format,omitempty"`
	Mirror  string   `json:"mirror,omitempty"`
	Mirrors []string `json:"mirrors,omitempty"`
	Repo    string   `json:"repo,omitempty"`
	Path    string   `json:"path,omitempty"`
}

type CVMFSListRequest struct {
	Mirror   string   `json:"mirror,omitempty"`
	Mirrors  []string `json:"mirrors,omitempty"`
	Repo     string   `json:"repo"`
	Path     string   `json:"path,omitempty"`
	CacheDir string   `json:"cache_dir,omitempty"`
}

type CVMFSDirectoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size,omitempty"`
}

type CVMFSListResponse struct {
	Entries []CVMFSDirectoryEntry `json:"entries"`
}

type CVMFSReadRequest struct {
	Mirror   string   `json:"mirror,omitempty"`
	Mirrors  []string `json:"mirrors,omitempty"`
	Repo     string   `json:"repo"`
	Path     string   `json:"path"`
	Offset   int64    `json:"offset,omitempty"`
	Length   int64    `json:"length,omitempty"`
	CacheDir string   `json:"cache_dir,omitempty"`
}

type CVMFSReadResponse struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Data   []byte `json:"data,omitempty"`
	EOF    bool   `json:"eof,omitempty"`
}

type CVMFSTransferState struct {
	ID         uint64  `json:"id"`
	Path       string  `json:"path"`
	Mirror     string  `json:"mirror,omitempty"`
	Bytes      int64   `json:"bytes"`
	TotalBytes int64   `json:"total_bytes,omitempty"`
	Progress   float64 `json:"progress,omitempty"`
	StartedAt  string  `json:"started_at,omitempty"`
}

type CVMFSStatusResponse struct {
	State           string               `json:"state"`
	SelectedMirror  string               `json:"selected_mirror,omitempty"`
	Bytes           int64                `json:"bytes"`
	TotalBytes      int64                `json:"total_bytes,omitempty"`
	Progress        float64              `json:"progress,omitempty"`
	CacheBytes      int64                `json:"cache_bytes,omitempty"`
	CacheLimitBytes int64                `json:"cache_limit_bytes,omitempty"`
	ActiveTransfers []CVMFSTransferState `json:"active_transfers"`
	LastError       string               `json:"last_error,omitempty"`
	LastErrorUnix   int64                `json:"last_error_unix,omitempty"`
}

type CVMFSMirrorProbeRequest struct {
	Repo string `json:"repo"`
}

type CVMFSMirrorProbeResult struct {
	Mirror                 string  `json:"mirror"`
	ManifestLatencyMillis  int64   `json:"manifest_latency_millis,omitempty"`
	RootCatalogMillis      int64   `json:"root_catalog_millis,omitempty"`
	RootCatalogBytes       int64   `json:"root_catalog_bytes,omitempty"`
	RootCatalogBytesPerSec float64 `json:"root_catalog_bytes_per_second,omitempty"`
	Error                  string  `json:"error,omitempty"`
}

type CVMFSMirrorProbeResponse struct {
	SelectedMirror string                   `json:"selected_mirror"`
	Results        []CVMFSMirrorProbeResult `json:"results"`
}

type CVMFSMirrorSelectionRequest struct {
	Repo   string `json:"repo"`
	Mirror string `json:"mirror"`
}

func (r PullImageRequest) MarshalJSON() ([]byte, error) {
	payload := map[string]any{}
	switch {
	case r.SourceRef != nil:
		payload["source"] = r.SourceRef
	case r.Source != "":
		payload["source"] = r.Source
	default:
		payload["source"] = ""
	}
	if r.CacheDir != "" {
		payload["cache_dir"] = r.CacheDir
	}
	if r.Architecture != "" {
		payload["architecture"] = r.Architecture
	}
	if r.Prefetch {
		payload["prefetch"] = true
	}
	if r.PrefetchWorkers > 0 {
		payload["prefetch_workers"] = r.PrefetchWorkers
	}
	if r.Refresh {
		payload["refresh"] = true
	}
	if r.KeepCompressed {
		payload["keep_compressed"] = true
	}
	if r.ActivateFrom != "" {
		payload["activate_from"] = r.ActivateFrom
	}
	return json.Marshal(payload)
}

func (r *PullImageRequest) UnmarshalJSON(data []byte) error {
	var raw struct {
		Source          json.RawMessage `json:"source"`
		Architecture    string          `json:"architecture,omitempty"`
		CacheDir        string          `json:"cache_dir,omitempty"`
		Prefetch        bool            `json:"prefetch,omitempty"`
		PrefetchWorkers int             `json:"prefetch_workers,omitempty"`
		Refresh         bool            `json:"refresh,omitempty"`
		KeepCompressed  bool            `json:"keep_compressed,omitempty"`
		ActivateFrom    string          `json:"activate_from,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Source = ""
	r.SourceRef = nil
	r.Architecture = raw.Architecture
	r.CacheDir = raw.CacheDir
	r.Prefetch = raw.Prefetch
	r.PrefetchWorkers = raw.PrefetchWorkers
	r.Refresh = raw.Refresh
	r.KeepCompressed = raw.KeepCompressed
	r.ActivateFrom = raw.ActivateFrom
	if len(raw.Source) == 0 || string(raw.Source) == "null" {
		return nil
	}
	var sourceString string
	if err := json.Unmarshal(raw.Source, &sourceString); err == nil {
		r.Source = sourceString
		return nil
	}
	var sourceRef ImageSource
	if err := json.Unmarshal(raw.Source, &sourceRef); err != nil {
		return fmt.Errorf("decode source: %w", err)
	}
	r.SourceRef = &sourceRef
	return nil
}

func (r PullImageRequest) SourceString() (string, error) {
	if strings.TrimSpace(r.Source) != "" {
		return r.Source, nil
	}
	if r.SourceRef == nil {
		return "", fmt.Errorf("image source is required")
	}
	switch strings.ToLower(strings.TrimSpace(r.SourceRef.Type)) {
	case "cvmfs":
		repo := strings.TrimSpace(r.SourceRef.Repo)
		if repo == "" {
			return "", fmt.Errorf("cvmfs repo is required")
		}
		pathValue := strings.TrimSpace(r.SourceRef.Path)
		if pathValue == "" {
			pathValue = "/"
		}
		mirror := strings.TrimRight(strings.TrimSpace(r.SourceRef.Mirror), "/")
		if mirror == "" {
			return fmt.Sprintf("cvmfs://%s%s", repo, ensureAbsolutePath(pathValue)), nil
		}
		mirror = ensureCVMFSMirrorPath(mirror)
		return fmt.Sprintf("%s/%s%s", mirror, repo, ensureAbsolutePath(pathValue)), nil
	case "simg":
		if strings.TrimSpace(r.SourceRef.Path) == "" {
			return "", fmt.Errorf("simg path is required")
		}
		return r.SourceRef.Path, nil
	case "rootfs-tar", "rootfs":
		if strings.TrimSpace(r.SourceRef.Path) == "" {
			return "", fmt.Errorf("rootfs tar path is required")
		}
		return "rootfs-tar:" + r.SourceRef.Path, nil
	case "oci":
		if strings.TrimSpace(r.SourceRef.Path) == "" {
			return "", fmt.Errorf("oci path is required")
		}
		return r.SourceRef.Path, nil
	case "docker-archive":
		if strings.TrimSpace(r.SourceRef.Path) == "" {
			return "", fmt.Errorf("docker archive path is required")
		}
		return "docker-archive:" + r.SourceRef.Path, nil
	default:
		return "", fmt.Errorf("unsupported source type %q", r.SourceRef.Type)
	}
}

func ensureAbsolutePath(value string) string {
	if value == "" {
		return "/"
	}
	if strings.HasPrefix(value, "/") {
		return value
	}
	return "/" + value
}

func ensureCVMFSMirrorPath(mirror string) string {
	mirror = strings.TrimRight(strings.TrimSpace(mirror), "/")
	u, err := url.Parse(mirror)
	if err != nil {
		if !strings.HasSuffix(mirror, "/cvmfs") {
			return mirror + "/cvmfs"
		}
		return mirror
	}
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/cvmfs") {
		u.Path = strings.TrimRight(u.Path, "/") + "/cvmfs"
	}
	return strings.TrimRight(u.String(), "/")
}

type ShareMount struct {
	Source   string `json:"source"`
	Mount    string `json:"mount"`
	Writable bool   `json:"writable,omitempty"`
	MapOwner bool   `json:"map_owner,omitempty"`
	OwnerUID uint32 `json:"owner_uid,omitempty"`
	OwnerGID uint32 `json:"owner_gid,omitempty"`
	Cache    string `json:"cache,omitempty"`
}

// PersistentMount attaches a named durable copy-on-write upper over an
// existing image directory. The daemon resolves Name beneath its configured
// store root; callers cannot select an arbitrary host path.
type PersistentMount struct {
	Name     string `json:"name"`
	Mount    string `json:"mount"`
	OwnerUID uint32 `json:"owner_uid,omitempty"`
	OwnerGID uint32 `json:"owner_gid,omitempty"`
	MapOwner bool   `json:"map_owner,omitempty"`
}

type PersistentMountState struct {
	Name               string `json:"name"`
	Mount              string `json:"mount"`
	FormatVersion      uint32 `json:"format_version"`
	LowerID            string `json:"lower_id,omitempty"`
	PreviousLowerID    string `json:"previous_lower_id,omitempty"`
	Sequence           uint64 `json:"sequence"`
	DurableSequence    uint64 `json:"durable_sequence"`
	UpperLogicalBytes  uint64 `json:"upper_logical_bytes,omitempty"`
	UpperDataBytes     uint64 `json:"upper_data_bytes,omitempty"`
	UpperPhysicalBytes uint64 `json:"upper_physical_bytes,omitempty"`
	WALBytes           uint64 `json:"wal_bytes,omitempty"`
	StagingBytes       uint64 `json:"staging_bytes,omitempty"`
	TrashBytes         uint64 `json:"trash_bytes,omitempty"`
	RecoveryStatus     string `json:"recovery_status,omitempty"`
	QuarantinePath     string `json:"quarantine_path,omitempty"`
	DiscardedBytes     uint64 `json:"discarded_bytes,omitempty"`
	LastCheckpoint     string `json:"last_checkpoint,omitempty"`
	LastError          string `json:"last_error,omitempty"`
	HostFreeBytes      uint64 `json:"host_free_bytes,omitempty"`
}

type NetworkConfig struct {
	Enabled                  bool          `json:"enabled,omitempty"`
	Mode                     string        `json:"mode,omitempty"`
	AllowInternet            bool          `json:"allow_internet,omitempty"`
	BlockHostAccess          bool          `json:"block_host_access,omitempty"`
	GuestIPv4                string        `json:"guest_ipv4,omitempty"`
	GuestMAC                 string        `json:"guest_mac,omitempty"`
	HostDNSName              string        `json:"host_dns_name,omitempty"`
	AllowedServiceProxyPorts []int         `json:"allowed_service_proxy_ports,omitempty"`
	PortForwards             []PortForward `json:"port_forwards,omitempty"`
	MaxForwardConnections    int           `json:"max_forward_connections,omitempty"`
}

type PortForward struct {
	Protocol       string `json:"protocol,omitempty"`
	HostAddr       string `json:"host_addr,omitempty"`
	HostPort       int    `json:"host_port,omitempty"`
	GuestAddr      string `json:"guest_addr,omitempty"`
	GuestPort      int    `json:"guest_port,omitempty"`
	MaxConnections int    `json:"max_connections,omitempty"`
}

type ServiceProxyPortRequest struct {
	Port int `json:"port"`
}

type VMSupportedResponse struct {
	Supported bool   `json:"supported"`
	Error     string `json:"error,omitempty"`
}

type CapabilitiesResponse struct {
	Host                   string   `json:"host"`
	Backend                string   `json:"backend,omitempty"`
	VMSupported            bool     `json:"vm_supported"`
	VMError                string   `json:"vm_error,omitempty"`
	MaxInstances           int      `json:"max_instances,omitempty"`
	SnapshotClasses        []string `json:"snapshot_classes,omitempty"`
	NetworkModes           []string `json:"network_modes,omitempty"`
	ShareConsistency       []string `json:"share_consistency,omitempty"`
	ResourceLimits         []string `json:"resource_limits,omitempty"`
	SupportsMultiImageExec bool     `json:"supports_multi_image_exec"`
	SupportsNestedVirt     bool     `json:"supports_nested_virtualization"`
	SupportsDisplay        bool     `json:"supports_display"`
	RequiresPrivilegedCCX3 bool     `json:"requires_privileged_ccx3"`
	Notes                  []string `json:"notes,omitempty"`
	MemoryCapacityMB       uint64   `json:"memory_capacity_mb,omitempty"`
	MemoryReservedMB       uint64   `json:"memory_reserved_mb,omitempty"`
	CPUCapacity            int      `json:"cpu_capacity,omitempty"`
	CPUReserved            int      `json:"cpu_reserved,omitempty"`
}

type DisplayConfig struct {
	Width         uint32 `json:"width,omitempty"`
	Height        uint32 `json:"height,omitempty"`
	VNCListen     string `json:"vnc_listen,omitempty"`
	VNCPassword   string `json:"vnc_password,omitempty"`
	Accelerated3D bool   `json:"accelerated_3d,omitempty"`
}

type DisplayState struct {
	Width      uint32 `json:"width"`
	Height     uint32 `json:"height"`
	VNCAddress string `json:"vnc_address,omitempty"`
}

type SharedMemoryConfig struct {
	Domain   string `json:"domain"`
	PhysAddr uint64 `json:"phys_addr"`
}

type CreateInstanceRequest struct {
	ID               string              `json:"id,omitempty"`
	Image            string              `json:"image"`
	DefaultUser      string              `json:"default_user,omitempty"`
	InitSystem       string              `json:"init,omitempty"`
	Kernel           string              `json:"kernel,omitempty"`
	Shares           []ShareMount        `json:"shares,omitempty"`
	PersistentMounts []PersistentMount   `json:"persistent_mounts,omitempty"`
	Network          *NetworkConfig      `json:"network,omitempty"`
	Display          *DisplayConfig      `json:"display,omitempty"`
	SharedMemory     *SharedMemoryConfig `json:"shared_memory,omitempty"`
	KernelModules    []string            `json:"kernel_modules,omitempty"`
	Env              []string            `json:"env,omitempty"`
	MemoryMB         uint64              `json:"memory_mb,omitempty"`
	BalloonMB        uint64              `json:"balloon_mb,omitempty"`
	CPUs             int                 `json:"cpus,omitempty"`
	NestedVirt       bool                `json:"nested_virtualization,omitempty"`
	AMD64Emulation   bool                `json:"amd64_emulation,omitempty"`
	Dmesg            bool                `json:"dmesg,omitempty"`
	SnapshotDir      string              `json:"snapshot_dir,omitempty"`
	RestoreSnapshot  string              `json:"restore_snapshot,omitempty"`
	TimeoutSeconds   float64             `json:"timeout_seconds,omitempty"`
	PolicyToken      uint64              `json:"-"`
}

type StartInstanceRequest struct {
	ID               string              `json:"id,omitempty"`
	Image            string              `json:"image,omitempty"`
	DefaultUser      string              `json:"default_user,omitempty"`
	InitSystem       string              `json:"init,omitempty"`
	Kernel           string              `json:"kernel,omitempty"`
	Shares           []ShareMount        `json:"shares,omitempty"`
	PersistentMounts []PersistentMount   `json:"persistent_mounts,omitempty"`
	Network          *NetworkConfig      `json:"network,omitempty"`
	Display          *DisplayConfig      `json:"display,omitempty"`
	SharedMemory     *SharedMemoryConfig `json:"shared_memory,omitempty"`
	KernelModules    []string            `json:"kernel_modules,omitempty"`
	Env              []string            `json:"env,omitempty"`
	MemoryMB         uint64              `json:"memory_mb,omitempty"`
	BalloonMB        uint64              `json:"balloon_mb,omitempty"`
	CPUs             int                 `json:"cpus,omitempty"`
	NestedVirt       bool                `json:"nested_virtualization,omitempty"`
	AMD64Emulation   bool                `json:"amd64_emulation,omitempty"`
	Dmesg            bool                `json:"dmesg,omitempty"`
	SnapshotDir      string              `json:"snapshot_dir,omitempty"`
	RestoreSnapshot  string              `json:"restore_snapshot,omitempty"`
	TimeoutSeconds   float64             `json:"timeout_seconds,omitempty"`
	PolicyToken      uint64              `json:"-"`
}

type InstanceState struct {
	ID                            string                 `json:"id,omitempty"`
	Status                        string                 `json:"status"`
	Image                         string                 `json:"image,omitempty"`
	InitSystem                    string                 `json:"init,omitempty"`
	Kernel                        string                 `json:"kernel,omitempty"`
	MemoryMB                      uint64                 `json:"memory_mb,omitempty"`
	BalloonMB                     uint64                 `json:"balloon_mb,omitempty"`
	BalloonActualMB               uint64                 `json:"balloon_actual_mb,omitempty"`
	BalloonStatus                 string                 `json:"balloon_status,omitempty"`
	CPUs                          int                    `json:"cpus,omitempty"`
	NestedVirt                    bool                   `json:"nested_virtualization,omitempty"`
	SharedMemory                  *SharedMemoryConfig    `json:"shared_memory,omitempty"`
	StartedAt                     string                 `json:"started_at,omitempty"`
	NetworkIPv4                   string                 `json:"network_ipv4,omitempty"`
	Display                       *DisplayState          `json:"display,omitempty"`
	PersistentMounts              []PersistentMountState `json:"persistent_mounts,omitempty"`
	BackingBytes                  uint64                 `json:"backing_bytes,omitempty"`
	BackingHighWaterBytes         uint64                 `json:"backing_high_water_bytes,omitempty"`
	BackingDataBytes              uint64                 `json:"backing_data_bytes,omitempty"`
	BackingDataHighWaterBytes     uint64                 `json:"backing_data_high_water_bytes,omitempty"`
	BackingMetadataBytes          uint64                 `json:"backing_metadata_bytes,omitempty"`
	BackingMetadataHighWaterBytes uint64                 `json:"backing_metadata_high_water_bytes,omitempty"`
	BackingPhysicalBytes          uint64                 `json:"backing_physical_bytes,omitempty"`
	BackingReclaimError           string                 `json:"backing_reclaim_error,omitempty"`
	BackingUsageStale             bool                   `json:"backing_usage_stale,omitempty"`
	BackingActiveMutations        uint64                 `json:"backing_active_mutations,omitempty"`
	Error                         string                 `json:"error,omitempty"`
	ExitedAt                      string                 `json:"exited_at,omitempty"`
	ExitReason                    string                 `json:"exit_reason,omitempty"`
}

type ConsoleHistoryResponse struct {
	History string `json:"history"`
}

type RunRequest struct {
	ID             string         `json:"id,omitempty"`
	Image          string         `json:"image"`
	InitSystem     string         `json:"init,omitempty"`
	Kernel         string         `json:"kernel,omitempty"`
	Shares         []ShareMount   `json:"shares,omitempty"`
	Network        *NetworkConfig `json:"network,omitempty"`
	KernelModules  []string       `json:"kernel_modules,omitempty"`
	Command        []string       `json:"command,omitempty"`
	Env            []string       `json:"env,omitempty"`
	RootDir        string         `json:"root_dir,omitempty"`
	ReplaceEnv     bool           `json:"replace_env,omitempty"`
	WorkDir        string         `json:"workdir,omitempty"`
	User           string         `json:"user,omitempty"`
	Stdin          []byte         `json:"stdin,omitempty"`
	TTY            bool           `json:"tty,omitempty"`
	ControlFD      bool           `json:"control_fd,omitempty"`
	Cols           int            `json:"cols,omitempty"`
	Rows           int            `json:"rows,omitempty"`
	MemoryMB       uint64         `json:"memory_mb,omitempty"`
	BalloonMB      uint64         `json:"balloon_mb,omitempty"`
	CPUs           int            `json:"cpus,omitempty"`
	NestedVirt     bool           `json:"nested_virtualization,omitempty"`
	Dmesg          bool           `json:"dmesg,omitempty"`
	TimeoutSeconds float64        `json:"timeout_seconds,omitempty"`
	PolicyToken    uint64         `json:"-"`
}

type ExecResponse struct {
	ExitCode int            `json:"exit_code"`
	Output   string         `json:"output,omitempty"`
	Usage    *ResourceUsage `json:"usage,omitempty"`
}

type ResourceUsage struct {
	WallSeconds   float64 `json:"wall_seconds,omitempty"`
	UserSeconds   float64 `json:"user_seconds,omitempty"`
	SystemSeconds float64 `json:"system_seconds,omitempty"`
	CPUSeconds    float64 `json:"cpu_seconds,omitempty"`
	MaxRSSBytes   uint64  `json:"max_rss_bytes,omitempty"`
	MemoryBytes   uint64  `json:"memory_bytes,omitempty"`
}

type StartVMRequest = CreateInstanceRequest
type VMState = InstanceState
type RunVMResponse = ExecResponse

type ExecRequest struct {
	Kind          string         `json:"kind,omitempty"`
	ID            string         `json:"id,omitempty"`
	Image         string         `json:"image,omitempty"`
	Command       []string       `json:"command"`
	Env           []string       `json:"env,omitempty"`
	RootDir       string         `json:"root_dir,omitempty"`
	Path          string         `json:"path,omitempty"`
	Directory     bool           `json:"directory,omitempty"`
	ReplaceEnv    bool           `json:"replace_env,omitempty"`
	SkipResolve   bool           `json:"skip_resolve,omitempty"`
	WorkDir       string         `json:"workdir,omitempty"`
	User          string         `json:"user,omitempty"`
	Stdin         []byte         `json:"stdin,omitempty"`
	TTY           bool           `json:"tty,omitempty"`
	ControlFD     bool           `json:"control_fd,omitempty"`
	Cols          int            `json:"cols,omitempty"`
	Rows          int            `json:"rows,omitempty"`
	ArchiveLimits *ArchiveLimits `json:"archive_limits,omitempty"`
}

// ArchiveLimits bounds the expanded work performed by an fs_extract request.
// Zero fields use limits derived from the destination filesystem's current
// capacity. TimeoutSeconds is optional because callers are best placed to set
// a deadline appropriate to the amount of data they are sending.
type ArchiveLimits struct {
	MaxEntries       uint64  `json:"max_entries,omitempty"`
	MaxFileBytes     int64   `json:"max_file_bytes,omitempty"`
	MaxExpandedBytes int64   `json:"max_expanded_bytes,omitempty"`
	TimeoutSeconds   float64 `json:"timeout_seconds,omitempty"`
}

type ExecInput struct {
	Kind   string `json:"kind"`
	Input  string `json:"input,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Signal string `json:"signal,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
}

type ExecEvent struct {
	Kind     string `json:"kind"`
	Stream   string `json:"stream,omitempty"`
	Output   string `json:"output,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}
