package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/containerd/stargz-snapshotter/estargz"
)

const stargzDeltaIndexVersion = 1

type stargzDeltaMember struct {
	Digest string `json:"digest"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

type stargzDeltaIndex struct {
	Version int                 `json:"version"`
	Members []stargzDeltaMember `json:"members"`
}

type stargzTOCDocument struct {
	Version   int                 `json:"version"`
	Entries   []*estargz.TOCEntry `json:"entries"`
	VMSHDelta *stargzDeltaIndex   `json:"vmshDelta,omitempty"`
}

type StargzRepackResult struct {
	BlobDigest    string
	DiffID        string
	TOCJSONDigest string
	BlobSize      int64
	Members       int
}

// RepackStargzLayerFile converts a tar or compressed tar OCI layer to a
// deterministic, standard-compatible eStargz blob with a vmsh member index.
// The output is replaced atomically only after the complete blob and DiffID
// have been calculated successfully.
func RepackStargzLayerFile(ctx context.Context, inputPath, outputPath string, chunkSize, minChunkSize int) (StargzRepackResult, error) {
	if ctx == nil {
		return StargzRepackResult{}, fmt.Errorf("repack context is required")
	}
	if chunkSize <= 0 {
		chunkSize = 256 << 10
	}
	if minChunkSize < 0 {
		return StargzRepackResult{}, fmt.Errorf("minimum chunk size cannot be negative")
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return StargzRepackResult{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return StargzRepackResult{}, err
	}
	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return StargzRepackResult{}, err
	}
	baseBlob, err := os.CreateTemp(outputDir, ".estargz-base-")
	if err != nil {
		return StargzRepackResult{}, err
	}
	basePath := baseBlob.Name()
	defer os.Remove(basePath)

	built, err := estargz.Build(
		io.NewSectionReader(input, 0, info.Size()),
		estargz.WithContext(ctx),
		estargz.WithChunkSize(chunkSize),
		estargz.WithMinChunkSize(minChunkSize),
		estargz.WithCompressionLevel(gzip.DefaultCompression),
	)
	if err != nil {
		baseBlob.Close()
		return StargzRepackResult{}, err
	}
	_, copyErr := io.Copy(baseBlob, built)
	closeBuiltErr := built.Close()
	closeBaseErr := baseBlob.Close()
	if err := errorsJoin(copyErr, closeBuiltErr, closeBaseErr); err != nil {
		return StargzRepackResult{}, err
	}

	document, tocOffset, err := readStargzTOCDocument(basePath)
	if err != nil {
		return StargzRepackResult{}, err
	}
	members, err := hashStargzPayloadMembers(basePath, document.Entries, tocOffset)
	if err != nil {
		return StargzRepackResult{}, err
	}
	document.VMSHDelta = &stargzDeltaIndex{Version: stargzDeltaIndexVersion, Members: members}

	tmpOutput, err := os.CreateTemp(outputDir, ".estargz-final-")
	if err != nil {
		return StargzRepackResult{}, err
	}
	tmpPath := tmpOutput.Name()
	defer os.Remove(tmpPath)
	result, err := writeEnhancedStargz(basePath, tocOffset, tmpOutput, document)
	if err != nil {
		tmpOutput.Close()
		return StargzRepackResult{}, err
	}
	if err := tmpOutput.Close(); err != nil {
		return StargzRepackResult{}, err
	}
	diffID, err := stargzDiffID(tmpPath)
	if err != nil {
		return StargzRepackResult{}, err
	}
	result.DiffID = diffID
	result.Members = len(members)
	if err := os.Rename(tmpPath, outputPath); err != nil {
		return StargzRepackResult{}, err
	}
	return result, nil
}

func readStargzTOCDocument(blobPath string) (*stargzTOCDocument, int64, error) {
	file, err := os.Open(blobPath)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	tocOffset, _, err := estargz.OpenFooter(io.NewSectionReader(file, 0, info.Size()))
	if err != nil || tocOffset < 0 || tocOffset >= info.Size() {
		return nil, 0, errNotStargzLayer
	}
	document, err := decodeStargzTOCDocument(io.NewSectionReader(file, tocOffset, info.Size()-tocOffset))
	return document, tocOffset, err
}

func decodeStargzTOCDocument(compressed io.Reader) (*stargzTOCDocument, error) {
	gzr, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("open eStargz TOC gzip member: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("read eStargz TOC tar header: %w", err)
	}
	if hdr.Name != estargz.TOCTarName || hdr.Size < 0 || hdr.Size > maxStargzTOCJSONBytes {
		return nil, fmt.Errorf("invalid eStargz TOC entry %q with size %d", hdr.Name, hdr.Size)
	}
	data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != hdr.Size {
		return nil, fmt.Errorf("eStargz TOC length mismatch: expected %d, got %d", hdr.Size, len(data))
	}
	var document stargzTOCDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if document.Version != 1 {
		return nil, fmt.Errorf("unsupported eStargz TOC version %d", document.Version)
	}
	return &document, nil
}

func hashStargzPayloadMembers(blobPath string, entries []*estargz.TOCEntry, tocOffset int64) ([]stargzDeltaMember, error) {
	offsets := []int64{0}
	seen := map[int64]bool{0: true}
	for _, entry := range entries {
		if entry == nil || (entry.Type != "reg" && entry.Type != "chunk") {
			continue
		}
		if entry.Offset < 0 || entry.Offset >= tocOffset {
			return nil, fmt.Errorf("eStargz member offset %d is outside payload", entry.Offset)
		}
		if !seen[entry.Offset] {
			seen[entry.Offset] = true
			offsets = append(offsets, entry.Offset)
		}
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	file, err := os.Open(blobPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	members := make([]stargzDeltaMember, 0, len(offsets))
	for i, offset := range offsets {
		end := tocOffset
		if i+1 < len(offsets) {
			end = offsets[i+1]
		}
		if end <= offset {
			return nil, fmt.Errorf("invalid eStargz member range %d-%d", offset, end)
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, io.NewSectionReader(file, offset, end-offset)); err != nil {
			return nil, err
		}
		members = append(members, stargzDeltaMember{
			Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)),
			Offset: offset,
			Size:   end - offset,
		})
	}
	return members, nil
}

func writeEnhancedStargz(basePath string, tocOffset int64, output *os.File, document *stargzTOCDocument) (StargzRepackResult, error) {
	base, err := os.Open(basePath)
	if err != nil {
		return StargzRepackResult{}, err
	}
	defer base.Close()
	blobHash := sha256.New()
	w := io.MultiWriter(output, blobHash)
	if _, err := io.Copy(w, io.NewSectionReader(base, 0, tocOffset)); err != nil {
		return StargzRepackResult{}, err
	}
	tocJSON, err := json.MarshalIndent(document, "", "\t")
	if err != nil {
		return StargzRepackResult{}, err
	}
	gzw, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
	if err != nil {
		return StargzRepackResult{}, err
	}
	gzw.Header.ModTime = time.Unix(0, 0)
	gzw.Header.OS = 255
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: estargz.TOCTarName, Mode: 0o644, Size: int64(len(tocJSON))}); err != nil {
		return StargzRepackResult{}, err
	}
	if _, err := tw.Write(tocJSON); err != nil {
		return StargzRepackResult{}, err
	}
	if err := tw.Close(); err != nil {
		return StargzRepackResult{}, err
	}
	if err := gzw.Close(); err != nil {
		return StargzRepackResult{}, err
	}
	if _, err := w.Write(stargzFooterBytes(tocOffset)); err != nil {
		return StargzRepackResult{}, err
	}
	if err := output.Sync(); err != nil {
		return StargzRepackResult{}, err
	}
	info, err := output.Stat()
	if err != nil {
		return StargzRepackResult{}, err
	}
	tocHash := sha256.Sum256(tocJSON)
	return StargzRepackResult{
		BlobDigest:    "sha256:" + hex.EncodeToString(blobHash.Sum(nil)),
		TOCJSONDigest: fmt.Sprintf("sha256:%x", tocHash),
		BlobSize:      info.Size(),
	}, nil
}

func stargzFooterBytes(tocOffset int64) []byte {
	var out bytesBuffer
	gzw, _ := gzip.NewWriterLevel(&out, gzip.NoCompression)
	header := make([]byte, 4)
	header[0], header[1] = 'S', 'G'
	subfield := fmt.Sprintf("%016xSTARGZ", tocOffset)
	binary.LittleEndian.PutUint16(header[2:], uint16(len(subfield)))
	gzw.Extra = append(header, subfield...)
	_ = gzw.Close()
	return out.Bytes()
}

func stargzDiffID(blobPath string) (string, error) {
	file, err := os.Open(blobPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	gzr, err := gzip.NewReader(bufio.NewReader(file))
	if err != nil {
		return "", err
	}
	defer gzr.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, gzr); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

type bytesBuffer struct{ data []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *bytesBuffer) Bytes() []byte { return b.data }

func errorsJoin(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
