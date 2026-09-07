package oci

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

const fsIndexMagic = "CCFSIDX1"

func encodeBinaryFSIndex(nodes []indexedNode) ([]byte, error) {
	var out bytes.Buffer
	writer := bufio.NewWriterSize(&out, 64<<10)
	if _, err := writer.WriteString(fsIndexMagic); err != nil {
		return nil, err
	}
	if err := writeLayerUvarint(writer, uint64(len(nodes))); err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if err := writeBinaryIndexedNode(writer, node); err != nil {
			return nil, err
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func encodeBinaryFSIndexMap(nodes map[string]*indexedNode) ([]byte, error) {
	paths := make([]string, 0, len(nodes))
	for path := range nodes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var out bytes.Buffer
	writer := bufio.NewWriterSize(&out, 64<<10)
	if _, err := writer.WriteString(fsIndexMagic); err != nil {
		return nil, err
	}
	if err := writeLayerUvarint(writer, uint64(len(paths))); err != nil {
		return nil, err
	}
	for _, path := range paths {
		node := nodes[path]
		if node == nil {
			return nil, fmt.Errorf("filesystem index node %q is nil", path)
		}
		if err := writeBinaryIndexedNode(writer, *node); err != nil {
			return nil, err
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeBinaryIndexedNode(w io.Writer, node indexedNode) error {
	if err := writeLayerString(w, node.Path); err != nil {
		return err
	}
	var kind byte
	switch node.Kind {
	case indexedKindDir:
		kind = layerKindDirectory
	case indexedKindFile:
		kind = layerKindFile
	case indexedKindSymlink:
		kind = layerKindSymlink
	default:
		return fmt.Errorf("unsupported indexed kind %q", node.Kind)
	}
	if _, err := w.Write([]byte{kind}); err != nil {
		return err
	}
	for _, value := range []uint64{
		uint64(node.Mode),
		uint64(node.UID),
		uint64(node.GID),
		uint64(node.RDev),
		node.Size,
		node.TarOffset,
		node.PackedOffset,
	} {
		if err := writeLayerUvarint(w, value); err != nil {
			return err
		}
	}
	if err := writeLayerVarint(w, node.ModTimeNS); err != nil {
		return err
	}
	for _, value := range []string{node.LinkTarget, node.TarPath, node.CVMFSTarget} {
		if err := writeLayerString(w, value); err != nil {
			return err
		}
	}
	packed := byte(0)
	if node.Packed {
		packed = 1
	}
	_, err := w.Write([]byte{packed})
	return err
}

func decodeBinaryFSIndex(data []byte) ([]indexedNode, error) {
	reader := bufio.NewReaderSize(bytes.NewReader(data[len(fsIndexMagic):]), 64<<10)
	count, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, err
	}
	// Each node requires at least a path length and kind byte. This bound also
	// prevents a corrupt index from requesting an unreasonable allocation.
	if count > uint64(len(data)) || count > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("invalid filesystem index node count %d", count)
	}
	nodes := make([]indexedNode, 0, int(count))
	for range count {
		node, err := readBinaryIndexedNode(reader)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("filesystem index has trailing data")
		}
		return nil, err
	}
	return nodes, nil
}

func readBinaryIndexedNode(r *bufio.Reader) (indexedNode, error) {
	var node indexedNode
	var err error
	node.Path, err = readLayerString(r)
	if err != nil {
		return indexedNode{}, err
	}
	kind, err := r.ReadByte()
	if err != nil {
		return indexedNode{}, err
	}
	switch kind {
	case layerKindDirectory:
		node.Kind = indexedKindDir
	case layerKindFile:
		node.Kind = indexedKindFile
	case layerKindSymlink:
		node.Kind = indexedKindSymlink
	default:
		return indexedNode{}, fmt.Errorf("unknown filesystem index kind %d", kind)
	}
	values := make([]uint64, 7)
	for i := range values {
		values[i], err = binary.ReadUvarint(r)
		if err != nil {
			return indexedNode{}, err
		}
	}
	node.Mode = uint32(values[0])
	node.UID = uint32(values[1])
	node.GID = uint32(values[2])
	node.RDev = uint32(values[3])
	node.Size = values[4]
	node.TarOffset = values[5]
	node.PackedOffset = values[6]
	node.ModTimeNS, err = binary.ReadVarint(r)
	if err != nil {
		return indexedNode{}, err
	}
	node.LinkTarget, err = readLayerString(r)
	if err != nil {
		return indexedNode{}, err
	}
	node.TarPath, err = readLayerString(r)
	if err != nil {
		return indexedNode{}, err
	}
	node.CVMFSTarget, err = readLayerString(r)
	if err != nil {
		return indexedNode{}, err
	}
	packed, err := r.ReadByte()
	if err != nil {
		return indexedNode{}, err
	}
	if packed > 1 {
		return indexedNode{}, fmt.Errorf("invalid packed flag %d", packed)
	}
	node.Packed = packed == 1
	return node, nil
}
