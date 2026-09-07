package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
)

// EnsureInternalScratch materializes Docker's special scratch base image as a
// local writable root filesystem. Docker Hub documents scratch, but it is not a
// pullable registry manifest.
func (s *Store) EnsureInternalScratch(ctx context.Context, name, architecture string) error {
	return s.ensureInternalScratch(ctx, name, architecture)
}

func (s *Store) ensureInternalScratch(ctx context.Context, name, architecture string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateImageStoreName(name); err != nil {
		return err
	}
	architecture = normalizeArchitecture(architecture)

	if meta, err := s.readMetadata(name); err == nil && meta.SourceKind == SourceKindInternal && meta.Source == internalScratchSource {
		if architecture == "" || meta.Architecture == "" || meta.Architecture == architecture {
			return nil
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if architecture == "" {
		architecture = runtime.GOARCH
	}

	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("create image store: %w", err)
	}
	imageDir := s.imageDir(name)
	tmpDir := imageDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove temp image dir: %w", err)
	}
	rootDir := filepath.Join(tmpDir, "rootfs")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return fmt.Errorf("create scratch rootfs: %w", err)
	}

	entries := map[string]fsmeta.Entry{
		"/": {
			UID:  0,
			GID:  0,
			Mode: fsmeta.LinuxModeFromFileMode(os.ModeDir | 0o755),
		},
	}
	fsMetaBuf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal scratch fs metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "rootfs.metadata.json"), fsMetaBuf, 0o644); err != nil {
		return fmt.Errorf("write scratch fs metadata: %w", err)
	}

	meta := metadata{
		Name:         name,
		Source:       internalScratchSource,
		SourceKind:   SourceKindInternal,
		Architecture: architecture,
		RootFSDir:    filepath.Join(imageDir, "rootfs"),
		MetadataPath: filepath.Join(imageDir, "rootfs.metadata.json"),
		WorkingDir:   "/",
	}
	metaBuf, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal scratch image metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "image.json"), metaBuf, 0o644); err != nil {
		return fmt.Errorf("write scratch image metadata: %w", err)
	}
	if err := os.RemoveAll(imageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old scratch image: %w", err)
	}
	if err := os.Rename(tmpDir, imageDir); err != nil {
		return fmt.Errorf("activate scratch image: %w", err)
	}
	return nil
}
