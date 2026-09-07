package oci

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

// TestRealStargzLayerRuntimeRead provides a repeatable integration check for a
// large producer-generated layer without committing that layer as a fixture.
// Set CC_TEST_ESTARGZ_LAYER to an enhanced eStargz blob to enable it.
func TestRealStargzLayerRuntimeRead(t *testing.T) {
	sourcePath := os.Getenv("CC_TEST_ESTARGZ_LAYER")
	if sourcePath == "" {
		t.Skip("CC_TEST_ESTARGZ_LAYER is not set")
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256Digest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	layer := descriptor{Digest: digest, Size: info.Size(), MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"}
	store := NewStore(t.TempDir())
	blobPath := filepath.Join(store.root, "_blobs", digestToFileName(digest))
	if err := linkOrCopyLayerContents(sourcePath, blobPath); err != nil {
		t.Fatal(err)
	}
	recognized, err := store.prepareStargzLayerFromBlob(layer)
	if err != nil || !recognized {
		t.Fatalf("prepare real eStargz layer: recognized=%t err=%v", recognized, err)
	}

	document, _, err := readStargzTOCDocument(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	var candidatePath string
	var candidateSize int64
	for _, entry := range document.Entries {
		if entry != nil && entry.Type == "reg" && entry.Size >= 512<<10 {
			candidatePath, candidateSize = entry.Name, entry.Size
			break
		}
	}
	if candidatePath == "" {
		t.Fatal("real layer has no regular file large enough for a chunk-crossing read")
	}

	imageDir := t.TempDir()
	layerRel := filepath.Join("layers", digestToFileName(digest)+".stargz")
	if err := linkOrCopyLayerContents(blobPath, filepath.Join(imageDir, layerRel)); err != nil {
		t.Fatal(err)
	}
	build := newIndexedBuildState()
	if err := applyLayerArchive(store.stargzLayerIndexPath(digest), stargzContentsPrefix+layerRel, build.merged, build.fsEntries); err != nil {
		t.Fatal(err)
	}
	ensureIndexedParents(build.merged, build.fsEntries)
	nodes := make([]indexedNode, 0, len(build.merged))
	for _, node := range build.merged {
		nodes = append(nodes, *node)
	}
	root, err := buildIndexedRootFS(imageDir, nodes)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := imagefs.LookupPath(root, "/"+candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	offset := uint64(255 << 10)
	readSize := uint32(32 << 10)
	if offset+uint64(readSize) > uint64(candidateSize) {
		t.Fatalf("selected file %q is unexpectedly too small", candidatePath)
	}
	got, err := entry.File.ReadAt(offset, readSize)
	if err != nil {
		t.Fatal(err)
	}
	standardReader, err := openStargzReader(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	standardFile, err := standardReader.OpenFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, readSize)
	if _, err := standardFile.ReadAt(want, int64(offset)); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("runtime read differs from standard eStargz reader for %s", candidatePath)
	}
	if _, err := os.Stat(filepath.Join(store.root, layerArchiveDirName, digestToFileName(digest), layerContentsName)); !os.IsNotExist(err) {
		t.Fatalf("real layer was expanded on disk: %v", err)
	}
	t.Logf("verified %s (%s) directly from %d compressed bytes", candidatePath, fmt.Sprint(candidateSize), info.Size())
}

// TestRealStargzLayerDeltaReconstruction uses two locally supplied enhanced
// layers while serving the target through an OCI-like Range endpoint. It
// verifies the exact downloader path and reports actual transfer reuse.
func TestRealStargzLayerDeltaReconstruction(t *testing.T) {
	basePath := os.Getenv("CC_TEST_ESTARGZ_BASE")
	targetPath := os.Getenv("CC_TEST_ESTARGZ_TARGET")
	if basePath == "" || targetPath == "" {
		t.Skip("CC_TEST_ESTARGZ_BASE and CC_TEST_ESTARGZ_TARGET are not set")
	}
	targetFile, err := os.Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer targetFile.Close()
	targetInfo, err := targetFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	baseInfo, err := os.Stat(basePath)
	if err != nil {
		t.Fatal(err)
	}
	baseDigest, err := fileSHA256Digest(basePath)
	if err != nil {
		t.Fatal(err)
	}
	targetDigest, err := fileSHA256Digest(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore(t.TempDir())
	baseBlobPath := filepath.Join(store.root, "_blobs", digestToFileName(baseDigest))
	if err := linkOrCopyLayerContents(basePath, baseBlobPath); err != nil {
		t.Fatal(err)
	}
	if recognized, err := store.prepareStargzLayerFromBlob(descriptor{Digest: baseDigest, Size: baseInfo.Size()}); err != nil || !recognized {
		t.Fatalf("prepare base layer: recognized=%t err=%v", recognized, err)
	}

	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= targetInfo.Size() {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		size := end - start + 1
		served.Add(size)
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, targetInfo.Size()))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.CopyN(w, io.NewSectionReader(targetFile, start, size), size)
	}))
	defer server.Close()
	target := descriptor{Digest: targetDigest, Size: targetInfo.Size()}
	reconstructed, err := store.tryReconstructStargzLayer(t.Context(), &registryContext{client: server.Client(), registry: server.URL}, "demo/neuro", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reconstructed {
		t.Fatal("real target layer was not reconstructed")
	}
	downloaded := served.Load()
	if downloaded >= target.Size {
		t.Fatalf("delta downloaded %d bytes for a %d-byte target", downloaded, target.Size)
	}
	t.Logf("reconstructed %d bytes after downloading %d bytes (%.4f%%)", target.Size, downloaded, float64(downloaded)*100/float64(target.Size))
}
