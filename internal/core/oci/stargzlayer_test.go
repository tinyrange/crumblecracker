package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestStargzLayerRemainsCompressedAndServesIndexedReads(t *testing.T) {
	body := make([]byte, 700<<10)
	for i := range body {
		body[i] = byte((i*31 + i/4093) % 251)
	}
	layerBlob := buildTestStargzLayer(t,
		testStargzEntry{name: "usr", mode: 0o755, typeflag: tar.TypeDir},
		testStargzEntry{name: "usr/share", mode: 0o755, typeflag: tar.TypeDir},
		testStargzEntry{name: "usr/share/payload.bin", mode: 0o640, body: body},
		testStargzEntry{name: "obsolete", mode: 0o644, body: []byte("old")},
	)

	store := NewStore(t.TempDir())
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(layerBlob))
	layer := descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    digest,
		Size:      int64(len(layerBlob)),
	}
	blobPath := filepath.Join(store.root, "_blobs", digestToFileName(digest))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, layerBlob, 0o644); err != nil {
		t.Fatal(err)
	}

	recognized, err := store.prepareStargzLayerFromBlob(layer)
	if err != nil {
		t.Fatal(err)
	}
	if !recognized || !store.cachedStargzLayerAvailable(layer) {
		t.Fatal("valid eStargz layer was not retained in the compressed cache")
	}
	if info, err := os.Stat(blobPath); err != nil || info.Size() != int64(len(layerBlob)) {
		t.Fatalf("compressed layer blob was not retained: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(store.root, layerArchiveDirName, digestToFileName(digest), layerContentsName)); !os.IsNotExist(err) {
		t.Fatalf("eStargz layer unexpectedly created expanded contents: %v", err)
	}

	imageDir := t.TempDir()
	layerRel := filepath.Join("layers", digestToFileName(digest)+".stargz")
	imageBlob := filepath.Join(imageDir, layerRel)
	if err := linkOrCopyLayerContents(blobPath, imageBlob); err != nil {
		t.Fatal(err)
	}
	build := newIndexedBuildState()
	if err := applyLayerArchive(
		store.stargzLayerIndexPath(digest),
		stargzContentsPrefix+layerRel,
		build.merged,
		build.fsEntries,
	); err != nil {
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
	entry, err := imagefs.LookupPath(root, "/usr/share/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if entry.File == nil {
		t.Fatal("payload is not an indexed file")
	}
	const offset = 60 << 10
	got, err := entry.File.ReadAt(offset, 24<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body[offset:offset+len(got)]) {
		t.Fatal("read crossing an eStargz chunk boundary returned incorrect bytes")
	}
}

func TestOrdinaryGzipLayerDoesNotEnterStargzCache(t *testing.T) {
	store := NewStore(t.TempDir())
	var compressed bytes.Buffer
	gzw := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "plain", Mode: 0o644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(compressed.Bytes()))
	layer := descriptor{Digest: digest, Size: int64(compressed.Len())}
	blobPath := filepath.Join(store.root, "_blobs", digestToFileName(digest))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	recognized, err := store.prepareStargzLayerFromBlob(layer)
	if err != nil {
		t.Fatal(err)
	}
	if recognized {
		t.Fatal("ordinary gzip layer was recognized as eStargz")
	}
}

func TestRepackStargzLayerAddsExactCompressedMemberIndex(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "input.tar")
	outputPath := filepath.Join(t.TempDir(), "output.tar.gz")
	input, err := os.Create(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(input)
	for _, entry := range []testStargzEntry{
		{name: "etc", mode: 0o755, typeflag: tar.TypeDir},
		{name: "etc/config", mode: 0o644, body: bytes.Repeat([]byte("configuration\n"), 40<<10)},
		{name: "usr", mode: 0o755, typeflag: tar.TypeDir},
		{name: "usr/tool", mode: 0o755, body: bytes.Repeat([]byte("tool payload\n"), 50<<10)},
	} {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: typeflag, Size: int64(len(entry.body))}
		if typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := RepackStargzLayerFile(t.Context(), inputPath, outputPath, 64<<10, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if result.BlobDigest == "" || result.DiffID == "" || result.TOCJSONDigest == "" || result.Members < 2 {
		t.Fatalf("incomplete repack result: %+v", result)
	}
	document, tocOffset, err := readStargzTOCDocument(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if document.VMSHDelta == nil || document.VMSHDelta.Version != stargzDeltaIndexVersion {
		t.Fatalf("missing vmsh delta index: %+v", document.VMSHDelta)
	}
	members, err := hashStargzPayloadMembers(outputPath, document.Entries, tocOffset)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStargzMembers(document.VMSHDelta.Members, members) {
		t.Fatalf("embedded member hashes do not describe output payload\nembedded=%+v\nactual=%+v", document.VMSHDelta.Members, members)
	}
	reader, err := openStargzReader(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := reader.OpenFile("usr/tool")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 32<<10)
	if _, err := file.ReadAt(got, 48<<10); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("tool payload\n"), 50<<10)[48<<10:80<<10]) {
		t.Fatal("enhanced eStargz layer returned incorrect file contents")
	}
}

func TestStargzDeltaReconstructionFetchesOnlyMissingTargetRanges(t *testing.T) {
	oldBody := deterministicTestBytes(2 << 20)
	newBody := append([]byte(nil), oldBody...)
	copy(newBody[900<<10:932<<10], deterministicTestBytes(32<<10))
	oldInput := filepath.Join(t.TempDir(), "old.tar")
	newInput := filepath.Join(t.TempDir(), "new.tar")
	writeSingleFileLayer(t, oldInput, oldBody)
	writeSingleFileLayer(t, newInput, newBody)
	oldBlob := filepath.Join(t.TempDir(), "old.tar.gz")
	newBlob := filepath.Join(t.TempDir(), "new.tar.gz")
	oldResult, err := RepackStargzLayerFile(t.Context(), oldInput, oldBlob, 64<<10, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	newResult, err := RepackStargzLayerFile(t.Context(), newInput, newBlob, 64<<10, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	targetData, err := os.ReadFile(newBlob)
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore(t.TempDir())
	oldCachePath := filepath.Join(store.root, "_blobs", digestToFileName(oldResult.BlobDigest))
	if err := os.MkdirAll(filepath.Dir(oldCachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(oldBlob, oldCachePath); err != nil {
		if err := copyFile(oldBlob, oldCachePath, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldInfo, err := os.Stat(oldBlob)
	if err != nil {
		t.Fatal(err)
	}
	if recognized, err := store.prepareStargzLayerFromBlob(descriptor{Digest: oldResult.BlobDigest, Size: oldInfo.Size()}); err != nil || !recognized {
		t.Fatalf("prepare old layer: recognized=%t err=%v", recognized, err)
	}

	var served int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(targetData)) {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		chunk := targetData[start : end+1]
		served += int64(len(chunk))
		w.Header().Set("Content-Length", fmt.Sprint(len(chunk)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(targetData)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(chunk)
	}))
	defer server.Close()
	target := descriptor{Digest: newResult.BlobDigest, Size: int64(len(targetData))}
	reconstructed, err := store.tryReconstructStargzLayer(
		t.Context(),
		&registryContext{client: server.Client(), registry: server.URL},
		"test/image",
		target,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reconstructed {
		t.Fatal("target layer was not reconstructed from reusable members")
	}
	got, err := os.ReadFile(filepath.Join(store.root, "_blobs", digestToFileName(target.Digest)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, targetData) {
		t.Fatal("delta reconstruction did not reproduce exact target OCI blob")
	}
	if served >= int64(len(targetData))/2 {
		t.Fatalf("delta fetched %d of %d bytes; expected substantial member reuse", served, len(targetData))
	}
}

func TestCopyRemoteBlobRangeReportsIntermediateProgress(t *testing.T) {
	data := deterministicTestBytes(1 << 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}))
	defer server.Close()

	dst, err := os.CreateTemp(t.TempDir(), "range-")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var reports []int64
	if err := copyRemoteBlobRange(t.Context(), &registryContext{client: server.Client(), registry: server.URL}, "/blob", dst, 0, int64(len(data)-1), int64(len(data)), func(current int64) {
		reports = append(reports, current)
	}); err != nil {
		t.Fatal(err)
	}
	if len(reports) < 2 || reports[0] <= 0 || reports[0] >= int64(len(data)) {
		t.Fatalf("range progress reports = %v, want an intermediate update", reports)
	}
	if got := reports[len(reports)-1]; got != int64(len(data)) {
		t.Fatalf("final range progress = %d, want %d", got, len(data))
	}
}

func TestPullEnhancedStargzImageKeepsCompressedLayerAndOpensFilesystem(t *testing.T) {
	payload := deterministicTestBytes(700 << 10)
	inputPath := filepath.Join(t.TempDir(), "layer.tar")
	outputPath := filepath.Join(t.TempDir(), "layer.tar.gz")
	writeSingleFileLayer(t, inputPath, payload)
	repacked, err := RepackStargzLayerFile(t.Context(), inputPath, outputPath, 64<<10, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	layerData, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"architecture":"amd64","config":{"User":"root"}}`)
	configSum := sha256.Sum256(configData)
	configDigest := fmt.Sprintf("sha256:%x", configSum)
	manifestData, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: []descriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    repacked.BlobDigest,
			Size:      int64(len(layerData)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/team/neuro/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		case r.URL.Path == "/v2/team/neuro/blobs/"+configDigest:
			_, _ = w.Write(configData)
		case r.URL.Path == "/v2/team/neuro/blobs/"+repacked.BlobDigest:
			// Returning 200 to the optional bounded probe exercises the safe
			// fallback to a complete, descriptor-verified blob download.
			w.Header().Set("Content-Length", fmt.Sprint(len(layerData)))
			_, _ = w.Write(layerData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sharedRoot := filepath.Join(t.TempDir(), "shared")
	store := NewStoreWithSharedCache(filepath.Join(t.TempDir(), "images"), sharedRoot)
	store.httpClient = server.Client()
	source := strings.TrimPrefix(server.URL, "https://") + "/team/neuro:latest"
	var reportedDownloadRate bool
	if _, err := store.Pull(t.Context(), "neuro", source, PullOptions{
		Architecture:         "amd64",
		KeepCompressedLayers: true,
		Report: func(event client.ProgressEvent) {
			if event.Status == "downloading" && event.RateBytesPerSecond > 0 {
				reportedDownloadRate = true
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !reportedDownloadRate {
		t.Fatal("compressed image pull did not report a download rate")
	}
	image, err := store.Open("neuro")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := imagefs.LookupPath(image.RootFS, "/usr/share/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := entry.File.ReadAt(63<<10, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload[63<<10:71<<10]) {
		t.Fatal("pulled image returned incorrect bytes across a compressed chunk boundary")
	}
	imageLayers, err := filepath.Glob(filepath.Join(image.RootFSDir, "layers", "*.stargz"))
	if err != nil || len(imageLayers) != 1 {
		allLayers, _ := filepath.Glob(filepath.Join(image.RootFSDir, "layers", "*"))
		t.Fatalf("compressed image layers = %v, all layers=%v, root=%q, err=%v", imageLayers, allLayers, image.RootFSDir, err)
	}
	if _, err := os.Stat(filepath.Join(sharedRoot, layerArchiveDirName, digestToFileName(repacked.BlobDigest), layerContentsName)); !os.IsNotExist(err) {
		t.Fatalf("pull expanded the enhanced layer: %v", err)
	}
}

func TestPullCompressedImageDownloadsLayersInParallel(t *testing.T) {
	configData := []byte(`{"architecture":"amd64","config":{"User":"root"}}`)
	configSum := sha256.Sum256(configData)
	configDigest := fmt.Sprintf("sha256:%x", configSum)
	layerData := make(map[string][]byte)
	layers := make([]descriptor, 0, 4)
	for index := range 4 {
		data := compressedTestLayer(t, testLayerEntry{
			header: tar.Header{Name: fmt.Sprintf("layer-%d", index), Mode: 0o644},
			body:   deterministicTestBytes((index + 1) * 64 << 10),
		})
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
		layerData[digest] = data
		layers = append(layers, descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    digest,
			Size:      int64(len(data)),
		})
	}
	manifestData, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: layers,
	})
	if err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	timeout := time.AfterFunc(2*time.Second, func() { releaseOnce.Do(func() { close(release) }) })
	defer timeout.Stop()
	var active atomic.Int64
	var maximum atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/team/parallel/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		case r.URL.Path == "/v2/team/parallel/blobs/"+configDigest:
			_, _ = w.Write(configData)
		default:
			digest := strings.TrimPrefix(r.URL.Path, "/v2/team/parallel/blobs/")
			data := layerData[digest]
			if data == nil {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				_, _ = w.Write(data)
				return
			}
			current := active.Add(1)
			defer active.Add(-1)
			for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
			}
			if current == 4 {
				releaseOnce.Do(func() { close(release) })
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = w.Write(data)
		}
	}))
	defer server.Close()

	store := NewStoreWithSharedCache(filepath.Join(t.TempDir(), "images"), filepath.Join(t.TempDir(), "shared"))
	store.httpClient = server.Client()
	source := strings.TrimPrefix(server.URL, "https://") + "/team/parallel:latest"
	if _, err := store.Pull(t.Context(), "parallel", source, PullOptions{
		Architecture:         "amd64",
		KeepCompressedLayers: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got < 4 {
		t.Fatalf("maximum concurrent layer downloads = %d, want at least 4", got)
	}
}

func equalStargzMembers(a, b []stargzDeltaMember) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func deterministicTestBytes(size int) []byte {
	out := make([]byte, size)
	state := uint64(0x9e3779b97f4a7c15)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = byte(state)
	}
	return out
}

func writeSingleFileLayer(t *testing.T, path string, body []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(file)
	for _, hdr := range []*tar.Header{
		{Name: "usr", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "usr/share", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "usr/share/payload.bin", Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(body))},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

type testStargzEntry struct {
	name     string
	mode     int64
	typeflag byte
	body     []byte
}

func buildTestStargzLayer(t *testing.T, entries ...testStargzEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     entry.name,
			Mode:     entry.mode,
			Typeflag: typeflag,
			Size:     int64(len(entry.body)),
		}
		if typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	blob, err := estargz.Build(
		io.NewSectionReader(bytes.NewReader(raw.Bytes()), 0, int64(raw.Len())),
		estargz.WithChunkSize(64<<10),
		estargz.WithMinChunkSize(64<<10),
	)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := blob.Close(); err != nil {
		t.Fatal(err)
	}
	return data
}
