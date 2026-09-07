package runtime

import (
	"context"
	"fmt"
	intcvmfs "github.com/tinyrange/crumblecracker/internal/core/cvmfs"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/protocol"
	"path"
	"slices"
	"strings"
)

type preparedCVMFSHostMount struct {
	guestPath string
	root      imagefs.Directory
	repo      string
	mirrors   []string
	client    *intcvmfs.Client
}

func (s *Runtime) cvmfsMount(repo string) *preparedCVMFSHostMount {
	if s == nil {
		return nil
	}
	repo = strings.TrimSpace(repo)
	for index := range s.cvmfsMounts {
		if s.cvmfsMounts[index].repo == repo {
			return &s.cvmfsMounts[index]
		}
	}
	return nil
}

func prepareCVMFSHostMounts(configs []CVMFSHostMount, cacheDir string, monitor *cvmfsMonitor) ([]preparedCVMFSHostMount, error) {
	prepared := make([]preparedCVMFSHostMount, 0, len(configs))
	seen := make(map[string]bool, len(configs))
	for _, config := range configs {
		guestPath := path.Clean(strings.TrimSpace(config.Mount))
		if !strings.HasPrefix(guestPath, "/") || guestPath == "/" {
			return nil, fmt.Errorf("CVMFS host mount path %q must be an absolute non-root path", config.Mount)
		}
		if seen[guestPath] {
			return nil, fmt.Errorf("duplicate CVMFS host mount path %q", guestPath)
		}
		seen[guestPath] = true
		repo := strings.TrimSpace(config.Repo)
		if repo == "" || strings.Contains(repo, "/") {
			return nil, fmt.Errorf("invalid CVMFS repository %q", config.Repo)
		}
		mirror := strings.TrimSpace(config.Mirror)
		if mirror == "" {
			mirror = intcvmfs.DefaultMirror
		}
		mirror = intcvmfs.NormalizeMirror(mirror)
		client := intcvmfs.NewClient()
		client.CacheDir = cacheDir
		client.Mirrors = append([]string(nil), config.Mirrors...)
		if monitor != nil {
			client.OnTransfer = monitor.Record
		}
		target, err := intcvmfs.FormatTarget(intcvmfs.Target{
			Remote: true, Mirror: mirror, Repo: repo, Path: ensureAbsolutePath(config.Path),
		})
		if err != nil {
			return nil, fmt.Errorf("configure CVMFS host mount %q: %w", guestPath, err)
		}
		root, err := intcvmfs.NewImageFS(client, target)
		if err != nil {
			return nil, fmt.Errorf("configure CVMFS host mount %q: %w", guestPath, err)
		}
		mirrors := make([]string, 0, len(config.Mirrors)+1)
		seenMirrors := make(map[string]bool, len(config.Mirrors)+1)
		for _, candidate := range append([]string{mirror}, config.Mirrors...) {
			candidate = intcvmfs.NormalizeMirror(candidate)
			if candidate == "" || seenMirrors[candidate] {
				continue
			}
			seenMirrors[candidate] = true
			mirrors = append(mirrors, candidate)
		}
		prepared = append(prepared, preparedCVMFSHostMount{guestPath: guestPath, root: root, repo: repo, mirrors: mirrors, client: client})
	}
	return prepared, nil
}

func cvmfsHostCacheLimit(configs []CVMFSHostMount) (int64, error) {
	var limit int64
	for _, config := range configs {
		if config.CacheLimitBytes < 0 {
			return 0, fmt.Errorf("CVMFS cache limit must not be negative")
		}
		if config.CacheLimitBytes == 0 {
			continue
		}
		if limit != 0 && limit != config.CacheLimitBytes {
			return 0, fmt.Errorf("CVMFS host mounts sharing a cache must use one cache limit")
		}
		limit = config.CacheLimitBytes
	}
	return limit, nil
}

type CVMFSHostMount struct {
	Mount           string
	Mirror          string
	Mirrors         []string
	Repo            string
	Path            string
	CacheLimitBytes int64
}

func ensureAbsolutePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	if strings.HasPrefix(value, "/") {
		return value
	}
	return "/" + value
}

func (s *Runtime) CVMFSStatusContext(ctx context.Context) (client.CVMFSStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return client.CVMFSStatusResponse{}, err
	}
	return s.cvmfsMonitor.Status(), nil
}
func (s *Runtime) ProbeCVMFSMirrorsContext(ctx context.Context, req client.CVMFSMirrorProbeRequest) (client.CVMFSMirrorProbeResponse, error) {
	mount := s.cvmfsMount(req.Repo)
	if mount == nil {
		return client.CVMFSMirrorProbeResponse{}, fmt.Errorf("CVMFS repository %q is not configured", req.Repo)
	}
	selected, measured, err := intcvmfs.ProbeMirrors(ctx, mount.client.HTTPClient, mount.repo, mount.mirrors)
	if err != nil {
		return client.CVMFSMirrorProbeResponse{}, err
	}
	mount.client.SetPreferredMirror(selected)
	s.cvmfsMonitor.SetSelectedMirror(selected)
	results := make([]client.CVMFSMirrorProbeResult, 0, len(measured))
	for _, result := range measured {
		results = append(results, client.CVMFSMirrorProbeResult{
			Mirror: result.Mirror, ManifestLatencyMillis: result.ManifestLatency.Milliseconds(),
			RootCatalogMillis: result.RootCatalogDuration.Milliseconds(), RootCatalogBytes: result.RootCatalogBytes,
			RootCatalogBytesPerSec: result.RootCatalogBytesPerSec, Error: result.Error,
		})
	}
	return client.CVMFSMirrorProbeResponse{SelectedMirror: selected, Results: results}, nil
}
func (s *Runtime) SelectCVMFSMirrorContext(ctx context.Context, req client.CVMFSMirrorSelectionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mount := s.cvmfsMount(req.Repo)
	if mount == nil {
		return fmt.Errorf("CVMFS repository %q is not configured", req.Repo)
	}
	selected := intcvmfs.NormalizeMirror(req.Mirror)
	if !slices.Contains(mount.mirrors, selected) {
		return fmt.Errorf("CVMFS mirror %q is not configured", req.Mirror)
	}
	mount.client.SetPreferredMirror(selected)
	s.cvmfsMonitor.SetSelectedMirror(selected)
	return nil
}
