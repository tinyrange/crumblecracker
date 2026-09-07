package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/tinyrange/crumblecracker/internal/core/download"
	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

type testLayerEntry struct {
	header tar.Header
	body   []byte
}

func compressedTestLayer(t *testing.T, entries ...testLayerEntry) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzw := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gzw)
	for _, entry := range entries {
		header := entry.header
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			header.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) != 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func TestUncachedLayerIndexesWhileItDownloads(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	body := bytes.Repeat([]byte("streamed layer contents\n"), 1<<16)
	if err := tw.WriteHeader(&tar.Header{
		Name: "payload",
		Mode: 0o644,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	sum := sha256.Sum256(data)
	layer := descriptor{
		Digest:    fmt.Sprintf("sha256:%x", sum),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Size:      int64(len(data)),
	}

	releaseDownload := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		split := min(len(data), 128<<10)
		_, _ = w.Write(data[:split])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-releaseDownload
		_, _ = w.Write(data[split:])
	}))
	defer server.Close()

	store := NewStore(t.TempDir())
	reg := &registryContext{client: server.Client(), registry: server.URL}
	indexStarted := make(chan struct{})
	reported := false
	done := make(chan error, 1)
	go func() {
		done <- store.ensureLayerArchive(
			t.Context(),
			reg,
			"test/image",
			"test",
			layer,
			func(int64, float64) {},
			func(current int64) {
				if current > 0 && !reported {
					reported = true
					close(indexStarted)
				}
			},
			false,
		)
	}()

	select {
	case <-indexStarted:
	case <-time.After(2 * time.Second):
		close(releaseDownload)
		t.Fatal("indexing did not begin before the layer download completed")
	}
	close(releaseDownload)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !store.cachedLayerArchiveAvailable(layer) {
		t.Fatal("streamed layer archive was not retained")
	}
}

func TestLayerArchiveCachesPackedContentsAndPreservesOverlaySemantics(t *testing.T) {
	firstData := []byte("lower contents")
	first := compressedTestLayer(t,
		testLayerEntry{header: tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}},
		testLayerEntry{header: tar.Header{Name: "etc/old", Mode: 0o640, Uid: 10, Gid: 20}, body: []byte("remove me")},
		testLayerEntry{header: tar.Header{Name: "opaque/", Typeflag: tar.TypeDir, Mode: 0o755}},
		testLayerEntry{header: tar.Header{Name: "opaque/lower", Mode: 0o644}, body: firstData},
		testLayerEntry{header: tar.Header{Name: "replace/", Typeflag: tar.TypeDir, Mode: 0o755}},
		testLayerEntry{header: tar.Header{Name: "replace/lower", Mode: 0o644}, body: []byte("hidden")},
	)
	secondData := []byte("upper contents")
	second := compressedTestLayer(t,
		testLayerEntry{header: tar.Header{Name: "etc/.wh.old", Mode: 0o000}},
		testLayerEntry{header: tar.Header{Name: "opaque/new", Mode: 0o600, Uid: 30, Gid: 40}, body: secondData},
		testLayerEntry{header: tar.Header{Name: "opaque/.wh..wh..opq", Mode: 0o000}},
		testLayerEntry{header: tar.Header{Name: "opaque/hard", Typeflag: tar.TypeLink, Linkname: "opaque/new", Mode: 0o600}},
		testLayerEntry{header: tar.Header{Name: "current", Typeflag: tar.TypeSymlink, Linkname: "opaque/new", Mode: 0o777}},
		testLayerEntry{header: tar.Header{Name: "replace", Mode: 0o644}, body: []byte("now a file")},
	)

	store := NewStore(t.TempDir())
	firstLayer := descriptor{
		Digest:    "sha256:first",
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Size:      int64(len(first)),
	}
	secondLayer := descriptor{
		Digest:    "sha256:second",
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Size:      int64(len(second)),
	}
	firstIndex, firstContents, err := store.writeLayerArchiveAtomic(firstLayer, bytes.NewReader(first), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondIndex, secondContents, err := store.writeLayerArchiveAtomic(secondLayer, bytes.NewReader(second), nil)
	if err != nil {
		t.Fatal(err)
	}

	firstInfo, err := os.Stat(firstContents)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len("remove me") + len(firstData) + len("hidden")); firstInfo.Size() != want {
		t.Fatalf("first packed contents size = %d, want %d", firstInfo.Size(), want)
	}
	secondInfo, err := os.Stat(secondContents)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(secondData) + len("now a file")); secondInfo.Size() != want {
		t.Fatalf("second packed contents size = %d, want %d", secondInfo.Size(), want)
	}

	build := newIndexedBuildState()
	if err := applyLayerArchive(firstIndex, firstContents, build.merged, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyLayerArchive(secondIndex, secondContents, build.merged, nil); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"/etc/old", "/opaque/lower", "/replace/lower"} {
		if build.merged[removed] != nil {
			t.Fatalf("%s survived its OCI whiteout", removed)
		}
	}
	upper := build.merged["/opaque/new"]
	hardlink := build.merged["/opaque/hard"]
	if upper == nil || hardlink == nil {
		t.Fatalf("upper entries missing: new=%+v hard=%+v", upper, hardlink)
	}
	if hardlink.TarPath != upper.TarPath || hardlink.TarOffset != upper.TarOffset || hardlink.Size != upper.Size {
		t.Fatalf("hardlink does not share upper contents: new=%+v hard=%+v", upper, hardlink)
	}
	contents, err := os.Open(upper.TarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	got := make([]byte, upper.Size)
	if _, err := contents.ReadAt(got, int64(upper.TarOffset)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secondData) {
		t.Fatalf("upper contents = %q, want %q", got, secondData)
	}
	link := build.merged["/current"]
	if link == nil || link.LinkTarget != "opaque/new" {
		t.Fatalf("symlink = %+v", link)
	}
}

type failOnRead struct{}

func (failOnRead) Read([]byte) (int, error) {
	return 0, errors.New("cached layer was read again")
}

func TestLayerArchiveReusesCompletedLayerWithoutCompressedBlob(t *testing.T) {
	data := compressedTestLayer(t, testLayerEntry{
		header: tar.Header{Name: "bin/tool", Mode: 0o755},
		body:   []byte("tool"),
	})
	store := NewStore(t.TempDir())
	layer := descriptor{
		Digest:    "sha256:reused",
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Size:      int64(len(data)),
	}
	blobPath := filepath.Join(store.root, "_blobs", digestToFileName(layer.Digest))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.prepareLayerArchiveFromBlob(layer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded compressed blob remains: %v", err)
	}
	firstIndex, firstContents := store.layerArchivePaths(layer.Digest)
	secondIndex, secondContents, err := store.writeLayerArchiveAtomic(layer, failOnRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstIndex != secondIndex || firstContents != secondContents {
		t.Fatalf("cached paths changed: (%q, %q) != (%q, %q)", firstIndex, firstContents, secondIndex, secondContents)
	}

	imageContents := filepath.Join(t.TempDir(), "layers", "reused.contents")
	build := newIndexedBuildState()
	if err := store.writeAndApplyCachedLayer(
		layer,
		imageContents,
		"layers/reused.contents",
		build.merged,
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if node := build.merged["/bin/tool"]; node == nil || node.TarPath != "layers/reused.contents" {
		t.Fatalf("reused node = %+v", node)
	}
}

func TestLayerArchiveReadsZstdOCILayer(t *testing.T) {
	gzipped := compressedTestLayer(t, testLayerEntry{
		header: tar.Header{Name: "zstd", Mode: 0o644},
		body:   []byte("compressed with zstd"),
	})
	gzr, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		t.Fatal(err)
	}
	tarData, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatal(err)
	}
	if err := gzr.Close(); err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(tarData, nil)
	encoder.Close()

	store := NewStore(t.TempDir())
	layer := descriptor{
		Digest:    "sha256:zstd",
		MediaType: "application/vnd.oci.image.layer.v1.tar+zstd",
		Size:      int64(len(compressed)),
	}
	indexPath, contentsPath, err := store.writeLayerArchiveAtomic(layer, bytes.NewReader(compressed), nil)
	if err != nil {
		t.Fatal(err)
	}
	build := newIndexedBuildState()
	if err := applyLayerArchive(indexPath, contentsPath, build.merged, nil); err != nil {
		t.Fatal(err)
	}
	node := build.merged["/zstd"]
	if node == nil || node.Size != uint64(len("compressed with zstd")) {
		t.Fatalf("zstd layer node = %+v", node)
	}
}

func TestStreamedLayerDoesNotCacheUnverifiedArchive(t *testing.T) {
	data := compressedTestLayer(t, testLayerEntry{
		header: tar.Header{Name: "payload", Mode: 0o644},
		body:   []byte("valid tar with the wrong descriptor digest"),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	defer server.Close()

	store := NewStore(t.TempDir())
	layer := descriptor{
		Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Size:      int64(len(data)),
	}
	reg := &registryContext{client: server.Client(), registry: server.URL}
	err := store.downloadAndApplyLayer(
		t.Context(),
		reg,
		"test/image",
		layer,
		filepath.Join(t.TempDir(), "layer.contents"),
		"layers/layer.contents",
		newIndexedBuildState().merged,
		nil,
		nil,
	)
	var digestErr *download.DigestError
	if !errors.As(err, &digestErr) {
		t.Fatalf("streamed layer error = %v, want digest error", err)
	}
	if store.cachedLayerArchiveAvailable(layer) {
		t.Fatal("unverified layer archive was retained")
	}
}

func TestBinaryFilesystemIndexRoundTripAndLegacyJSONCompatibility(t *testing.T) {
	nodes := map[string]*indexedNode{
		"/": {
			Path: "/", Kind: indexedKindDir,
			Mode: fsmeta.LinuxModeFromFileMode(os.ModeDir | 0o755),
		},
		"/tool": {
			Path: "/tool", Kind: indexedKindFile, Mode: 0o100755,
			UID: 12, GID: 34, Size: 56, ModTimeNS: -78,
			TarPath: "layers/tool.contents", TarOffset: 90,
		},
	}
	encoded, err := encodeFSIndex(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, []byte(fsIndexMagic)) {
		t.Fatalf("binary filesystem index has no version magic")
	}
	decoded, err := decodeFSIndex(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want := []indexedNode{*nodes["/"], *nodes["/tool"]}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded index = %#v, want %#v", decoded, want)
	}

	legacy, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	decodedLegacy, err := decodeFSIndex(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodedLegacy, want) {
		t.Fatalf("decoded legacy index = %#v, want %#v", decodedLegacy, want)
	}
}

func TestFinalizedBinaryIndexOpensWithoutDuplicateMetadata(t *testing.T) {
	store := NewStore(t.TempDir())
	imageDir := store.imageDir("indexed")
	tmpDir := imageDir + ".tmp"
	contentsRel := filepath.Join("layers", "base.contents")
	contentsPath := filepath.Join(tmpDir, contentsRel)
	if err := os.MkdirAll(filepath.Dir(contentsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("indexed contents")
	if err := os.WriteFile(contentsPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	build := newIndexedBuildState()
	build.merged["/owned"] = &indexedNode{
		Path: "/owned", Kind: indexedKindFile,
		Mode: fsmeta.LinuxModeFromFileMode(0o640),
		UID:  123, GID: 456, Size: uint64(len(body)),
		TarPath: contentsRel,
	}
	spec := SourceSpec{Kind: SourceKindOCI, Raw: "example.invalid/indexed:latest"}
	if err := store.finalizeIndexedImage("indexed", spec, "", imageDir, tmpDir, imageConfig{}, build); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(imageDir, "rootfs.metadata.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate metadata file exists: %v", err)
	}

	image, err := store.Open("indexed")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := imagefs.LookupPath(image.RootFS, "/owned")
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := entry.File.Owner()
	if uid != 123 || gid != 456 {
		t.Fatalf("owner = %d:%d, want 123:456", uid, gid)
	}
	got, err := entry.File.ReadAt(0, uint32(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("contents = %q, want %q", got, body)
	}

	cloneStore := NewStore(t.TempDir())
	if err := cloneStore.cloneFromStore(store, "indexed", "clone", spec); err != nil {
		t.Fatal(err)
	}
	cloned, err := cloneStore.Open("clone")
	if err != nil {
		t.Fatal(err)
	}
	clonedEntry, err := imagefs.LookupPath(cloned.RootFS, "/owned")
	if err != nil {
		t.Fatal(err)
	}
	clonedBody, err := clonedEntry.File.ReadAt(0, uint32(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clonedBody, body) {
		t.Fatalf("cloned contents = %q, want %q", clonedBody, body)
	}
}

func TestLinkOrCopyLayerContentsReusesExistingHardLink(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "cached-layer")
	dstPath := filepath.Join(dir, "image", "layer")
	want := []byte("compressed layer contents")
	if err := os.WriteFile(srcPath, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkOrCopyLayerContents(srcPath, dstPath); err != nil {
		t.Fatal(err)
	}
	if err := linkOrCopyLayerContents(srcPath, dstPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cached layer contents = %q, want %q", got, want)
	}
}

var _ io.Reader = failOnRead{}
