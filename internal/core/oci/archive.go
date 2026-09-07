package oci

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxDockerArchiveMetadataBytes = 16 << 20

type dockerArchiveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func (s *Store) pullDockerArchiveDirect(ctx context.Context, name string, spec SourceSpec) error {
	archivePath, selector, err := parseDockerArchiveSource(spec.Raw)
	if err != nil {
		return err
	}
	imageDir := s.imageDir(name)
	tmpDir := imageDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove temp image dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "layers"), 0o755); err != nil {
		return fmt.Errorf("create layers dir: %w", err)
	}

	manifestEntries, err := readDockerArchiveManifest(ctx, archivePath)
	if err != nil {
		return err
	}
	entry, err := selectDockerArchiveManifest(manifestEntries, selector)
	if err != nil {
		return err
	}
	configBlob, err := readDockerArchiveMember(ctx, archivePath, entry.Config, maxDockerArchiveMetadataBytes)
	if err != nil {
		return fmt.Errorf("read docker archive config %q: %w", entry.Config, err)
	}
	var cfg imageConfig
	if err := json.Unmarshal(configBlob, &cfg); err != nil {
		return fmt.Errorf("decode docker archive config: %w", err)
	}

	layerPlan := make(map[string]dockerArchiveLayerTarget, len(entry.Layers))
	layerPaths := make([]string, 0, len(entry.Layers))
	for i, rawLayer := range entry.Layers {
		member := cleanArchiveMemberName(rawLayer)
		if member == "" {
			return fmt.Errorf("docker archive layer %d has empty path", i)
		}
		layerRel := filepath.Join("layers", fmt.Sprintf("%03d-%s.tar", i, digestToFileName(member)))
		layerPlan[member] = dockerArchiveLayerTarget{
			index: i,
			rel:   layerRel,
			path:  filepath.Join(tmpDir, layerRel),
		}
		layerPaths = append(layerPaths, member)
	}
	if err := copyDockerArchiveLayers(ctx, archivePath, layerPlan); err != nil {
		return err
	}

	build := newIndexedBuildState()
	for i, layerMember := range layerPaths {
		target := layerPlan[layerMember]
		if err := applyIndexedLayer(target.path, target.rel, build.merged, build.fsEntries); err != nil {
			return fmt.Errorf("index docker archive layer %d %q: %w", i, layerMember, err)
		}
	}
	return s.finalizeIndexedImage(name, spec, "", imageDir, tmpDir, cfg, build)
}

type dockerArchiveLayerTarget struct {
	index int
	rel   string
	path  string
}

func parseDockerArchiveSource(source string) (archivePath string, selector string, err error) {
	const prefix = "docker-archive:"
	if !strings.HasPrefix(strings.ToLower(source), prefix) {
		return "", "", fmt.Errorf("docker archive source must start with %q", prefix)
	}
	value := source[len(prefix):]
	if value == "" {
		return "", "", fmt.Errorf("docker archive path is required")
	}
	if hash := strings.LastIndex(value, "#"); hash >= 0 {
		archivePath = value[:hash]
		selector = value[hash+1:]
	} else {
		archivePath = value
	}
	if strings.TrimSpace(archivePath) == "" {
		return "", "", fmt.Errorf("docker archive path is required")
	}
	return archivePath, strings.TrimSpace(selector), nil
}

func readDockerArchiveManifest(ctx context.Context, archivePath string) ([]dockerArchiveManifestEntry, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open docker archive: %w", err)
	}
	defer file.Close()

	var manifest []dockerArchiveManifestEntry
	tr := tar.NewReader(file)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read docker archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		name := cleanArchiveMemberName(hdr.Name)
		if name == "" {
			return nil, fmt.Errorf("invalid docker archive member %q", hdr.Name)
		}
		if name != "manifest.json" {
			continue
		}
		if hdr.Size < 0 || hdr.Size > maxDockerArchiveMetadataBytes {
			return nil, fmt.Errorf("docker archive manifest too large: %d bytes", hdr.Size)
		}
		data, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return nil, fmt.Errorf("read docker archive manifest: %w", err)
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("decode docker archive manifest: %w", err)
		}
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("docker archive manifest.json not found")
	}
	return manifest, nil
}

func readDockerArchiveMember(ctx context.Context, archivePath, rawMember string, maxSize int64) ([]byte, error) {
	member := cleanArchiveMemberName(rawMember)
	if member == "" {
		return nil, fmt.Errorf("empty member path")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open docker archive: %w", err)
	}
	defer file.Close()

	tr := tar.NewReader(file)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read docker archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		if cleanArchiveMemberName(hdr.Name) != member {
			continue
		}
		if hdr.Size < 0 || hdr.Size > maxSize {
			return nil, fmt.Errorf("member too large: %d bytes", hdr.Size)
		}
		data, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, fmt.Errorf("member not found")
}

func selectDockerArchiveManifest(entries []dockerArchiveManifestEntry, selector string) (dockerArchiveManifestEntry, error) {
	if selector == "" {
		if len(entries) != 1 {
			return dockerArchiveManifestEntry{}, fmt.Errorf("docker archive contains %d images; select one with #repo:tag", len(entries))
		}
		return entries[0], nil
	}
	for _, entry := range entries {
		for _, tag := range entry.RepoTags {
			if tag == selector {
				return entry, nil
			}
		}
	}
	return dockerArchiveManifestEntry{}, fmt.Errorf("docker archive image %q not found", selector)
}

func copyDockerArchiveLayers(ctx context.Context, archivePath string, targets map[string]dockerArchiveLayerTarget) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open docker archive: %w", err)
	}
	defer file.Close()

	remaining := make(map[string]dockerArchiveLayerTarget, len(targets))
	for key, target := range targets {
		remaining[key] = target
	}
	tr := tar.NewReader(file)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read docker archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		name := cleanArchiveMemberName(hdr.Name)
		target, ok := remaining[name]
		if !ok {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target.path), 0o755); err != nil {
			return fmt.Errorf("create layer dir: %w", err)
		}
		if err := writeLayerTarFromReader(target.path, "", tr); err != nil {
			return fmt.Errorf("copy docker archive layer %q: %w", name, err)
		}
		delete(remaining, name)
		if len(remaining) == 0 {
			return nil
		}
	}
	missing := make([]string, 0, len(remaining))
	for name := range remaining {
		missing = append(missing, name)
	}
	return fmt.Errorf("docker archive missing layers: %s", strings.Join(missing, ", "))
}

func cleanArchiveMemberName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" {
		return ""
	}
	name = path.Clean(strings.TrimPrefix(name, "/"))
	if name == "." || strings.HasPrefix(name, "../") {
		return ""
	}
	return name
}
