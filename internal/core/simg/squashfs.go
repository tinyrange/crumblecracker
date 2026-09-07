package simg

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

const (
	compZlib          = 1
	metaUncompressed  = 0x8000
	dataUncompressed  = 0x01000000
	dataSizeMask      = 0x00ffffff
	fragmentEntrySize = 16
	fragmentPerMeta   = 8192 / fragmentEntrySize

	inodeTypeBasicDir  = 1
	inodeTypeBasicFile = 2
	inodeTypeBasicSym  = 3
	inodeTypeLDir      = 8
	inodeTypeLFile     = 9
	inodeTypeLSym      = 10
)

type Superblock struct {
	Magic             [4]byte
	Inodes            uint32
	MkfsTime          uint32
	BlockSize         uint32
	Fragments         uint32
	Compression       uint16
	BlockLog          uint16
	Flags             uint16
	NoIDs             uint16
	Major             uint16
	Minor             uint16
	RootInode         uint64
	BytesUsed         uint64
	IDTableStart      uint64
	XattrIDTableStart uint64
	InodeTableStart   uint64
	DirectoryTable    uint64
	FragmentTable     uint64
	LookupTable       uint64
}

type Reader struct {
	path         string
	base         int64
	sb           Superblock
	inodeMeta    *metaReader
	dirMeta      *metaReader
	fragmentPtrs []uint64
	fragCache    map[uint32][]byte
	fragMu       sync.Mutex
}

type metaReader struct {
	file  *os.File
	base  int64
	cache map[uint32]metaBlock
}

type metaBlock struct {
	data []byte
	next uint32
}

type metaCursor struct {
	r   *metaReader
	blk uint32
	off int
}

type DirEntry struct {
	Name      string
	InodeRef  uint64
	InodeType uint16
}

type InodeBase struct {
	InodeType uint16
	Mode      uint16
	UID       uint16
	GID       uint16
	MTime     uint32
	InodeNum  uint32
}

type DirInode struct {
	Base       InodeBase
	StartBlock uint32
	Offset     uint16
	SizeBytes  int
}

type FileInode struct {
	Base          InodeBase
	StartBlock    uint64
	FragmentIndex uint32
	FragmentOff   uint32
	FileSize      uint64
	BlockSizes    []uint32
}

type SymlinkInode struct {
	Base   InodeBase
	Target string
}

func OpenSquashFS(image *File) (*Reader, error) {
	base := image.SquashFSOffset
	if base < 0 {
		return nil, errors.New("invalid squashfs offset")
	}
	var sb Superblock
	if _, err := image.F.Seek(base, io.SeekStart); err != nil {
		return nil, err
	}
	if err := binary.Read(image.F, binary.LittleEndian, &sb); err != nil {
		return nil, err
	}
	if string(sb.Magic[:]) != squashFSMagic {
		return nil, fmt.Errorf("invalid squashfs magic at %d", base)
	}
	if sb.Compression != compZlib {
		return nil, fmt.Errorf("unsupported squashfs compression %d (only zlib is supported)", sb.Compression)
	}
	sq := &Reader{
		path: image.Path,
		base: base,
		sb:   sb,
		inodeMeta: &metaReader{
			file:  image.F,
			base:  base + int64(sb.InodeTableStart),
			cache: map[uint32]metaBlock{},
		},
		dirMeta: &metaReader{
			file:  image.F,
			base:  base + int64(sb.DirectoryTable),
			cache: map[uint32]metaBlock{},
		},
		fragCache: map[uint32][]byte{},
	}
	if sb.Fragments > 0 {
		ptrCount := int((sb.Fragments + fragmentPerMeta - 1) / fragmentPerMeta)
		ptrBytes, err := readAtMost(image.F, base+int64(sb.FragmentTable), ptrCount*8)
		if err != nil {
			return nil, err
		}
		sq.fragmentPtrs = make([]uint64, 0, ptrCount)
		for i := 0; i < ptrCount; i++ {
			sq.fragmentPtrs = append(sq.fragmentPtrs, binary.LittleEndian.Uint64(ptrBytes[i*8:(i+1)*8]))
		}
	}
	return sq, nil
}

func BuildImageFS(path string) (imagefs.Directory, map[string]fsmeta.Entry, string, error) {
	img, err := Open(path)
	if err != nil {
		return nil, nil, "", err
	}
	defer img.Close()
	sq, err := OpenSquashFS(img)
	if err != nil {
		return nil, nil, "", err
	}
	root := newSIMGDir(fs.ModeDir|0o755, 0, 0, 0, time.Unix(0, 0))
	entries := map[string]fsmeta.Entry{
		"/": {UID: 0, GID: 0, Mode: fsmeta.LinuxModeFromFileMode(fs.ModeDir | 0o755)},
	}
	if err := sq.populateDir(root, entries, "/", sq.sb.RootInode); err != nil {
		return nil, nil, img.SIFArch(), err
	}
	if root.modTime.IsZero() {
		root.modTime = time.Unix(0, 0)
	}
	namespace, err := imagefs.BuildNamespace(root)
	if err != nil {
		return nil, nil, img.SIFArch(), fmt.Errorf("build image namespace: %w", err)
	}
	attachSIMGNamespace(namespace)
	return root, entries, img.SIFArch(), nil
}

func attachSIMGNamespace(namespace *imagefs.Namespace) {
	for id := 1; id < len(namespace.Nodes); id++ {
		node := namespace.Nodes[id]
		if node == nil || node.Entry.Dir == nil {
			continue
		}
		if directory, ok := node.Entry.Dir.(*simgDir); ok {
			directory.namespace = namespace
			directory.nodeID = uint64(id)
			directory.entries = nil
		}
	}
}

func (f *File) SIFArch() string {
	if f == nil || f.SIF == nil {
		return ""
	}
	return f.SIF.Arch
}

func (sq *Reader) populateDir(dir *simgDir, entries map[string]fsmeta.Entry, dirPath string, inodeRef uint64) error {
	node, err := sq.ReadInode(inodeRef)
	if err != nil {
		return err
	}
	d, ok := node.(*DirInode)
	if !ok {
		return fmt.Errorf("%q is not a directory", dirPath)
	}
	dir.mode = modeFromSquashDir(d.Base.Mode)
	dir.modTime = time.Unix(int64(d.Base.MTime), 0)
	dir.uid = uint32(d.Base.UID)
	dir.gid = uint32(d.Base.GID)
	entries[dirPath] = fsmeta.Entry{UID: dir.uid, GID: dir.gid, Mode: fsmeta.LinuxModeFromFileMode(fs.ModeDir | dir.mode.Perm())}

	children, err := sq.ReadDirectoryEntries(d)
	if err != nil {
		return err
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	for _, child := range children {
		childPath := path.Join(dirPath, child.Name)
		if !strings.HasPrefix(childPath, "/") {
			childPath = "/" + childPath
		}
		inode, err := sq.ReadInode(child.InodeRef)
		if err != nil {
			return err
		}
		switch n := inode.(type) {
		case *DirInode:
			childDir := newSIMGDir(modeFromSquashDir(n.Base.Mode), uint32(n.Base.UID), uint32(n.Base.GID), 0, time.Unix(int64(n.Base.MTime), 0))
			dir.entries[child.Name] = imagefs.Entry{Dir: childDir}
			if err := sq.populateDir(childDir, entries, childPath, child.InodeRef); err != nil {
				return err
			}
		case *FileInode:
			source := &simgFile{
				mode:    modeFromSquashFile(n.Base.Mode),
				uid:     uint32(n.Base.UID),
				gid:     uint32(n.Base.GID),
				modTime: time.Unix(int64(n.Base.MTime), 0),
				source:  sq,
				inode:   *n,
			}
			dir.entries[child.Name] = imagefs.Entry{File: source}
			entries[childPath] = fsmeta.Entry{
				UID:  source.uid,
				GID:  source.gid,
				Mode: fsmeta.LinuxModeFromFileMode(source.mode),
			}
		case *SymlinkInode:
			link := &simgSymlink{
				mode:    modeFromSquashSymlink(n.Base.Mode),
				uid:     uint32(n.Base.UID),
				gid:     uint32(n.Base.GID),
				modTime: time.Unix(int64(n.Base.MTime), 0),
				target:  n.Target,
			}
			dir.entries[child.Name] = imagefs.Entry{Symlink: link}
			entries[childPath] = fsmeta.Entry{
				UID:  link.uid,
				GID:  link.gid,
				Mode: fsmeta.LinuxModeFromFileMode(fs.ModeSymlink | link.mode.Perm()),
			}
		default:
			continue
		}
	}
	return nil
}

func (m *metaReader) readBlock(rel uint32) ([]byte, uint32, error) {
	if blk, ok := m.cache[rel]; ok {
		return blk.data, blk.next, nil
	}
	pos := m.base + int64(rel)
	if _, err := m.file.Seek(pos, io.SeekStart); err != nil {
		return nil, 0, err
	}
	var hdr uint16
	if err := binary.Read(m.file, binary.LittleEndian, &hdr); err != nil {
		return nil, 0, err
	}
	rawLen := int(hdr & 0x7fff)
	raw := make([]byte, rawLen)
	if _, err := io.ReadFull(m.file, raw); err != nil {
		return nil, 0, err
	}
	out := raw
	if hdr&metaUncompressed == 0 {
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, 0, err
		}
		decomp, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, 0, err
		}
		out = decomp
	}
	next := rel + 2 + uint32(rawLen)
	m.cache[rel] = metaBlock{data: out, next: next}
	return out, next, nil
}

func (m *metaReader) readAt(block uint32, offset int, n int) ([]byte, error) {
	c := &metaCursor{r: m, blk: block, off: offset}
	return c.readN(n)
}

func (c *metaCursor) readN(n int) ([]byte, error) {
	if n < 0 {
		return nil, errors.New("negative read")
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		blk, next, err := c.r.readBlock(c.blk)
		if err != nil {
			return nil, err
		}
		if c.off >= len(blk) {
			c.blk = next
			c.off = 0
			continue
		}
		take := n - len(out)
		if left := len(blk) - c.off; left < take {
			take = left
		}
		out = append(out, blk[c.off:c.off+take]...)
		c.off += take
		if c.off >= len(blk) {
			c.blk = next
			c.off = 0
		}
	}
	return out, nil
}

func (sq *Reader) ReadInode(ref uint64) (any, error) {
	blk := uint32((ref >> 16) & 0xffffffff)
	off := int(uint16(ref & 0xffff))
	baseBytes, err := sq.inodeMeta.readAt(blk, off, 16)
	if err != nil {
		return nil, err
	}
	b := InodeBase{
		InodeType: binary.LittleEndian.Uint16(baseBytes[0:2]),
		Mode:      binary.LittleEndian.Uint16(baseBytes[2:4]),
		UID:       binary.LittleEndian.Uint16(baseBytes[4:6]),
		GID:       binary.LittleEndian.Uint16(baseBytes[6:8]),
		MTime:     binary.LittleEndian.Uint32(baseBytes[8:12]),
		InodeNum:  binary.LittleEndian.Uint32(baseBytes[12:16]),
	}
	switch b.InodeType {
	case inodeTypeBasicDir:
		raw, err := sq.inodeMeta.readAt(blk, off, 32)
		if err != nil {
			return nil, err
		}
		sz := int(binary.LittleEndian.Uint16(raw[24:26])) - 3
		if sz < 0 {
			sz = 0
		}
		return &DirInode{Base: b, StartBlock: binary.LittleEndian.Uint32(raw[16:20]), Offset: binary.LittleEndian.Uint16(raw[26:28]), SizeBytes: sz}, nil
	case inodeTypeLDir:
		raw, err := sq.inodeMeta.readAt(blk, off, 40)
		if err != nil {
			return nil, err
		}
		sz := int(binary.LittleEndian.Uint32(raw[20:24])) - 3
		if sz < 0 {
			sz = 0
		}
		return &DirInode{Base: b, StartBlock: binary.LittleEndian.Uint32(raw[24:28]), Offset: binary.LittleEndian.Uint16(raw[34:36]), SizeBytes: sz}, nil
	case inodeTypeBasicFile:
		raw32, err := sq.inodeMeta.readAt(blk, off, 32)
		if err != nil {
			return nil, err
		}
		fileSize := uint64(binary.LittleEndian.Uint32(raw32[28:32]))
		nBlocks := sq.fileBlockCount(fileSize, binary.LittleEndian.Uint32(raw32[20:24]))
		raw, err := sq.inodeMeta.readAt(blk, off, 32+nBlocks*4)
		if err != nil {
			return nil, err
		}
		blocks := make([]uint32, nBlocks)
		for i := 0; i < nBlocks; i++ {
			blocks[i] = binary.LittleEndian.Uint32(raw[32+i*4 : 36+i*4])
		}
		return &FileInode{Base: b, StartBlock: uint64(binary.LittleEndian.Uint32(raw32[16:20])), FragmentIndex: binary.LittleEndian.Uint32(raw32[20:24]), FragmentOff: binary.LittleEndian.Uint32(raw32[24:28]), FileSize: fileSize, BlockSizes: blocks}, nil
	case inodeTypeLFile:
		raw56, err := sq.inodeMeta.readAt(blk, off, 56)
		if err != nil {
			return nil, err
		}
		fileSize := binary.LittleEndian.Uint64(raw56[24:32])
		nBlocks := sq.fileBlockCount(fileSize, binary.LittleEndian.Uint32(raw56[44:48]))
		raw, err := sq.inodeMeta.readAt(blk, off, 56+nBlocks*4)
		if err != nil {
			return nil, err
		}
		blocks := make([]uint32, nBlocks)
		for i := 0; i < nBlocks; i++ {
			blocks[i] = binary.LittleEndian.Uint32(raw[56+i*4 : 60+i*4])
		}
		return &FileInode{Base: b, StartBlock: binary.LittleEndian.Uint64(raw56[16:24]), FragmentIndex: binary.LittleEndian.Uint32(raw56[44:48]), FragmentOff: binary.LittleEndian.Uint32(raw56[48:52]), FileSize: fileSize, BlockSizes: blocks}, nil
	case inodeTypeBasicSym:
		raw24, err := sq.inodeMeta.readAt(blk, off, 24)
		if err != nil {
			return nil, err
		}
		targetLen := int(binary.LittleEndian.Uint32(raw24[20:24]))
		raw, err := sq.inodeMeta.readAt(blk, off, 24+targetLen)
		if err != nil {
			return nil, err
		}
		return &SymlinkInode{Base: b, Target: string(raw[24 : 24+targetLen])}, nil
	case inodeTypeLSym:
		raw28, err := sq.inodeMeta.readAt(blk, off, 28)
		if err != nil {
			return nil, err
		}
		targetLen := int(binary.LittleEndian.Uint32(raw28[20:24]))
		raw, err := sq.inodeMeta.readAt(blk, off, 28+targetLen)
		if err != nil {
			return nil, err
		}
		return &SymlinkInode{Base: b, Target: string(raw[28 : 28+targetLen])}, nil
	default:
		return nil, fmt.Errorf("unsupported inode type %d", b.InodeType)
	}
}

func (sq *Reader) fileBlockCount(size uint64, fragmentIdx uint32) int {
	if sq.sb.BlockSize == 0 {
		return 0
	}
	block := uint64(sq.sb.BlockSize)
	if fragmentIdx == 0xffffffff {
		return int((size + block - 1) / block)
	}
	return int(size / block)
}

func (sq *Reader) ReadDirectoryEntries(d *DirInode) ([]DirEntry, error) {
	entries := make([]DirEntry, 0, 64)
	cursor := &metaCursor{r: sq.dirMeta, blk: d.StartBlock, off: int(d.Offset)}
	remaining := d.SizeBytes
	for remaining > 0 {
		hdr, err := cursor.readN(12)
		if err != nil {
			return nil, err
		}
		remaining -= 12
		count := int(binary.LittleEndian.Uint32(hdr[0:4])) + 1
		startBlock := binary.LittleEndian.Uint32(hdr[4:8])
		for i := 0; i < count; i++ {
			eb, err := cursor.readN(8)
			if err != nil {
				return nil, err
			}
			remaining -= 8
			inodeOff := binary.LittleEndian.Uint16(eb[0:2])
			entryType := binary.LittleEndian.Uint16(eb[4:6])
			nameLen := int(binary.LittleEndian.Uint16(eb[6:8])) + 1
			nameBytes, err := cursor.readN(nameLen)
			if err != nil {
				return nil, err
			}
			remaining -= nameLen
			name := string(nameBytes)
			if name == "." || name == ".." {
				continue
			}
			ref := (uint64(startBlock) << 16) | uint64(inodeOff)
			entries = append(entries, DirEntry{Name: name, InodeRef: ref, InodeType: entryType})
		}
	}
	return entries, nil
}

type simgDir struct {
	mode      fs.FileMode
	uid       uint32
	gid       uint32
	rdev      uint32
	modTime   time.Time
	entries   map[string]imagefs.Entry
	namespace *imagefs.Namespace
	nodeID    uint64
}

func newSIMGDir(mode fs.FileMode, uid, gid, rdev uint32, modTime time.Time) *simgDir {
	return &simgDir{mode: mode, uid: uid, gid: gid, rdev: rdev, modTime: modTime, entries: map[string]imagefs.Entry{}}
}

func (d *simgDir) Stat() fs.FileMode       { return d.mode & fs.ModePerm }
func (d *simgDir) ModTime() time.Time      { return d.modTime }
func (d *simgDir) Owner() (uint32, uint32) { return d.uid, d.gid }
func (d *simgDir) RDev() uint32            { return d.rdev }
func (d *simgDir) Namespace() *imagefs.Namespace {
	return d.namespace
}
func (d *simgDir) ReadDir() ([]imagefs.DirEnt, error) {
	if d.namespace != nil && d.nodeID < uint64(len(d.namespace.Nodes)) {
		node := d.namespace.Nodes[d.nodeID]
		out := make([]imagefs.DirEnt, 0, len(node.Children))
		for name, id := range node.Children {
			out = append(out, simgDirEnt(name, d.namespace.Nodes[id].Entry))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}
	out := make([]imagefs.DirEnt, 0, len(d.entries))
	for name, entry := range d.entries {
		out = append(out, simgDirEnt(name, entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func simgDirEnt(name string, entry imagefs.Entry) imagefs.DirEnt {
	switch {
	case entry.Dir != nil:
		return imagefs.DirEnt{Name: name, Mode: fs.ModeDir | entry.Dir.Stat()}
	case entry.Symlink != nil:
		return imagefs.DirEnt{Name: name, Mode: fs.ModeSymlink | entry.Symlink.Stat()}
	case entry.File != nil:
		_, mode := entry.File.Stat()
		return imagefs.DirEnt{Name: name, Mode: mode}
	default:
		return imagefs.DirEnt{Name: name}
	}
}

func (d *simgDir) Lookup(name string) (imagefs.Entry, error) {
	if d.namespace != nil && d.nodeID < uint64(len(d.namespace.Nodes)) {
		node := d.namespace.Nodes[d.nodeID]
		id, ok := node.Children[name]
		if !ok {
			return imagefs.Entry{}, os.ErrNotExist
		}
		return d.namespace.Nodes[id].Entry, nil
	}
	entry, ok := d.entries[name]
	if !ok {
		return imagefs.Entry{}, os.ErrNotExist
	}
	return entry, nil
}

type simgFile struct {
	mode    fs.FileMode
	uid     uint32
	gid     uint32
	rdev    uint32
	modTime time.Time
	source  *Reader
	inode   FileInode
}

func (f *simgFile) Stat() (uint64, fs.FileMode) { return f.inode.FileSize, f.mode }
func (f *simgFile) ModTime() time.Time          { return f.modTime }
func (f *simgFile) Owner() (uint32, uint32)     { return f.uid, f.gid }
func (f *simgFile) RDev() uint32                { return f.rdev }
func (f *simgFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	return f.source.readFileRange(&f.inode, off, size)
}

type simgSymlink struct {
	mode    fs.FileMode
	uid     uint32
	gid     uint32
	rdev    uint32
	modTime time.Time
	target  string
}

func (l *simgSymlink) Stat() fs.FileMode       { return l.mode & fs.ModePerm }
func (l *simgSymlink) ModTime() time.Time      { return l.modTime }
func (l *simgSymlink) Target() string          { return l.target }
func (l *simgSymlink) Owner() (uint32, uint32) { return l.uid, l.gid }
func (l *simgSymlink) RDev() uint32            { return l.rdev }

func (sq *Reader) readFileRange(fi *FileInode, off uint64, size uint32) ([]byte, error) {
	if off >= fi.FileSize || size == 0 {
		return nil, nil
	}
	end := off + uint64(size)
	if end > fi.FileSize {
		end = fi.FileSize
	}
	out := make([]byte, 0, end-off)
	blockSize := uint64(sq.sb.BlockSize)
	dataPos := fi.StartBlock
	blockStart := uint64(0)
	file, err := os.Open(sq.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	for _, enc := range fi.BlockSizes {
		want := blockSize
		if remaining := fi.FileSize - blockStart; remaining < want {
			want = remaining
		}
		logicalEnd := blockStart + want
		compLen := uint64(enc & dataSizeMask)
		if enc == 0 {
			if overlapStart, overlapEnd, ok := overlap(off, end, blockStart, logicalEnd); ok {
				out = append(out, make([]byte, overlapEnd-overlapStart)...)
			}
		} else {
			if logicalEnd <= off {
				dataPos += compLen
				blockStart = logicalEnd
				continue
			}
			raw, err := readAtMost(file, sq.base+int64(dataPos), int(compLen))
			if err != nil {
				return nil, err
			}
			dataPos += compLen
			decoded, err := decodeDataBlock(raw, enc)
			if err != nil {
				return nil, err
			}
			if uint64(len(decoded)) < want {
				return nil, fmt.Errorf("short decompressed squashfs block")
			}
			if overlapStart, overlapEnd, ok := overlap(off, end, blockStart, logicalEnd); ok {
				start := overlapStart - blockStart
				finish := overlapEnd - blockStart
				out = append(out, decoded[start:finish]...)
			}
		}
		blockStart = logicalEnd
		if blockStart >= end {
			return out, nil
		}
	}
	if end <= blockStart {
		return out, nil
	}
	if fi.FragmentIndex == 0xffffffff {
		return out, nil
	}
	frag, err := sq.readFragment(fi.FragmentIndex)
	if err != nil {
		return nil, err
	}
	fragLogicalEnd := fi.FileSize
	if overlapStart, overlapEnd, ok := overlap(off, end, blockStart, fragLogicalEnd); ok {
		start := int(fi.FragmentOff + uint32(overlapStart-blockStart))
		finish := int(fi.FragmentOff + uint32(overlapEnd-blockStart))
		if start < 0 || finish > len(frag) || start > finish {
			return nil, fmt.Errorf("fragment read out of bounds")
		}
		out = append(out, frag[start:finish]...)
	}
	return out, nil
}

func (sq *Reader) readFragment(index uint32) ([]byte, error) {
	if index >= sq.sb.Fragments {
		return nil, fmt.Errorf("fragment index out of range: %d", index)
	}
	sq.fragMu.Lock()
	if data, ok := sq.fragCache[index]; ok {
		sq.fragMu.Unlock()
		return data, nil
	}
	sq.fragMu.Unlock()

	file, err := os.Open(sq.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	ptrIdx := index / fragmentPerMeta
	entryIdx := index % fragmentPerMeta
	if int(ptrIdx) >= len(sq.fragmentPtrs) {
		return nil, fmt.Errorf("fragment pointer out of range: %d", ptrIdx)
	}
	m := &metaReader{file: file, base: sq.base + int64(sq.fragmentPtrs[ptrIdx]), cache: map[uint32]metaBlock{}}
	blk, _, err := m.readBlock(0)
	if err != nil {
		return nil, err
	}
	entryPos := int(entryIdx) * fragmentEntrySize
	if entryPos+fragmentEntrySize > len(blk) {
		return nil, fmt.Errorf("fragment entry %d missing", index)
	}
	entry := blk[entryPos : entryPos+fragmentEntrySize]
	start := binary.LittleEndian.Uint64(entry[0:8])
	sizeEnc := binary.LittleEndian.Uint32(entry[8:12])
	compLen := uint64(sizeEnc & dataSizeMask)
	raw, err := readAtMost(file, sq.base+int64(start), int(compLen))
	if err != nil {
		return nil, err
	}
	out, err := decodeDataBlock(raw, sizeEnc)
	if err != nil {
		return nil, err
	}
	sq.fragMu.Lock()
	sq.fragCache[index] = out
	sq.fragMu.Unlock()
	return out, nil
}

func decodeDataBlock(raw []byte, sizeEnc uint32) ([]byte, error) {
	if sizeEnc&dataUncompressed != 0 {
		return raw, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func overlap(reqStart, reqEnd, segStart, segEnd uint64) (uint64, uint64, bool) {
	start := max(reqStart, segStart)
	end := min(reqEnd, segEnd)
	return start, end, start < end
}

func modeFromSquashDir(mode uint16) fs.FileMode     { return fs.ModeDir | fs.FileMode(mode&0o7777) }
func modeFromSquashFile(mode uint16) fs.FileMode    { return fs.FileMode(mode & 0o7777) }
func modeFromSquashSymlink(mode uint16) fs.FileMode { return fs.ModeSymlink | fs.FileMode(mode&0o7777) }
