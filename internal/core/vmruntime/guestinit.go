package vmruntime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/linux/initramfs"
)

type GuestInitConfig struct {
	Command            []string            `json:"command"`
	Env                []string            `json:"env"`
	WorkDir            string              `json:"workdir"`
	User               string              `json:"user,omitempty"`
	InitSystem         string              `json:"init,omitempty"`
	Hostname           string              `json:"hostname,omitempty"`
	Modules            []string            `json:"modules,omitempty"`
	EmulatorTag        string              `json:"emulator_tag,omitempty"`
	RootFSTag          string              `json:"rootfs_tag,omitempty"`
	RootFSImagePath    string              `json:"rootfs_image_path,omitempty"`
	RootFSImageType    string              `json:"rootfs_image_type,omitempty"`
	RootFSImage        []byte              `json:"-"`
	Shares             []GuestInitShare    `json:"shares,omitempty"`
	VsockPort          uint32              `json:"vsock_port,omitempty"`
	ReadyMarker        string              `json:"ready_marker,omitempty"`
	BeginMarker        string              `json:"begin_marker"`
	OutputMarkerPref   string              `json:"output_marker_prefix,omitempty"`
	ErrorMarkerPref    string              `json:"error_marker_prefix,omitempty"`
	ControlMarkerPref  string              `json:"control_marker_prefix,omitempty"`
	UsageMarkerPref    string              `json:"usage_marker_prefix,omitempty"`
	ExitMarkerPrefix   string              `json:"exit_marker_prefix"`
	PrecopyAMD64Root   bool                `json:"precopy_amd64_root,omitempty"`
	DisableCgroupMount bool                `json:"disable_cgroup_mount,omitempty"`
	Network            *GuestNetworkConfig `json:"network,omitempty"`
	SnapshotMMIOBase   uint64              `json:"snapshot_mmio_base,omitempty"`
	UnixTime           int64               `json:"unix_time,omitempty"`
	KernelRelease      string              `json:"kernel_release,omitempty"`
	ModuleSymversPath  string              `json:"module_symvers_path,omitempty"`
	ModuleSymvers      []byte              `json:"-"`
}

type GuestNetworkConfig struct {
	Interface string `json:"interface,omitempty"`
	Address   string `json:"address,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	DNS       string `json:"dns,omitempty"`
}

type GuestInitShare struct {
	Tag      string `json:"tag"`
	Mount    string `json:"mount"`
	Writable bool   `json:"writable,omitempty"`
}

func MergeEnv(base, overrides []string) []string {
	index := map[string]int{}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range overrides {
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

func HasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func WithDefaultEnv(env []string) []string {
	out := append([]string(nil), env...)
	if !HasEnvKey(out, "PATH") {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !HasEnvKey(out, "HOME") {
		out = append(out, "HOME=/root")
	}
	return out
}

func DefaultHostname(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" || hostname == "(none)" {
		return "ccx3"
	}
	return hostname
}

func ModulePaths(modules []alpine.Module) []string {
	if len(modules) == 0 {
		return nil
	}
	out := make([]string, 0, len(modules))
	for _, mod := range modules {
		out = append(out, "/ccx3/modules/"+mod.Name+".ko")
	}
	return out
}

func EmulatorTagForPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return EmulatorTag
}

func GuestShareConfigs(shares []DirectoryShare) []GuestInitShare {
	if len(shares) == 0 {
		return nil
	}
	out := make([]GuestInitShare, 0, len(shares))
	for i, share := range shares {
		out = append(out, GuestInitShare{
			Tag:      fmt.Sprintf("share%d", i),
			Mount:    share.Mount,
			Writable: share.Writable,
		})
	}
	return out
}

func BuildInitramfs(initPayload []byte, modules []alpine.Module, config GuestInitConfig) ([]byte, error) {
	if len(config.RootFSImage) > 0 {
		if strings.TrimSpace(config.RootFSImageType) == "" {
			config.RootFSImageType = "ext4"
		}
		if strings.TrimSpace(config.RootFSImagePath) == "" {
			config.RootFSImagePath = "/ccx3/rootfs." + strings.TrimSpace(config.RootFSImageType)
		}
	}
	if len(config.ModuleSymvers) > 0 {
		if strings.TrimSpace(config.KernelRelease) == "" {
			return nil, fmt.Errorf("kernel release is required with Module.symvers")
		}
		config.ModuleSymversPath = "/ccx3/Module.symvers"
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal guest init config: %w", err)
	}

	files := []initramfs.File{
		{Path: "/dev", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/proc", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/sys", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/run", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/tmp", Mode: 0o1777, Type: initramfs.TypeDirectory},
		{Path: "/etc", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/ccx3", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/ccx3/modules", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/dev/console", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 5, DevMinor: 1},
		{Path: "/dev/mem", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 1},
		{Path: "/dev/null", Mode: 0o666, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 3},
		{Path: "/dev/kmsg", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 11},
		{Path: "/dev/loop0", Mode: 0o660, Type: initramfs.TypeBlockDevice, DevMajor: 7, DevMinor: 0},
		{Path: "/etc/ccx3-init.json", Mode: 0o600, Data: configJSON, Type: initramfs.TypeRegular},
		{Path: "/init", Mode: 0o755, Data: initPayload, Type: initramfs.TypeRegular},
	}
	if len(config.RootFSImage) > 0 {
		files = append(files, initramfs.File{
			Path: config.RootFSImagePath,
			Mode: 0o600,
			Data: config.RootFSImage,
			Type: initramfs.TypeRegular,
		})
	}
	if len(config.ModuleSymvers) > 0 {
		files = append(files, initramfs.File{
			Path: config.ModuleSymversPath,
			Mode: 0o644,
			Data: config.ModuleSymvers,
			Type: initramfs.TypeRegular,
		})
	}
	for _, mod := range modules {
		files = append(files, initramfs.File{
			Path: "/ccx3/modules/" + mod.Name + ".ko",
			Mode: 0o644,
			Data: mod.Data,
			Type: initramfs.TypeRegular,
		})
	}
	return initramfs.Build(files)
}
