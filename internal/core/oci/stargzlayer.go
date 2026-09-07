package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
)

const (
	stargzLayerDirName    = "_stargz_v1"
	stargzLayerIndexName  = "layer.index"
	stargzContentsPrefix  = "estargz:"
	maxStargzTOCJSONBytes = 128 << 20
)

func (s *Store) stargzLayerIndexPath(digest string) string {
	return filepath.Join(s.root, stargzLayerDirName, digestToFileName(digest), stargzLayerIndexName)
}

func (s *Store) cachedStargzLayerAvailable(layer descriptor) bool {
	index, err := os.Open(s.stargzLayerIndexPath(layer.Digest))
	if err != nil {
		return false
	}
	defer index.Close()
	magic := make([]byte, len(layerArchiveMagic))
	if _, err := io.ReadFull(index, magic); err != nil || string(magic) != layerArchiveMagic {
		return false
	}
	return s.cachedLayerBlobAvailable(layer)
}

// prepareStargzLayerFromBlob recognizes an eStargz layer and creates the
// overlay index without expanding file contents. It returns false without an
// error when the blob is an ordinary OCI layer.
func (s *Store) prepareStargzLayerFromBlob(layer descriptor) (bool, error) {
	if s.cachedStargzLayerAvailable(layer) {
		return true, nil
	}
	blobPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest))
	toc, err := readStargzTOC(blobPath)
	if err != nil {
		if errors.Is(err, errNotStargzLayer) {
			return false, nil
		}
		return false, err
	}

	// Open with the upstream reader as well. Besides validating the footer and
	// TOC, this catches invalid chunk relationships before the image is cached.
	if _, err := openStargzReader(blobPath); err != nil {
		return false, fmt.Errorf("validate eStargz layer: %w", err)
	}

	indexPath := s.stargzLayerIndexPath(layer.Digest)
	parent := filepath.Dir(filepath.Dir(indexPath))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	tmpDir, err := os.MkdirTemp(parent, ".stargz-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmpDir)
	tmpIndex := filepath.Join(tmpDir, stargzLayerIndexName)
	if err := writeStargzLayerIndex(tmpIndex, toc); err != nil {
		return false, err
	}
	if err := os.Rename(tmpDir, filepath.Dir(indexPath)); err != nil && !s.cachedStargzLayerAvailable(layer) {
		return false, err
	}
	return true, nil
}

var errNotStargzLayer = errors.New("not an eStargz layer")

func readStargzTOC(blobPath string) (*estargz.JTOC, error) {
	file, err := os.Open(blobPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	sr := io.NewSectionReader(file, 0, info.Size())
	tocOffset, _, err := estargz.OpenFooter(sr)
	if err != nil || tocOffset < 0 || tocOffset >= info.Size() {
		return nil, errNotStargzLayer
	}
	gzr, err := gzip.NewReader(io.NewSectionReader(file, tocOffset, info.Size()-tocOffset))
	if err != nil {
		return nil, fmt.Errorf("open eStargz TOC gzip member: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("read eStargz TOC tar header: %w", err)
	}
	if path.Clean(strings.TrimPrefix(hdr.Name, "/")) != estargz.TOCTarName {
		return nil, fmt.Errorf("unexpected eStargz TOC entry %q", hdr.Name)
	}
	if hdr.Size < 0 || hdr.Size > maxStargzTOCJSONBytes {
		return nil, fmt.Errorf("eStargz TOC size %d exceeds limit", hdr.Size)
	}
	data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
	if err != nil {
		return nil, fmt.Errorf("read eStargz TOC: %w", err)
	}
	if int64(len(data)) != hdr.Size {
		return nil, fmt.Errorf("eStargz TOC length mismatch: expected %d, got %d", hdr.Size, len(data))
	}
	var toc estargz.JTOC
	if err := json.Unmarshal(data, &toc); err != nil {
		return nil, fmt.Errorf("decode eStargz TOC: %w", err)
	}
	if toc.Version != 1 {
		return nil, fmt.Errorf("unsupported eStargz TOC version %d", toc.Version)
	}
	return &toc, nil
}

func writeStargzLayerIndex(indexPath string, toc *estargz.JTOC) error {
	index, err := os.OpenFile(indexPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer index.Close()
	w := bufio.NewWriterSize(index, 64<<10)
	if _, err := w.WriteString(layerArchiveMagic); err != nil {
		return err
	}
	for _, entry := range toc.Entries {
		if entry == nil || entry.Type == "chunk" || isStargzLandmark(entry.Name) {
			continue
		}
		name, err := sanitizeArchivePath(entry.Name)
		if err != nil {
			return err
		}
		base := path.Base(name)
		dir := path.Dir(name)
		if base == ".wh..wh..opq" {
			if err := writeLayerRecord(w, layerRecord{
				op:   layerRecordOpaque,
				node: indexedNode{Path: fsmeta.Normalize(dir)},
			}); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			if err := writeLayerRecord(w, layerRecord{
				op: layerRecordDelete,
				node: indexedNode{
					Path: fsmeta.Normalize(path.Join(dir, strings.TrimPrefix(base, ".wh."))),
				},
			}); err != nil {
				return err
			}
			continue
		}

		hdr, err := stargzTarHeader(entry)
		if err != nil {
			return err
		}
		node := indexedNode{
			Path:      fsmeta.Normalize(name),
			UID:       uint32(entry.UID),
			GID:       uint32(entry.GID),
			Mode:      fsmeta.LinuxModeFromTarHeader(hdr),
			Size:      uint64(max(0, entry.Size)),
			ModTimeNS: hdr.ModTime.UnixNano(),
			RDev:      uint32(entry.DevMajor<<8 | entry.DevMinor),
		}
		record := layerRecord{op: layerRecordAdd, node: node}
		switch entry.Type {
		case "dir":
			record.node.Kind = indexedKindDir
			record.node.Size = 0
		case "reg":
			record.node.Kind = indexedKindFile
		case "char", "block", "fifo":
			record.node.Kind = indexedKindFile
			record.node.Size = 0
		case "symlink":
			record.node.Kind = indexedKindSymlink
			record.node.LinkTarget = fsmeta.NormalizeSymlinkTarget(entry.LinkName)
			record.node.Size = uint64(len(entry.LinkName))
		case "hardlink":
			target, err := sanitizeArchivePath(entry.LinkName)
			if err != nil {
				return err
			}
			record.op = layerRecordHardlink
			record.node.Kind = indexedKindFile
			record.node.Size = 0
			record.hardlinkTarget = fsmeta.Normalize(target)
		default:
			return fmt.Errorf("unsupported eStargz entry type %q for %s", entry.Type, name)
		}
		if err := writeLayerRecord(w, record); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return index.Sync()
}

func stargzTarHeader(entry *estargz.TOCEntry) (*tar.Header, error) {
	modTime := time.Unix(0, 0)
	if entry.ModTime3339 != "" {
		parsed, err := time.Parse(time.RFC3339Nano, entry.ModTime3339)
		if err != nil {
			return nil, fmt.Errorf("invalid eStargz mtime for %q: %w", entry.Name, err)
		}
		modTime = parsed
	}
	hdr := &tar.Header{
		Name:     entry.Name,
		Linkname: entry.LinkName,
		Size:     entry.Size,
		Mode:     entry.Mode,
		Uid:      entry.UID,
		Gid:      entry.GID,
		ModTime:  modTime,
		Devmajor: int64(entry.DevMajor),
		Devminor: int64(entry.DevMinor),
	}
	switch entry.Type {
	case "dir":
		hdr.Typeflag = tar.TypeDir
	case "reg":
		hdr.Typeflag = tar.TypeReg
	case "symlink":
		hdr.Typeflag = tar.TypeSymlink
	case "hardlink":
		hdr.Typeflag = tar.TypeLink
	case "char":
		hdr.Typeflag = tar.TypeChar
	case "block":
		hdr.Typeflag = tar.TypeBlock
	case "fifo":
		hdr.Typeflag = tar.TypeFifo
	default:
		return nil, fmt.Errorf("unsupported eStargz entry type %q", entry.Type)
	}
	return hdr, nil
}

func isStargzLandmark(name string) bool {
	clean := path.Clean(strings.TrimPrefix(name, "/"))
	return clean == estargz.PrefetchLandmark || clean == estargz.NoPrefetchLandmark
}

type filePathReaderAt string

func (p filePathReaderAt) ReadAt(buf []byte, offset int64) (int, error) {
	file, err := os.Open(string(p))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return file.ReadAt(buf, offset)
}

func openStargzReader(blobPath string) (*estargz.Reader, error) {
	info, err := os.Stat(blobPath)
	if err != nil {
		return nil, err
	}
	return estargz.Open(io.NewSectionReader(filePathReaderAt(blobPath), 0, info.Size()))
}
