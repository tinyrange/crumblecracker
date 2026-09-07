package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ociImageManifestMediaType    = "application/vnd.oci.image.manifest.v1+json"
	dockerImageManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	ociGzipLayerMediaType        = "application/vnd.oci.image.layer.v1.tar+gzip"
	dockerGzipLayerMediaType     = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	ociLayoutVersion             = "1.0.0"
)

type OCIImageRepackEvent struct {
	Manifest    int
	Layer       int
	Layers      int
	InputDigest string
	InputSize   int64
	Result      StargzRepackResult
}

type ociLayoutDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Data         []byte            `json:"data,omitempty"`
	Platform     *platform         `json:"platform,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
}

type ociLayoutIndex struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType,omitempty"`
	ArtifactType  string                `json:"artifactType,omitempty"`
	Manifests     []ociLayoutDescriptor `json:"manifests"`
	Subject       *ociLayoutDescriptor  `json:"subject,omitempty"`
	Annotations   map[string]string     `json:"annotations,omitempty"`
}

type ociLayoutManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType,omitempty"`
	ArtifactType  string                `json:"artifactType,omitempty"`
	Config        ociLayoutDescriptor   `json:"config"`
	Layers        []ociLayoutDescriptor `json:"layers"`
	Subject       *ociLayoutDescriptor  `json:"subject,omitempty"`
	Annotations   map[string]string     `json:"annotations,omitempty"`
}

type ociRootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

// RepackOCIImageLayout converts every gzip layer in a single-platform OCI
// image layout to enhanced eStargz. It writes a new, self-contained layout and
// leaves the conventional input layout unchanged.
func RepackOCIImageLayout(ctx context.Context, inputDir, outputDir string, chunkSize, minChunkSize int, report func(OCIImageRepackEvent)) error {
	if ctx == nil {
		return fmt.Errorf("repack context is required")
	}
	if _, err := os.Stat(outputDir); err == nil {
		return fmt.Errorf("output OCI layout already exists: %s", outputDir)
	} else if !os.IsNotExist(err) {
		return err
	}
	indexData, err := os.ReadFile(filepath.Join(inputDir, "index.json"))
	if err != nil {
		return fmt.Errorf("read OCI index: %w", err)
	}
	var index ociLayoutIndex
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("decode OCI index: %w", err)
	}
	if index.SchemaVersion != 2 || len(index.Manifests) == 0 {
		return fmt.Errorf("OCI layout must contain at least one schema-2 manifest")
	}

	parent := filepath.Dir(outputDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(parent, ".estargz-layout-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, "blobs", "sha256"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "oci-layout"), []byte("{\"imageLayoutVersion\":\""+ociLayoutVersion+"\"}\n"), 0o644); err != nil {
		return err
	}

	for manifestIndex := range index.Manifests {
		desc := index.Manifests[manifestIndex]
		if desc.MediaType != ociImageManifestMediaType && desc.MediaType != dockerImageManifestMediaType {
			return fmt.Errorf("unsupported OCI layout descriptor media type %q", desc.MediaType)
		}
		updated, err := repackOCILayoutManifest(ctx, inputDir, tmpDir, manifestIndex, desc, chunkSize, minChunkSize, report)
		if err != nil {
			return err
		}
		index.Manifests[manifestIndex] = updated
	}
	updatedIndex, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	updatedIndex = append(updatedIndex, '\n')
	if err := os.WriteFile(filepath.Join(tmpDir, "index.json"), updatedIndex, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, outputDir); err != nil {
		return fmt.Errorf("activate enhanced OCI layout: %w", err)
	}
	return nil
}

func repackOCILayoutManifest(ctx context.Context, inputDir, outputDir string, manifestIndex int, desc ociLayoutDescriptor, chunkSize, minChunkSize int, report func(OCIImageRepackEvent)) (ociLayoutDescriptor, error) {
	manifestData, err := readOCILayoutBlob(inputDir, desc.Digest)
	if err != nil {
		return ociLayoutDescriptor{}, fmt.Errorf("read manifest %s: %w", desc.Digest, err)
	}
	var manifest ociLayoutManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return ociLayoutDescriptor{}, fmt.Errorf("decode manifest %s: %w", desc.Digest, err)
	}
	configData, err := readOCILayoutBlob(inputDir, manifest.Config.Digest)
	if err != nil {
		return ociLayoutDescriptor{}, fmt.Errorf("read image config: %w", err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(configData, &config); err != nil {
		return ociLayoutDescriptor{}, fmt.Errorf("decode image config: %w", err)
	}
	var rootFS ociRootFS
	if err := json.Unmarshal(config["rootfs"], &rootFS); err != nil {
		return ociLayoutDescriptor{}, fmt.Errorf("decode image rootfs: %w", err)
	}
	if len(rootFS.DiffIDs) != len(manifest.Layers) {
		return ociLayoutDescriptor{}, fmt.Errorf("image has %d layers but %d DiffIDs", len(manifest.Layers), len(rootFS.DiffIDs))
	}

	for layerIndex := range manifest.Layers {
		layer := manifest.Layers[layerIndex]
		inputLayer := layer
		if layer.MediaType != ociGzipLayerMediaType && layer.MediaType != dockerGzipLayerMediaType {
			if err := copyOCILayoutBlob(inputDir, outputDir, layer.Digest); err != nil {
				return ociLayoutDescriptor{}, err
			}
			continue
		}
		inputPath, err := ociLayoutBlobPath(inputDir, layer.Digest)
		if err != nil {
			return ociLayoutDescriptor{}, err
		}
		provisional := filepath.Join(outputDir, "blobs", "sha256", fmt.Sprintf(".layer-%d-%d", manifestIndex, layerIndex))
		result, err := RepackStargzLayerFile(ctx, inputPath, provisional, chunkSize, minChunkSize)
		if err != nil {
			return ociLayoutDescriptor{}, fmt.Errorf("repack layer %d (%s): %w", layerIndex, layer.Digest, err)
		}
		outputPath, err := ociLayoutBlobPath(outputDir, result.BlobDigest)
		if err != nil {
			return ociLayoutDescriptor{}, err
		}
		if err := os.Rename(provisional, outputPath); err != nil {
			return ociLayoutDescriptor{}, err
		}
		layer.Digest = result.BlobDigest
		layer.Size = result.BlobSize
		manifest.Layers[layerIndex] = layer
		rootFS.DiffIDs[layerIndex] = result.DiffID
		if report != nil {
			report(OCIImageRepackEvent{Manifest: manifestIndex, Layer: layerIndex, Layers: len(manifest.Layers), InputDigest: inputLayer.Digest, InputSize: inputLayer.Size, Result: result})
		}
	}
	rootFSData, err := json.Marshal(rootFS)
	if err != nil {
		return ociLayoutDescriptor{}, err
	}
	config["rootfs"] = rootFSData
	configDesc, err := writeOCIJSONBlob(outputDir, config)
	if err != nil {
		return ociLayoutDescriptor{}, fmt.Errorf("write enhanced image config: %w", err)
	}
	originalConfig := manifest.Config
	configDesc.MediaType = originalConfig.MediaType
	configDesc.Annotations = originalConfig.Annotations
	configDesc.URLs = originalConfig.URLs
	configDesc.ArtifactType = originalConfig.ArtifactType
	manifest.Config = configDesc
	manifestDesc, err := writeOCIJSONBlob(outputDir, manifest)
	if err != nil {
		return ociLayoutDescriptor{}, fmt.Errorf("write enhanced manifest: %w", err)
	}
	manifestDesc.MediaType = desc.MediaType
	manifestDesc.Annotations = desc.Annotations
	manifestDesc.Platform = desc.Platform
	manifestDesc.URLs = desc.URLs
	manifestDesc.ArtifactType = desc.ArtifactType
	return manifestDesc, nil
}

func readOCILayoutBlob(layoutDir, digest string) ([]byte, error) {
	path, err := ociLayoutBlobPath(layoutDir, digest)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func copyOCILayoutBlob(inputDir, outputDir, digest string) error {
	source, err := ociLayoutBlobPath(inputDir, digest)
	if err != nil {
		return err
	}
	target, err := ociLayoutBlobPath(outputDir, digest)
	if err != nil {
		return err
	}
	return linkOrCopyLayerContents(source, target)
}

func ociLayoutBlobPath(layoutDir, digest string) (string, error) {
	if !validSHA256Digest(digest) {
		return "", fmt.Errorf("unsupported OCI blob digest %q", digest)
	}
	return filepath.Join(layoutDir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")), nil
}

func writeOCIJSONBlob(layoutDir string, value any) (ociLayoutDescriptor, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return ociLayoutDescriptor{}, err
	}
	hash := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(hash[:])
	path, err := ociLayoutBlobPath(layoutDir, digest)
	if err != nil {
		return ociLayoutDescriptor{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ociLayoutDescriptor{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return ociLayoutDescriptor{Digest: digest, Size: int64(len(data))}, nil
		}
		return ociLayoutDescriptor{}, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return ociLayoutDescriptor{}, err
	}
	if err := file.Close(); err != nil {
		return ociLayoutDescriptor{}, err
	}
	return ociLayoutDescriptor{Digest: digest, Size: int64(len(data))}, nil
}
