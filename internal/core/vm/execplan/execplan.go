package execplan

import (
	"fmt"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type Resolver struct {
	Root           imagefs.Directory
	BaseEnv        []string
	DefaultWorkDir string
	DefaultRootDir string
	MissingRootErr string
	Env            func(base, overrides []string, replace bool) []string
	User           func(string) (string, error)
	MarkResolved   bool
}

func ResolveExecRequest(req client.ExecRequest, resolver Resolver) (client.ExecRequest, error) {
	env := append([]string(nil), req.Env...)
	if resolver.Env != nil {
		env = resolver.Env(resolver.BaseEnv, req.Env, req.ReplaceEnv)
	}
	user := req.User
	if resolver.User != nil {
		resolved, err := resolver.User(req.User)
		if err != nil {
			return client.ExecRequest{}, err
		}
		user = resolved
	}
	command := append([]string(nil), req.Command...)
	if !req.SkipResolve {
		if resolver.Root == nil {
			if strings.TrimSpace(resolver.MissingRootErr) != "" {
				return client.ExecRequest{}, fmt.Errorf("%s", resolver.MissingRootErr)
			}
			return client.ExecRequest{}, fmt.Errorf("running instance does not have a root filesystem")
		}
		resolved, err := imagefs.ResolveCommand(resolver.Root, req.Command, env)
		if err != nil {
			return client.ExecRequest{}, err
		}
		command = resolved
	}
	workDir := req.WorkDir
	if workDir == "" {
		workDir = resolver.DefaultWorkDir
	}
	if workDir == "" {
		workDir = "/"
	}
	if !strings.HasPrefix(workDir, "/") {
		return client.ExecRequest{}, fmt.Errorf("workdir must be absolute")
	}
	rootDir := req.RootDir
	if rootDir == "" {
		rootDir = resolver.DefaultRootDir
	}
	skipResolve := req.SkipResolve
	if resolver.MarkResolved {
		skipResolve = true
	}
	return client.ExecRequest{
		ID:            req.ID,
		Kind:          req.Kind,
		Command:       command,
		Env:           env,
		RootDir:       rootDir,
		Path:          req.Path,
		Directory:     req.Directory,
		ReplaceEnv:    req.ReplaceEnv,
		SkipResolve:   skipResolve,
		WorkDir:       workDir,
		User:          user,
		Stdin:         append([]byte(nil), req.Stdin...),
		TTY:           req.TTY,
		ControlFD:     req.ControlFD,
		Cols:          req.Cols,
		Rows:          req.Rows,
		ArchiveLimits: req.ArchiveLimits,
	}, nil
}

func ResolveRunRequest(req client.RunRequest, rootDir string, resolver Resolver) (client.ExecRequest, error) {
	execReq := client.ExecRequest{
		ID:         req.ID,
		Command:    append([]string(nil), req.Command...),
		Env:        append([]string(nil), req.Env...),
		RootDir:    rootDir,
		ReplaceEnv: req.ReplaceEnv,
		WorkDir:    req.WorkDir,
		User:       req.User,
		Stdin:      append([]byte(nil), req.Stdin...),
		TTY:        req.TTY,
		ControlFD:  req.ControlFD,
		Cols:       req.Cols,
		Rows:       req.Rows,
	}
	resolved, err := ResolveExecRequest(execReq, resolver)
	if err != nil {
		return client.ExecRequest{}, err
	}
	resolved.ReplaceEnv = true
	resolved.SkipResolve = true
	return resolved, nil
}

func ControlRequest(req client.ExecRequest, defaultWorkDir string) client.ExecRequest {
	workDir := req.WorkDir
	if workDir == "" {
		workDir = defaultWorkDir
	}
	return client.ExecRequest{
		ID:            req.ID,
		Kind:          req.Kind,
		RootDir:       req.RootDir,
		Path:          req.Path,
		Directory:     req.Directory,
		WorkDir:       workDir,
		User:          req.User,
		Stdin:         append([]byte(nil), req.Stdin...),
		ArchiveLimits: req.ArchiveLimits,
	}
}

func CheckControlRequest(osName string, caps managedguest.Capabilities, kind string) error {
	display := displayName(osName)
	unsupported := func(feature string) error {
		return UnsupportedFeature(display, caps, feature)
	}
	switch strings.TrimSpace(kind) {
	case "", "exec", "sync":
		return nil
	case "fs_mkdir", "fs_write":
		if caps.CopyIn {
			return nil
		}
		return unsupported("copy into guest")
	case "fs_extract":
		if caps.CopyIn && caps.ArchiveExtract {
			return nil
		}
		if !caps.CopyIn {
			return unsupported("copy into guest")
		}
		return unsupported("archive extraction")
	case "fs_archive":
		if caps.CopyOut {
			return nil
		}
		return unsupported("copy out of guest")
	default:
		return fmt.Errorf("%s runtime does not support managed control request %q", display, kind)
	}
}

type CapabilityProvider interface {
	ManagedCapabilities() managedguest.Capabilities
}

func CheckAlternateImageExec(provider any) error {
	var caps managedguest.Capabilities
	if provider, ok := provider.(CapabilityProvider); ok {
		caps = provider.ManagedCapabilities()
	}
	if caps.AlternateImageExec {
		return nil
	}
	return UnsupportedFeature("managed guest", caps, "alternate images")
}

func UnsupportedFeature(runtimeName string, caps managedguest.Capabilities, feature string) error {
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		runtimeName = "managed guest"
	}
	feature = strings.TrimSpace(feature)
	if advertisedCapability(caps, feature) {
		return fmt.Errorf("%s runtime advertises %s but no implementation is wired", runtimeName, feature)
	}
	return fmt.Errorf("%s runtime does not support %s yet", runtimeName, feature)
}

func displayName(osName string) string {
	if strings.TrimSpace(osName) == "" {
		return "managed guest"
	}
	return osName
}

func advertisedCapability(caps managedguest.Capabilities, feature string) bool {
	switch strings.TrimSpace(feature) {
	case "filesystem shares":
		return caps.DynamicShares
	case "port forwards":
		return caps.PortForward
	case "alternate images":
		return caps.AlternateImageExec
	case "root snapshots":
		return caps.RootSnapshot
	case "image snapshots":
		return caps.ImageSnapshot
	case "copy into guest":
		return caps.CopyIn
	case "copy out of guest":
		return caps.CopyOut
	case "archive extraction":
		return caps.ArchiveExtract
	default:
		return false
	}
}
