package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/tinyrange/crumblecracker/internal/core/download"
	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
)

const (
	layerArchiveMagic   = "CCLAYER1"
	layerArchiveDirName = "_layers_v1"
	layerIndexName      = "layer.index"
	layerContentsName   = "layer.contents"

	layerRecordAdd      = 1
	layerRecordDelete   = 2
	layerRecordOpaque   = 3
	layerRecordHardlink = 4

	layerKindDirectory = 1
	layerKindFile      = 2
	layerKindSymlink   = 3

	maxLayerIndexString = 16 << 20
)

type layerRecord struct {
	op             byte
	node           indexedNode
	hardlinkTarget string
}

func (s *Store) layerArchivePaths(digest string) (indexPath, contentsPath string) {
	dir := filepath.Join(s.root, layerArchiveDirName, digestToFileName(digest))
	return filepath.Join(dir, layerIndexName), filepath.Join(dir, layerContentsName)
}

func (s *Store) cachedLayerArchiveAvailable(layer descriptor) bool {
	indexPath, contentsPath := s.layerArchivePaths(layer.Digest)
	index, err := os.Open(indexPath)
	if err != nil {
		return false
	}
	defer index.Close()
	magic := make([]byte, len(layerArchiveMagic))
	if _, err := io.ReadFull(index, magic); err != nil || string(magic) != layerArchiveMagic {
		return false
	}
	info, err := os.Stat(contentsPath)
	return err == nil && info.Mode().IsRegular()
}

func (s *Store) writeLayerArchiveAtomic(
	layer descriptor,
	body io.Reader,
	progress func(int64),
) (string, string, error) {
	indexPath, contentsPath := s.layerArchivePaths(layer.Digest)
	if s.cachedLayerArchiveAvailable(layer) {
		return indexPath, contentsPath, nil
	}
	parent := filepath.Dir(filepath.Dir(indexPath))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", err
	}
	tmpDir, err := os.MkdirTemp(parent, ".layer-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmpDir)

	tmpIndex := filepath.Join(tmpDir, layerIndexName)
	tmpContents := filepath.Join(tmpDir, layerContentsName)
	if err := writeLayerArchive(tmpIndex, tmpContents, layer.MediaType, body, progress); err != nil {
		return "", "", err
	}
	finalDir := filepath.Dir(indexPath)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		if !s.cachedLayerArchiveAvailable(layer) {
			return "", "", err
		}
	}
	return indexPath, contentsPath, nil
}

func (s *Store) prepareLayerArchiveFromBlob(layer descriptor, progress func(int64)) error {
	if s.cachedLayerArchiveAvailable(layer) {
		if progress != nil {
			progress(layer.Size)
		}
		return nil
	}
	blobPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest))
	file, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	_, _, writeErr := s.writeLayerArchiveAtomic(layer, file, progress)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	_ = os.Remove(blobPath)
	return nil
}

func writeLayerArchive(indexPath, contentsPath, mediaType string, body io.Reader, progress func(int64)) error {
	index, err := os.OpenFile(indexPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer index.Close()
	contents, err := os.OpenFile(contentsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer contents.Close()

	indexWriter := bufio.NewWriterSize(index, 64<<10)
	if _, err := indexWriter.WriteString(layerArchiveMagic); err != nil {
		return err
	}

	tracked := &countingReader{r: body}
	if progress != nil {
		tracked.report = func(current, _ int64) {
			progress(current)
		}
	}
	buffered := bufio.NewReader(tracked)
	var src io.Reader = buffered
	var gzr *gzip.Reader
	var zstdr *zstd.Decoder
	gzipLayer := strings.Contains(mediaType, "gzip")
	zstdLayer := strings.Contains(mediaType, "zstd")
	if !gzipLayer && !zstdLayer {
		if magic, peekErr := buffered.Peek(4); peekErr == nil {
			gzipLayer = magic[0] == 0x1f && magic[1] == 0x8b
			zstdLayer = magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd
		}
	}
	switch {
	case gzipLayer:
		gzr, err = gzip.NewReader(src)
		if err != nil {
			return fmt.Errorf("open gzip layer: %w", err)
		}
		defer gzr.Close()
		src = gzr
	case zstdLayer:
		zstdr, err = zstd.NewReader(src)
		if err != nil {
			return fmt.Errorf("open zstd layer: %w", err)
		}
		defer zstdr.Close()
		src = zstdr
	}

	maxBytes, err := download.FilesystemBudget(contentsPath)
	if err != nil {
		return fmt.Errorf("determine layer contents budget: %w", err)
	}
	budgetedContents, err := download.NewLimitWriter(sparseFileWriter{file: contents}, maxBytes)
	if err != nil {
		return err
	}
	tr := tar.NewReader(src)
	var contentsOffset uint64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read layer tar: %w", err)
		}
		if path.Clean(strings.TrimPrefix(hdr.Name, "/")) == "." {
			continue
		}
		name, err := sanitizeArchivePath(hdr.Name)
		if err != nil {
			return err
		}
		base := path.Base(name)
		dir := path.Dir(name)
		if base == ".wh..wh..opq" {
			if err := writeLayerRecord(indexWriter, layerRecord{
				op:   layerRecordOpaque,
				node: indexedNode{Path: fsmeta.Normalize(dir)},
			}); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			if err := writeLayerRecord(indexWriter, layerRecord{
				op: layerRecordDelete,
				node: indexedNode{
					Path: fsmeta.Normalize(path.Join(dir, strings.TrimPrefix(base, ".wh."))),
				},
			}); err != nil {
				return err
			}
			continue
		}

		node := indexedNode{
			Path:      fsmeta.Normalize(name),
			UID:       uint32(hdr.Uid),
			GID:       uint32(hdr.Gid),
			Mode:      fsmeta.LinuxModeFromTarHeader(hdr),
			Size:      uint64(hdr.Size),
			ModTimeNS: hdr.ModTime.UnixNano(),
			TarOffset: contentsOffset,
			RDev:      uint32(hdr.Devmajor<<8 | hdr.Devminor),
		}
		record := layerRecord{op: layerRecordAdd, node: node}
		switch hdr.Typeflag {
		case tar.TypeDir:
			record.node.Kind = indexedKindDir
			record.node.Size = 0
			record.node.TarOffset = 0
		case tar.TypeReg, tar.TypeRegA:
			record.node.Kind = indexedKindFile
			if _, err := io.CopyN(budgetedContents, tr, hdr.Size); err != nil {
				return fmt.Errorf("write contents for %q: %w", name, err)
			}
			contentsOffset += uint64(hdr.Size)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			record.node.Kind = indexedKindFile
			record.node.Size = 0
			record.node.TarOffset = 0
		case tar.TypeSymlink:
			record.node.Kind = indexedKindSymlink
			record.node.LinkTarget = fsmeta.NormalizeSymlinkTarget(hdr.Linkname)
			record.node.Size = uint64(len(hdr.Linkname))
			record.node.TarOffset = 0
		case tar.TypeLink:
			target, err := sanitizeArchivePath(hdr.Linkname)
			if err != nil {
				return err
			}
			record.op = layerRecordHardlink
			record.node.Kind = indexedKindFile
			record.node.Size = 0
			record.node.TarOffset = 0
			record.hardlinkTarget = fsmeta.Normalize(target)
		case tar.TypeXGlobalHeader:
			continue
		default:
			return fmt.Errorf("unsupported layer entry type %d for %s", hdr.Typeflag, name)
		}
		if err := writeLayerRecord(indexWriter, record); err != nil {
			return err
		}
	}
	// archive/tar stops at the end marker. Drain the decompressor to validate
	// the compressed stream checksum and account for all downloaded bytes.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return err
	}
	if err := contents.Truncate(int64(contentsOffset)); err != nil {
		return err
	}
	if err := indexWriter.Flush(); err != nil {
		return err
	}
	if err := index.Sync(); err != nil {
		return err
	}
	if err := contents.Sync(); err != nil {
		return err
	}
	if progress != nil {
		progress(int64(tracked.n))
	}
	return nil
}

type sparseFileWriter struct {
	file *os.File
}

func (w sparseFileWriter) Write(p []byte) (int, error) {
	allZero := true
	for _, value := range p {
		if value != 0 {
			allZero = false
			break
		}
	}
	if !allZero {
		return w.file.Write(p)
	}
	if _, err := w.file.Seek(int64(len(p)), io.SeekCurrent); err != nil {
		return 0, err
	}
	return len(p), nil
}

func writeLayerRecord(w io.Writer, record layerRecord) error {
	if _, err := w.Write([]byte{record.op}); err != nil {
		return err
	}
	if err := writeLayerString(w, record.node.Path); err != nil {
		return err
	}
	if record.op == layerRecordDelete || record.op == layerRecordOpaque {
		return nil
	}
	var kind byte
	switch record.node.Kind {
	case indexedKindDir:
		kind = layerKindDirectory
	case indexedKindFile:
		kind = layerKindFile
	case indexedKindSymlink:
		kind = layerKindSymlink
	default:
		return fmt.Errorf("unsupported indexed kind %q", record.node.Kind)
	}
	if _, err := w.Write([]byte{kind}); err != nil {
		return err
	}
	for _, value := range []uint64{
		uint64(record.node.Mode),
		uint64(record.node.UID),
		uint64(record.node.GID),
		uint64(record.node.RDev),
		record.node.Size,
		record.node.TarOffset,
	} {
		if err := writeLayerUvarint(w, value); err != nil {
			return err
		}
	}
	if err := writeLayerVarint(w, record.node.ModTimeNS); err != nil {
		return err
	}
	if err := writeLayerString(w, record.node.LinkTarget); err != nil {
		return err
	}
	if record.op == layerRecordHardlink {
		return writeLayerString(w, record.hardlinkTarget)
	}
	return nil
}

func applyLayerArchive(
	indexPath, contentsRef string,
	merged map[string]*indexedNode,
	entries map[string]fsmeta.Entry,
) error {
	file, err := os.Open(indexPath)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	magic := make([]byte, len(layerArchiveMagic))
	if _, err := io.ReadFull(reader, magic); err != nil {
		return err
	}
	if string(magic) != layerArchiveMagic {
		return fmt.Errorf("unsupported layer index format")
	}
	layerEntries := make(map[string]*indexedNode)
	for {
		record, err := readLayerRecord(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read layer index: %w", err)
		}
		switch record.op {
		case layerRecordDelete:
			removeMergedPath(merged, entries, record.node.Path)
			continue
		case layerRecordOpaque:
			for key := range merged {
				if key != record.node.Path &&
					strings.HasPrefix(key, record.node.Path+"/") &&
					layerEntries[key] == nil {
					delete(merged, key)
					if entries != nil {
						delete(entries, key)
					}
				}
			}
			continue
		case layerRecordHardlink:
			target := layerEntries[record.hardlinkTarget]
			if target == nil {
				target = merged[record.hardlinkTarget]
			}
			if target == nil || target.Kind != indexedKindFile {
				return fmt.Errorf("hardlink target %q for %q not found", record.hardlinkTarget, record.node.Path)
			}
			record.node.Size = target.Size
			record.node.TarPath = target.TarPath
			record.node.TarOffset = target.TarOffset
		case layerRecordAdd:
			if record.node.Kind == indexedKindFile && record.node.Size != 0 {
				record.node.TarPath = contentsRef
			}
		default:
			return fmt.Errorf("unknown layer operation %d", record.op)
		}
		node := record.node
		// Descendants can only remain when this path previously represented a
		// directory. Avoid scanning the complete merged namespace for ordinary
		// file updates; large images contain hundreds of thousands of them.
		existing := merged[node.Path]
		if node.Kind != indexedKindDir && existing != nil && existing.Kind == indexedKindDir {
			for key := range merged {
				if strings.HasPrefix(key, node.Path+"/") {
					delete(merged, key)
					delete(layerEntries, key)
					if entries != nil {
						delete(entries, key)
					}
				}
			}
		}
		merged[node.Path] = &node
		layerEntries[node.Path] = &node
		if entries != nil {
			meta := fsmeta.Entry{UID: node.UID, GID: node.GID, Mode: node.Mode, RDev: node.RDev}
			if node.Kind == indexedKindSymlink {
				meta.LinkTarget = node.LinkTarget
			}
			entries[node.Path] = meta
		}
	}
}

func readLayerRecord(r *bufio.Reader) (layerRecord, error) {
	op, err := r.ReadByte()
	if err != nil {
		return layerRecord{}, err
	}
	name, err := readLayerString(r)
	if err != nil {
		return layerRecord{}, err
	}
	record := layerRecord{op: op, node: indexedNode{Path: name}}
	if op == layerRecordDelete || op == layerRecordOpaque {
		return record, nil
	}
	kind, err := r.ReadByte()
	if err != nil {
		return layerRecord{}, err
	}
	switch kind {
	case layerKindDirectory:
		record.node.Kind = indexedKindDir
	case layerKindFile:
		record.node.Kind = indexedKindFile
	case layerKindSymlink:
		record.node.Kind = indexedKindSymlink
	default:
		return layerRecord{}, fmt.Errorf("unknown layer entry kind %d", kind)
	}
	values := make([]uint64, 6)
	for i := range values {
		values[i], err = binary.ReadUvarint(r)
		if err != nil {
			return layerRecord{}, err
		}
	}
	record.node.Mode = uint32(values[0])
	record.node.UID = uint32(values[1])
	record.node.GID = uint32(values[2])
	record.node.RDev = uint32(values[3])
	record.node.Size = values[4]
	record.node.TarOffset = values[5]
	record.node.ModTimeNS, err = binary.ReadVarint(r)
	if err != nil {
		return layerRecord{}, err
	}
	record.node.LinkTarget, err = readLayerString(r)
	if err != nil {
		return layerRecord{}, err
	}
	if op == layerRecordHardlink {
		record.hardlinkTarget, err = readLayerString(r)
	}
	return record, err
}

func writeLayerUvarint(w io.Writer, value uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], value)
	_, err := w.Write(buf[:n])
	return err
}

func writeLayerVarint(w io.Writer, value int64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint(buf[:], value)
	_, err := w.Write(buf[:n])
	return err
}

func writeLayerString(w io.Writer, value string) error {
	if len(value) > maxLayerIndexString {
		return fmt.Errorf("layer index string is too large: %d bytes", len(value))
	}
	if err := writeLayerUvarint(w, uint64(len(value))); err != nil {
		return err
	}
	_, err := io.WriteString(w, value)
	return err
}

func readLayerString(r io.ByteReader) (string, error) {
	size, err := binary.ReadUvarint(r)
	if err != nil {
		return "", err
	}
	if size > maxLayerIndexString {
		return "", fmt.Errorf("layer index string is too large: %d bytes", size)
	}
	buf := make([]byte, int(size))
	reader, ok := r.(io.Reader)
	if !ok {
		return "", fmt.Errorf("layer index reader cannot read strings")
	}
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func linkOrCopyLayerContents(srcPath, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	if dstInfo, err := os.Stat(dstPath); err == nil {
		srcInfo, err := os.Stat(srcPath)
		if err != nil {
			return err
		}
		if os.SameFile(srcInfo, dstInfo) {
			return nil
		}
		return fmt.Errorf("layer contents destination already exists: %s", dstPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(srcPath, dstPath); err == nil {
		return nil
	}
	return copyFile(srcPath, dstPath, fs.FileMode(0o644))
}
