package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRepackOCIImageLayoutProducesIndependentEnhancedImage(t *testing.T) {
	inputDir := filepath.Join(t.TempDir(), "input")
	outputDir := filepath.Join(t.TempDir(), "output")
	if err := os.MkdirAll(filepath.Join(inputDir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := deterministicTestBytes(700 << 10)
	var uncompressed bytes.Buffer
	tw := tar.NewWriter(&uncompressed)
	if err := tw.WriteHeader(&tar.Header{Name: "usr/share/demo.bin", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gzw := gzip.NewWriter(&compressed)
	if _, err := gzw.Write(uncompressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	layerDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(compressed.Bytes()))
	layerPath, err := ociLayoutBlobPath(inputDir, layerDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layerPath, compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	diffID := fmt.Sprintf("sha256:%x", sha256.Sum256(uncompressed.Bytes()))
	configDesc, err := writeOCIJSONBlob(inputDir, map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs":       ociRootFS{Type: "layers", DiffIDs: []string{diffID}},
		"config":       map[string]any{"User": "root"},
	})
	if err != nil {
		t.Fatal(err)
	}
	configDesc.MediaType = "application/vnd.oci.image.config.v1+json"
	manifestDesc, err := writeOCIJSONBlob(inputDir, ociLayoutManifest{
		SchemaVersion: 2,
		MediaType:     ociImageManifestMediaType,
		Config:        configDesc,
		Layers: []ociLayoutDescriptor{{
			MediaType: ociGzipLayerMediaType,
			Digest:    layerDigest,
			Size:      int64(compressed.Len()),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDesc.MediaType = ociImageManifestMediaType
	indexData, err := json.Marshal(ociLayoutIndex{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []ociLayoutDescriptor{manifestDesc}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "index.json"), indexData, 0o644); err != nil {
		t.Fatal(err)
	}

	var events []OCIImageRepackEvent
	if err := RepackOCIImageLayout(t.Context(), inputDir, outputDir, 64<<10, 64<<10, func(event OCIImageRepackEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].InputDigest != layerDigest || events[0].Result.BlobDigest == layerDigest {
		t.Fatalf("repack events = %+v", events)
	}
	if _, err := os.Stat(layerPath); err != nil {
		t.Fatalf("conventional input image changed: %v", err)
	}

	outputIndexData, err := os.ReadFile(filepath.Join(outputDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var outputIndex ociLayoutIndex
	if err := json.Unmarshal(outputIndexData, &outputIndex); err != nil {
		t.Fatal(err)
	}
	manifestData, err := readOCILayoutBlob(outputDir, outputIndex.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	var outputManifest ociLayoutManifest
	if err := json.Unmarshal(manifestData, &outputManifest); err != nil {
		t.Fatal(err)
	}
	if len(outputManifest.Layers) != 1 || outputManifest.Layers[0].Digest != events[0].Result.BlobDigest {
		t.Fatalf("enhanced manifest layers = %+v", outputManifest.Layers)
	}
	enhancedPath, err := ociLayoutBlobPath(outputDir, outputManifest.Layers[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := readStargzTOCDocument(enhancedPath)
	if err != nil {
		t.Fatal(err)
	}
	if document.VMSHDelta == nil || len(document.VMSHDelta.Members) < 2 {
		t.Fatalf("enhanced layer delta index = %+v", document.VMSHDelta)
	}
	outputConfigData, err := readOCILayoutBlob(outputDir, outputManifest.Config.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var outputConfig map[string]json.RawMessage
	if err := json.Unmarshal(outputConfigData, &outputConfig); err != nil {
		t.Fatal(err)
	}
	var outputRootFS ociRootFS
	if err := json.Unmarshal(outputConfig["rootfs"], &outputRootFS); err != nil {
		t.Fatal(err)
	}
	if len(outputRootFS.DiffIDs) != 1 || outputRootFS.DiffIDs[0] != events[0].Result.DiffID {
		t.Fatalf("enhanced DiffIDs = %v", outputRootFS.DiffIDs)
	}
}
