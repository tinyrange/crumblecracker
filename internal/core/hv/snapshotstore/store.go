package snapshotstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	stagingPrefix = ".snapshot-staging-"
	finalPrefix   = "snapshot-"
	staleAfter    = 24 * time.Hour
)

type Capture struct {
	root      string
	staging   string
	final     string
	published bool
}

// Begin creates a private capture directory. It is deliberately hidden from
// snapshot-* discovery until Publish atomically installs it.
func Begin(root string) (*Capture, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("snapshot root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create snapshot root: %w", err)
	}
	cleanupStale(root, time.Now())

	staging, err := os.MkdirTemp(root, stagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("create snapshot staging directory: %w", err)
	}
	suffix := strings.TrimPrefix(filepath.Base(staging), stagingPrefix)
	name := finalPrefix + time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + suffix
	return &Capture{root: root, staging: staging, final: filepath.Join(root, name)}, nil
}

func (c *Capture) Dir() string {
	if c == nil {
		return ""
	}
	return c.staging
}

// Abort removes an unpublished capture. It is safe to call after Publish.
func (c *Capture) Abort() error {
	if c == nil || c.published || c.staging == "" {
		return nil
	}
	return os.RemoveAll(c.staging)
}

// Publish validates and syncs every component before atomically making the
// capture visible. Required component names must be direct children.
func (c *Capture) Publish(required ...string) (string, error) {
	if c == nil || c.staging == "" || c.final == "" {
		return "", fmt.Errorf("snapshot capture is not initialized")
	}
	if c.published {
		return c.final, nil
	}
	for _, name := range required {
		if name == "" || filepath.Base(name) != name {
			return "", fmt.Errorf("invalid snapshot component %q", name)
		}
		info, err := os.Lstat(filepath.Join(c.staging, name))
		if err != nil {
			return "", fmt.Errorf("validate snapshot component %q: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return "", fmt.Errorf("snapshot component %q is not a non-empty regular file", name)
		}
	}

	entries, err := os.ReadDir(c.staging)
	if err != nil {
		return "", fmt.Errorf("read snapshot staging directory: %w", err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("inspect snapshot component %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("snapshot component %q is not a regular file", entry.Name())
		}
		if err := syncFile(filepath.Join(c.staging, entry.Name())); err != nil {
			return "", fmt.Errorf("sync snapshot component %q: %w", entry.Name(), err)
		}
	}
	if err := syncDir(c.staging); err != nil {
		return "", fmt.Errorf("sync snapshot staging directory: %w", err)
	}
	if err := os.Rename(c.staging, c.final); err != nil {
		return "", fmt.Errorf("publish snapshot: %w", err)
	}
	c.published = true
	if err := syncDir(c.root); err != nil {
		return "", fmt.Errorf("sync snapshot root: %w", err)
	}
	return c.final, nil
}

func syncFile(path string) error {
	// FlushFileBuffers requires a writable handle on Windows. Snapshot
	// components are still private staging files at this point.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func cleanupStale(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), stagingPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < staleAfter {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}
