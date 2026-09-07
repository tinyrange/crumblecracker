package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/tinyrange/crumblecracker/internal/core/download"
)

const maxRemoteStargzTOCBytes int64 = 128 << 20

var errRegistryRangeUnsupported = errors.New("registry does not support bounded blob ranges")

type localStargzMember struct {
	path   string
	offset int64
	size   int64
}

type plannedStargzMember struct {
	target stargzDeltaMember
	local  *localStargzMember
}

// tryReconstructStargzLayer attempts to assemble the exact target OCI blob
// using members from older local eStargz layers and missing byte ranges from
// the registry. A false result means the normal full-blob path should be used.
func (s *Store) tryReconstructStargzLayer(
	ctx context.Context,
	reg *registryContext,
	imageName string,
	layer descriptor,
	progress func(int64, float64),
) (bool, error) {
	document, tocOffset, err := fetchRemoteStargzTOCDocument(ctx, reg, imageName, layer)
	if err != nil {
		if errors.Is(err, errRegistryRangeUnsupported) || errors.Is(err, errNotStargzLayer) {
			return false, nil
		}
		// Delta metadata is optional. A malformed or unavailable index must not
		// make an otherwise valid OCI layer unusable.
		return false, nil
	}
	if err := validateStargzDeltaIndex(document.VMSHDelta, tocOffset); err != nil {
		return false, nil
	}
	available, err := s.localStargzMembers(layer.Digest)
	if err != nil {
		return false, err
	}
	planned := make([]plannedStargzMember, 0, len(document.VMSHDelta.Members)+1)
	var reusedBytes int64
	for _, member := range document.VMSHDelta.Members {
		item := plannedStargzMember{target: member}
		if local := available[member.Digest]; local != nil && local.size == member.Size {
			copy := *local
			item.local = &copy
			reusedBytes += member.Size
		}
		planned = append(planned, item)
	}
	// The TOC contains target-specific offsets and the complete member list, so
	// fetch it and the footer as one final range.
	planned = append(planned, plannedStargzMember{target: stargzDeltaMember{
		Offset: tocOffset,
		Size:   layer.Size - tocOffset,
	}})
	if reusedBytes == 0 {
		return false, nil
	}

	blobPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		return false, err
	}
	partialPath := blobPath + ".delta.partial"
	out, err := os.OpenFile(partialPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	completed := false
	defer func() {
		_ = out.Close()
		if !completed {
			_ = os.Remove(partialPath)
		}
	}()
	if err := out.Truncate(layer.Size); err != nil {
		return false, err
	}

	var reconstructed int64
	var networkBytes int64
	networkStarted := time.Now()
	report := func(current int64) {
		if progress == nil {
			return
		}
		var rate float64
		if elapsed := time.Since(networkStarted).Seconds(); elapsed > 0 {
			rate = float64(networkBytes) / elapsed
		}
		progress(current, rate)
	}
	for index := 0; index < len(planned); {
		item := planned[index]
		if item.local != nil {
			if err := copyFileRange(out, item.target.Offset, item.local.path, item.local.offset, item.target.Size); err != nil {
				return false, err
			}
			reconstructed += item.target.Size
			report(reconstructed)
			index++
			continue
		}
		start := item.target.Offset
		end := start + item.target.Size
		index++
		for index < len(planned) && planned[index].local == nil && planned[index].target.Offset == end {
			end += planned[index].target.Size
			index++
		}
		rangeBase := networkBytes
		if err := copyRemoteBlobRange(ctx, reg, "/"+imageName+"/blobs/"+layer.Digest, out, start, end-1, layer.Size, func(downloaded int64) {
			networkBytes = rangeBase + downloaded
			report(reconstructed + downloaded)
		}); err != nil {
			if errors.Is(err, errRegistryRangeUnsupported) {
				return false, nil
			}
			return false, err
		}
		reconstructed += end - start
		networkBytes = rangeBase + end - start
		report(reconstructed)
	}
	if err := out.Sync(); err != nil {
		return false, err
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	actual, err := fileSHA256Digest(partialPath)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(actual, layer.Digest) {
		// A producer/compressor mismatch cannot corrupt the active cache. Use the
		// full download path, which independently verifies the descriptor.
		return false, nil
	}
	if err := os.Rename(partialPath, blobPath); err != nil {
		return false, err
	}
	completed = true
	return true, nil
}

func fetchRemoteStargzTOCDocument(ctx context.Context, reg *registryContext, imageName string, layer descriptor) (*stargzTOCDocument, int64, error) {
	if layer.Size <= estargz.FooterSize {
		return nil, 0, errNotStargzLayer
	}
	path := "/" + imageName + "/blobs/" + layer.Digest
	footer, err := readRemoteBlobRange(ctx, reg, path, layer.Size-estargz.FooterSize, layer.Size-1, layer.Size, estargz.FooterSize)
	if err != nil {
		return nil, 0, err
	}
	_, tocOffset, _, err := (&estargz.GzipDecompressor{}).ParseFooter(footer)
	if err != nil || tocOffset < 0 || tocOffset >= layer.Size-estargz.FooterSize {
		return nil, 0, errNotStargzLayer
	}
	tocBytes := layer.Size - tocOffset
	if tocBytes > maxRemoteStargzTOCBytes {
		return nil, 0, fmt.Errorf("compressed eStargz TOC is too large: %d", tocBytes)
	}
	tail, err := readRemoteBlobRange(ctx, reg, path, tocOffset, layer.Size-1, layer.Size, tocBytes)
	if err != nil {
		return nil, 0, err
	}
	document, err := decodeStargzTOCDocument(bytes.NewReader(tail))
	if err != nil {
		return nil, 0, err
	}
	return document, tocOffset, nil
}

func validateStargzDeltaIndex(index *stargzDeltaIndex, payloadSize int64) error {
	if index == nil || index.Version != stargzDeltaIndexVersion || len(index.Members) == 0 {
		return fmt.Errorf("missing supported vmsh delta index")
	}
	var offset int64
	for _, member := range index.Members {
		if member.Offset != offset || member.Size <= 0 || !validSHA256Digest(member.Digest) {
			return fmt.Errorf("invalid vmsh delta member at offset %d", member.Offset)
		}
		offset += member.Size
		if offset > payloadSize {
			return fmt.Errorf("vmsh delta members exceed eStargz payload")
		}
	}
	if offset != payloadSize {
		return fmt.Errorf("vmsh delta members cover %d bytes, want %d", offset, payloadSize)
	}
	return nil
}

func (s *Store) localStargzMembers(excludeDigest string) (map[string]*localStargzMember, error) {
	dir := filepath.Join(s.root, "_blobs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*localStargzMember{}, nil
	}
	if err != nil {
		return nil, err
	}
	available := make(map[string]*localStargzMember)
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == digestToFileName(excludeDigest) || strings.HasSuffix(entry.Name(), ".partial") {
			continue
		}
		blobPath := filepath.Join(dir, entry.Name())
		document, tocOffset, err := readStargzTOCDocument(blobPath)
		if err != nil || validateStargzDeltaIndex(document.VMSHDelta, tocOffset) != nil {
			continue
		}
		for _, member := range document.VMSHDelta.Members {
			if available[member.Digest] == nil {
				available[member.Digest] = &localStargzMember{path: blobPath, offset: member.Offset, size: member.Size}
			}
		}
	}
	return available, nil
}

func copyFileRange(dst *os.File, dstOffset int64, srcPath string, srcOffset, size int64) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	n, err := io.Copy(io.NewOffsetWriter(dst, dstOffset), io.NewSectionReader(src, srcOffset, size))
	if err != nil {
		return err
	}
	if n != size {
		return &download.LengthError{Expected: size, Actual: n}
	}
	return nil
}

func copyRemoteBlobRange(ctx context.Context, reg *registryContext, path string, dst *os.File, start, end, total int64, progress func(int64)) error {
	resp, err := reg.doByteRange(ctx, path, start, end)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := validateBoundedBlobContentRange(resp, start, end, total); err != nil {
		return err
	}
	size := end - start + 1
	writer := &stargzRangeProgressWriter{dst: io.NewOffsetWriter(dst, start), report: progress}
	_, err = download.Copy(ctx, writer, resp, download.Budget{MaxBytes: size, ExpectedBytes: size})
	if err == nil && progress != nil {
		progress(size)
	}
	return err
}

type stargzRangeProgressWriter struct {
	dst        io.Writer
	written    int64
	lastReport time.Time
	report     func(int64)
}

func (w *stargzRangeProgressWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.written += int64(n)
	now := time.Now()
	if n > 0 && w.report != nil && (w.lastReport.IsZero() || now.Sub(w.lastReport) >= 200*time.Millisecond) {
		w.lastReport = now
		w.report(w.written)
	}
	return n, err
}

func readRemoteBlobRange(ctx context.Context, reg *registryContext, path string, start, end, total, maxBytes int64) ([]byte, error) {
	resp, err := reg.doByteRange(ctx, path, start, end)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := validateBoundedBlobContentRange(resp, start, end, total); err != nil {
		return nil, err
	}
	return download.ReadAll(ctx, resp, download.Budget{MaxBytes: maxBytes, ExpectedBytes: end - start + 1})
}

func (reg *registryContext) doByteRange(ctx context.Context, path string, start, end int64) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.registry+path, nil)
		if err != nil {
			return nil, err
		}
		if token := reg.bearerToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := reg.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry request: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			if err := reg.authorize(ctx, resp.Header.Get("www-authenticate")); err != nil {
				resp.Body.Close()
				return nil, err
			}
			resp.Body.Close()
			continue
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil, errRegistryRangeUnsupported
		}
		if resp.StatusCode != http.StatusPartialContent {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			return nil, &registryStatusError{code: resp.StatusCode, status: resp.Status, body: strings.TrimSpace(string(body))}
		}
		return resp, nil
	}
	return nil, fmt.Errorf("registry authorization failed")
}

func validateBoundedBlobContentRange(resp *http.Response, start, end, total int64) error {
	var actualStart, actualEnd, actualTotal int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &actualStart, &actualEnd, &actualTotal); err != nil {
		return fmt.Errorf("invalid OCI blob Content-Range %q", resp.Header.Get("Content-Range"))
	}
	if actualStart != start || actualEnd != end || actualTotal != total {
		return fmt.Errorf("unexpected OCI blob Content-Range %q", resp.Header.Get("Content-Range"))
	}
	return nil
}

func fileSHA256Digest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func validSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
