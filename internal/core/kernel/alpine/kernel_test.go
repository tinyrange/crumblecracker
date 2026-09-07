package alpine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAPKChecksumUsesControlGzipStream(t *testing.T) {
	signature := gzipBytes(t, []byte("signature"))
	control := gzipBytes(t, []byte("control"))
	data := gzipBytes(t, []byte("data"))
	path := filepath.Join(t.TempDir(), "package.apk")
	if err := os.WriteFile(path, bytes.Join([][]byte{signature, control, data}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	want := sha1.Sum(control)
	got, err := apkChecksumHex(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != fmt.Sprintf("%x", want) {
		t.Fatalf("checksum = %s, want %x", got, want)
	}
}

func TestFetchIndexEntriesRejectsMismatchedKernelMetadata(t *testing.T) {
	arch := defaultArch()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := fmt.Sprintf("P:linux-virt\nV:1.0-r0\nA:%s\n\nP:linux-virt-dev\nV:1.1-r0\nA:%s\n\n", arch, arch)
		_, _ = w.Write(gzipTar(t, map[string][]byte{"APKINDEX": []byte(index)}))
	}))
	defer server.Close()

	manager := NewManager(t.TempDir())
	manager.mirror = server.URL
	manager.httpClient = server.Client()
	if _, _, err := manager.fetchIndexEntries(context.Background()); err == nil {
		t.Fatal("mismatched kernel and development packages unexpectedly accepted")
	}
}

func TestEnsureDownloadedRecoversFromRepositoryIndexPackageRace(t *testing.T) {
	arch := defaultArch()
	packageData := gzipTar(t, map[string][]byte{"lib/modules/1.0-test/modules.dep": {}})
	developmentData := gzipTar(t, map[string][]byte{"usr/src/linux-headers-1.0-test/Module.symvers": []byte("0x12345678\tmodule_layout\tvmlinux\tEXPORT_SYMBOL\n")})
	actualDigest := sha1.Sum(packageData)
	developmentDigest := sha1.Sum(developmentData)
	staleDigest := sha1.Sum([]byte("previous package"))
	var mu sync.Mutex
	indexRequests, packageRequests, developmentRequests := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s cache control = %q", r.URL.Path, r.Header.Get("Cache-Control"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/APKINDEX.tar.gz"):
			mu.Lock()
			indexRequests++
			request := indexRequests
			mu.Unlock()
			digest := actualDigest[:]
			if request == 1 {
				digest = staleDigest[:]
			}
			index := fmt.Sprintf("P:linux-virt\nV:1.0-r0\nA:%s\nS:%d\nC:Q1%s\n\nP:linux-virt-dev\nV:1.0-r0\nA:%s\nS:%d\nC:Q1%s\n\n", arch, len(packageData), base64.StdEncoding.EncodeToString(digest), arch, len(developmentData), base64.StdEncoding.EncodeToString(developmentDigest[:]))
			_, _ = w.Write(gzipTar(t, map[string][]byte{"APKINDEX": []byte(index)}))
		case strings.HasSuffix(r.URL.Path, "/linux-virt-1.0-r0.apk"):
			mu.Lock()
			packageRequests++
			request := packageRequests
			mu.Unlock()
			if request > 1 && r.URL.Query().Get("cc-repository-retry") == "" {
				t.Error("retry did not bypass a stale package cache key")
			}
			_, _ = w.Write(packageData)
		case strings.HasSuffix(r.URL.Path, "/linux-virt-dev-1.0-r0.apk"):
			mu.Lock()
			developmentRequests++
			mu.Unlock()
			_, _ = w.Write(developmentData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewManager(t.TempDir())
	manager.mirror = server.URL
	manager.httpClient = server.Client()
	if err := manager.ensureDownloaded(context.Background(), nil); err != nil {
		t.Fatalf("ensure downloaded: %v", err)
	}
	metadata, err := manager.ReadKernelMetadata()
	if err != nil {
		t.Fatalf("read kernel metadata: %v", err)
	}
	if metadata.Release != "1.0-test" || !bytes.Equal(metadata.ModuleSymvers, []byte("0x12345678\tmodule_layout\tvmlinux\tEXPORT_SYMBOL\n")) {
		t.Fatalf("kernel metadata = release %q symvers %q", metadata.Release, metadata.ModuleSymvers)
	}
	mu.Lock()
	defer mu.Unlock()
	if indexRequests != 2 || packageRequests != 2 || developmentRequests != 1 {
		t.Fatalf("requests = index %d package %d development %d, want 2, 2, 1", indexRequests, packageRequests, developmentRequests)
	}
}

func gzipTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
