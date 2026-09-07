package ffs

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"sort"
	"strings"
	"time"

	ffsimgvm "github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

type Options struct {
	SizeBytes         int64
	ExtraBytes        int64
	DeterministicTime time.Time
	Layout            Layout
}

type CapacityError struct {
	RequiredBytes  uint64
	AvailableBytes uint64
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("FFS image is too small: requires at least %d bytes, has %d bytes", e.RequiredBytes, e.AvailableBytes)
}

const (
	defaultFFSSize = 64 << 20
	ffsPageSize    = 16 << 10

	ffsSectorSize = 512
	ffsBSize      = 16 << 10
	ffsFSize      = 2 << 10
	ffsFrag       = ffsBSize / ffsFSize
	ffsNSPF       = ffsFSize / ffsSectorSize
	ffsFSBTODB    = 2
	ffsInodeSize  = 128
	ffsInopb      = ffsBSize / ffsInodeSize
	ffsNindir     = ffsBSize / 4
	ffsNDaddr     = 12
	ffsNIaddr     = 3

	ffsSBOFF   = 8192
	ffsPartLBA = 1024
	ffsSBlkNo  = 8
	ffsCBlkNo  = 16
	ffsIBlkNo  = 24
	ffsDBlkNo  = 32
	ffsRootIno = 2
	ffsMaxName = 255
	ffsMaxFPG  = 65536

	ffsMagic = 0x011954
	ffsOkay  = 0x7c269d38
	cgMagic  = 0x090255
	dlMagic  = 0x82564557

	dtFIFO = 1
	dtCHR  = 2
	dtDIR  = 4
	dtBLK  = 6
	dtREG  = 8
	dtLNK  = 10
	dtSOCK = 12

	ifmt   = 0o170000
	ififo  = 0o010000
	ifchr  = 0o020000
	ifdir  = 0o040000
	ifblk  = 0o060000
	ifreg  = 0o100000
	iflnk  = 0o120000
	ifsock = 0o140000
)

type Layout string

const (
	LayoutOpenBSDDisk Layout = ""
	LayoutRaw         Layout = "raw"
)

type Superblock [8192]byte

func (s *Superblock) setU32(off int, v uint32) { binary.LittleEndian.PutUint32(s[off:off+4], v) }
func (s *Superblock) setU64(off int, v uint64) { binary.LittleEndian.PutUint64(s[off:off+8], v) }
func (s *Superblock) SetBlockLayout(dataStart uint32) {
	s.setU32(8, ffsSBlkNo)
	s.setU32(12, ffsCBlkNo)
	s.setU32(16, ffsIBlkNo)
	s.setU32(20, dataStart)
	s.setU32(152, dataStart)
	s.setU32(48, ffsBSize)
	s.setU32(52, ffsFSize)
	s.setU32(56, ffsFrag)
	s.setU32(72, uint32(^uint32(ffsBSize-1)))
	s.setU32(76, uint32(^uint32(ffsFSize-1)))
	s.setU32(80, 14)
	s.setU32(84, 11)
	s.setU32(88, 1)
	s.setU32(92, ffsBSize)
	s.setU32(96, 3)
	s.setU32(100, ffsFSBTODB)
	s.setU32(104, 8192)
	s.setU32(116, ffsNindir)
	s.setU32(120, ffsInopb)
	s.setU32(124, ffsNSPF)
	s.setU32(844, ffsBSize)
	s.setU32(1196, 16384)
	s.setU32(1200, 64)
	s.setU64(1336, uint64(ffsBSize-1))
	s.setU64(1344, uint64(ffsFSize-1))
}
func (s *Superblock) SetSize(fsSize, dsize uint32) {
	s.setU32(36, fsSize)
	s.setU32(40, dsize)
	s.setU64(1080, uint64(fsSize))
	s.setU64(1088, uint64(dsize))
}
func (s *Superblock) SetCylinderGroups(ncg, ipg, fpg, dataStart uint32) {
	sectorsPerCylinder := fpg * ffsNSPF
	cgsize := cylinderGroupSize(ipg, fpg)
	s.setU32(44, ncg)
	s.setU32(132, sectorsPerCylinder)
	s.setU32(160, cgsize)
	s.setU32(168, sectorsPerCylinder)
	s.setU32(172, sectorsPerCylinder)
	s.setU32(176, ncg)
	s.setU32(180, 1)
	s.setU32(184, ipg)
	s.setU32(188, fpg)
	s.setU32(1096, dataStart)
}
func (s *Superblock) SetSummary(ndir, nbfree, nifree, nffree uint32) {
	s.setU32(192, ndir)
	s.setU32(196, nbfree)
	s.setU32(200, nifree)
	s.setU32(204, nffree)
	s.setU64(1008, uint64(ndir))
	s.setU64(1016, uint64(nbfree))
	s.setU64(1024, uint64(nifree))
	s.setU64(1032, uint64(nffree))
}
func (s *Superblock) SetTimeAndID(ts int64) {
	s.setU32(32, uint32(ts))
	s.setU32(144, uint32(ts))
	s.setU32(148, uint32(uint64(ts)^0x5a5a5a5a))
	s.setU64(1072, uint64(ts))
	s.setU32(832, uint32(ts)+ffsOkay)
}
func (s *Superblock) SetOpenBSDDefaults() {
	s.setU32(28, 0xffffffff)
	s.setU32(68, 60)
	s.setU32(128, 0)
	s.setU32(132, 64)
	s.setU32(136, 1)
	s.setU32(140, 0)
	s.setU32(156, ffsFSize)
	s.setU32(160, ffsFSize)
	s.setU32(164, 0)
	s.setU32(168, 64)
	s.setU32(172, 64)
	s[209] = 1
	s[213] = 1
	copy(s[216:217], "/")
	s.setU32(840, 1)
	s.setU64(1000, 8192)
	s.setU32(1320, 60)
	s.setU32(1324, 2)
	s.setU64(1328, 0x000400400402ffff)
	s.setU32(1356, 1)
	s.setU32(1360, 1)
	s.setU32(1372, ffsMagic)
}

func cylinderGroupSize(ipg, fpg uint32) uint32 {
	iusedoff := uint32(0xae)
	freeoff := iusedoff + uint32((ipg+7)/8)
	if freeoff%2 != 0 {
		freeoff++
	}
	nextfreeoff := freeoff + uint32((fpg+7)/8)
	return uint32(roundUp(int64(nextfreeoff), ffsFSize))
}

type UFS1Dinode [ffsInodeSize]byte

func (d *UFS1Dinode) SetMode(mode uint16)      { binary.LittleEndian.PutUint16(d[0:2], mode) }
func (d *UFS1Dinode) SetNlink(n uint16)        { binary.LittleEndian.PutUint16(d[2:4], n) }
func (d *UFS1Dinode) SetDirDepth(depth uint32) { binary.LittleEndian.PutUint32(d[4:8], depth) }
func (d *UFS1Dinode) SetSize(size uint64)      { binary.LittleEndian.PutUint64(d[8:16], size) }
func (d *UFS1Dinode) SetAccessTime(ts uint32)  { binary.LittleEndian.PutUint32(d[16:20], ts) }
func (d *UFS1Dinode) SetModifyTime(ts uint32)  { binary.LittleEndian.PutUint32(d[24:28], ts) }
func (d *UFS1Dinode) SetChangeTime(ts uint32)  { binary.LittleEndian.PutUint32(d[32:36], ts) }
func (d *UFS1Dinode) SetDirectBlock(i int, b uint32) {
	binary.LittleEndian.PutUint32(d[40+i*4:44+i*4], b)
}
func (d *UFS1Dinode) SetIndirectBlock(i int, b uint32) {
	binary.LittleEndian.PutUint32(d[88+i*4:92+i*4], b)
}
func (d *UFS1Dinode) SetInlineSymlink(target string) { copy(d[40:100], target) }
func (d *UFS1Dinode) SetRDev(rdev uint32)            { d.SetDirectBlock(0, rdev) }
func (d *UFS1Dinode) SetBlocks(blocks uint32)        { binary.LittleEndian.PutUint32(d[104:108], blocks) }
func (d *UFS1Dinode) SetGeneration(gen uint32)       { binary.LittleEndian.PutUint32(d[108:112], gen) }
func (d *UFS1Dinode) SetUID(uid uint32)              { binary.LittleEndian.PutUint32(d[112:116], uid) }
func (d *UFS1Dinode) SetGID(gid uint32)              { binary.LittleEndian.PutUint32(d[116:120], gid) }

type CylinderGroup [ffsBSize]byte

func (c *CylinderGroup) setU16(off int, v uint16) { binary.LittleEndian.PutUint16(c[off:off+2], v) }
func (c *CylinderGroup) setU32(off int, v uint32) { binary.LittleEndian.PutUint32(c[off:off+4], v) }
func (c *CylinderGroup) SetHeader(cgx uint32, ts int64, ipg, ndblk uint32, ncyl uint16) {
	c.setU32(4, cgMagic)
	c.setU32(8, uint32(ts))
	c.setU32(12, cgx)
	c.setU16(16, ncyl)
	c.setU16(18, uint16(ipg))
	c.setU32(20, ndblk)
	c.setU32(112, 0)
	c.setU32(116, 0)
	c.setU32(136, uint32(ts))
}
func (c *CylinderGroup) SetSummary(ndir, nbfree, nifree, nffree uint32) {
	c.setU32(24, ndir)
	c.setU32(28, nbfree)
	c.setU32(32, nifree)
	c.setU32(36, nffree)
}
func (c *CylinderGroup) SetRotors(block, frag, ino uint32) {
	c.setU32(40, block)
	c.setU32(44, frag)
	c.setU32(48, ino)
}
func (c *CylinderGroup) SetFragSummary(size, count uint32) {
	if size >= ffsFrag {
		return
	}
	c.setU32(52+int(size)*4, count)
}
func (c *CylinderGroup) SetOffsets(btotoff, boff, iusedoff, freeoff, nextfreeoff uint32) {
	c.setU32(84, btotoff)
	c.setU32(88, boff)
	c.setU32(92, iusedoff)
	c.setU32(96, freeoff)
	c.setU32(100, nextfreeoff)
}
func (c *CylinderGroup) MarkInodeUsed(iusedoff uint32, ino int) {
	c[iusedoff+uint32(ino/8)] |= 1 << uint(ino%8)
}
func (c *CylinderGroup) MarkFragFree(freeoff, frag uint32) {
	c[freeoff+frag/8] |= 1 << uint(frag%8)
}
func (c *CylinderGroup) SetBlockTotals(btotoff, boff, nbfree uint32) {
	c.setU32(int(btotoff), nbfree)
	c.setU16(int(boff), uint16(nbfree))
}

type Disklabel [ffsSectorSize]byte

func (d *Disklabel) setU16(off int, v uint16) { binary.LittleEndian.PutUint16(d[off:off+2], v) }
func (d *Disklabel) setU32(off int, v uint32) { binary.LittleEndian.PutUint32(d[off:off+4], v) }
func (d *Disklabel) SetHeader(totalSectors uint32) {
	d.setU32(0, dlMagic)
	d.setU16(4, 12)
	copy(d[8:24], []byte("vnd device"))
	copy(d[24:40], []byte("fictitious"))
	d.setU32(40, ffsSectorSize)
	d.setU32(44, 100)
	d.setU32(48, 1)
	d.setU32(52, (totalSectors+99)/100)
	d.setU32(56, 100)
	d.setU32(60, totalSectors)
	d.setU32(80, ffsPartLBA)
	d.setU32(84, totalSectors)
	d.setU32(92, totalSectors)
	d.setU16(114, 1)
	d.setU32(132, dlMagic)
	d.setU16(138, 16)
}
func (d *Disklabel) SetPartition(index int, size, offset uint32, fstype, fragblock byte, cpg uint16) {
	part := 148 + index*16
	d.setU32(part, size)
	d.setU32(part+4, offset)
	d[part+12] = fstype
	d[part+13] = fragblock
	d.setU16(part+14, cpg)
}
func (d *Disklabel) UpdateChecksum() {
	d.setU16(136, 0)
	var sum uint16
	for off := 0; off < 148+16*16; off += 2 {
		sum ^= binary.LittleEndian.Uint16(d[off : off+2])
	}
	d.setU16(136, sum)
}

type MBR [ffsSectorSize]byte

func (m *MBR) SetOpenBSDPartition(start, sectors uint32) {
	part := m[446+3*16 : 446+4*16]
	part[0] = 0x80
	part[1] = 0x00
	part[2] = 0x01
	part[3] = 0x10
	part[4] = 0xa6
	part[5] = 0xfe
	part[6] = 0xff
	part[7] = 0xff
	binary.LittleEndian.PutUint32(part[8:12], start)
	binary.LittleEndian.PutUint32(part[12:16], sectors)
	m[510] = 0x55
	m[511] = 0xaa
}

type Direct struct {
	buf []byte
}

func (d Direct) SetIno(ino uint32)   { binary.LittleEndian.PutUint32(d.buf[0:4], ino) }
func (d Direct) SetReclen(n uint16)  { binary.LittleEndian.PutUint16(d.buf[4:6], n) }
func (d Direct) SetType(typ uint8)   { d.buf[6] = typ }
func (d Direct) SetName(name string) { d.buf[7] = byte(len(name)); copy(d.buf[8:], name) }

type ffsStats struct {
	files uint64
	dirs  uint64
	links uint64
	devs  uint64
	bytes uint64
}

type ffsNode struct {
	name       string
	ino        uint32
	mode       uint16
	uid        uint32
	gid        uint32
	rdev       uint32
	modTime    time.Time
	size       uint64
	depth      uint32
	file       imagefs.File
	target     string
	parent     *ffsNode
	children   []*ffsNode
	blocks     []uint32
	blockFrags []uint32
	indir      [ffsNIaddr]uint32
	indir2     []uint32
	nlink      int
}

type ffsBuilder struct {
	ctx       context.Context
	root      imagefs.Directory
	vm        *ffsimgvm.VirtualMemory
	size      int64
	fsOffset  int64
	fsSize    int64
	fsFrags   uint32
	cgFrags   uint32
	cgCount   uint32
	ipg       uint32
	dataStart uint32
	nextIno   uint32
	nextFrag  uint32
	usedFrags []bool
	usedInos  []bool
	now       int64
	hardlinks map[string]*ffsNode
	assigned  map[uint32]bool
	written   map[uint32]bool
}

func Build(ctx context.Context, root imagefs.Directory, opts Options) (*ffsimgvm.VirtualMemory, error) {
	if root == nil {
		return nil, fmt.Errorf("root filesystem is required")
	}
	stats, err := scanFFS(ctx, root, "/")
	if err != nil {
		return nil, err
	}
	size := opts.SizeBytes
	if size == 0 {
		size = estimateFFSSize(stats, opts.ExtraBytes)
	}
	fsSize := roundUp(size, ffsBSize)
	if fsSize < ffsDBlkNo*ffsFSize+ffsBSize {
		fsSize = ffsDBlkNo*ffsFSize + ffsBSize
	}
	if fsSize/ffsFSize > math.MaxUint32 {
		return nil, fmt.Errorf("FFS image is too large: %d bytes", fsSize)
	}
	fsOffset := int64(ffsPartLBA * ffsSectorSize)
	totalSize := fsOffset + fsSize
	switch opts.Layout {
	case LayoutOpenBSDDisk:
	case LayoutRaw:
		fsOffset = 0
		totalSize = fsSize
	default:
		return nil, fmt.Errorf("unsupported FFS layout %q", opts.Layout)
	}
	cgFrags := uint32(fsSize / ffsFSize)
	if cgFrags > ffsMaxFPG {
		cgFrags = ffsMaxFPG
	}
	cgCount := uint32((fsSize/ffsFSize + int64(cgFrags) - 1) / int64(cgFrags))
	b := &ffsBuilder{
		ctx:       ctx,
		root:      root,
		vm:        ffsimgvm.NewVirtualMemory(totalSize, ffsPageSize),
		size:      totalSize,
		fsOffset:  fsOffset,
		fsSize:    fsSize,
		fsFrags:   uint32(fsSize / ffsFSize),
		cgFrags:   cgFrags,
		cgCount:   cgCount,
		nextIno:   ffsRootIno,
		nextFrag:  ffsDBlkNo,
		usedFrags: make([]bool, uint32(fsSize/ffsFSize)),
		now:       time.Now().Unix(),
		hardlinks: make(map[string]*ffsNode),
		assigned:  make(map[uint32]bool),
		written:   make(map[uint32]bool),
	}
	if !opts.DeterministicTime.IsZero() {
		b.now = opts.DeterministicTime.Unix()
	}
	inodesNeeded := uint32(stats.files + stats.dirs + stats.links + stats.devs + 8)
	ipg := uint32(roundUp(int64(inodesNeeded), ffsInopb))
	if ipg < ffsInopb {
		ipg = ffsInopb
	}
	b.ipg = ipg
	inodeFrags := (ipg * ffsInodeSize) / ffsFSize
	dataStart := roundUpFrag(ffsIBlkNo+inodeFrags, ffsFrag)
	if dataStart >= b.cgFrags {
		return nil, fmt.Errorf("too many inodes for FFS image: %d", ipg)
	}
	b.dataStart = dataStart
	b.nextFrag = roundUpFrag(dataStart+1, ffsFrag)
	b.usedInos = make([]bool, ipg*cgCount)
	b.usedInos[0] = true
	b.usedInos[1] = true
	for cgx := uint32(0); cgx < b.cgCount; cgx++ {
		base, end := b.cgRange(cgx)
		start := base
		if cgx != 0 {
			start += ffsSBlkNo
		}
		for frag := start; frag < base+b.dataStart && frag < end; frag++ {
			b.usedFrags[frag] = true
		}
	}
	if b.dataStart < b.fsFrags {
		b.usedFrags[b.dataStart] = true
	}

	rootNode, err := b.collectDir(root, "/", nil, "")
	if err != nil {
		return nil, err
	}
	if err := b.assignData(rootNode); err != nil {
		return nil, err
	}
	if err := b.writeTree(rootNode); err != nil {
		return nil, err
	}
	if opts.Layout == LayoutOpenBSDDisk {
		b.writeMBR()
		b.writeDisklabel()
	}
	b.writeSuperblocks(rootNode)
	b.writeCylinderGroups(rootNode)
	return b.vm, nil
}

func Write(ctx context.Context, w io.Writer, root imagefs.Directory, opts Options) error {
	vm, err := Build(ctx, root, opts)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, io.NewSectionReader(vm, 0, vm.Size()))
	return err
}

func scanFFS(ctx context.Context, dir imagefs.Directory, guestPath string) (ffsStats, error) {
	if err := ctx.Err(); err != nil {
		return ffsStats{}, err
	}
	stats := ffsStats{dirs: 1}
	children, err := dir.ReadDir()
	if err != nil {
		return ffsStats{}, fmt.Errorf("read %s: %w", guestPath, err)
	}
	for _, child := range sortedImageDirEnts(children) {
		if child.Name == "." || child.Name == ".." {
			continue
		}
		childPath := path.Join(guestPath, child.Name)
		entry, err := dir.Lookup(child.Name)
		if err != nil {
			return ffsStats{}, fmt.Errorf("lookup %s: %w", childPath, err)
		}
		switch {
		case entry.Dir != nil:
			childStats, err := scanFFS(ctx, entry.Dir, childPath)
			if err != nil {
				return ffsStats{}, err
			}
			stats.files += childStats.files
			stats.dirs += childStats.dirs
			stats.links += childStats.links
			stats.devs += childStats.devs
			stats.bytes += childStats.bytes
		case entry.Symlink != nil:
			stats.links++
			stats.bytes += uint64(len(entry.Symlink.Target()))
		case entry.File != nil:
			size, mode := entry.File.Stat()
			if mode&fs.ModeDevice != 0 || mode&fs.ModeCharDevice != 0 {
				stats.devs++
				continue
			}
			if mode.Type() != 0 {
				return ffsStats{}, fmt.Errorf("%s has unsupported file type %s for FFS filesystem", childPath, mode.Type())
			}
			stats.files++
			stats.bytes += size
		default:
			return ffsStats{}, fmt.Errorf("%s has no filesystem entry", childPath)
		}
	}
	return stats, nil
}

func (b *ffsBuilder) collectDir(dir imagefs.Directory, guestPath string, parent *ffsNode, name string) (*ffsNode, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	mode := ifdir | uint16(dir.Stat().Perm())
	uid, gid := dir.Owner()
	depth := uint32(0)
	if parent != nil {
		depth = parent.depth + 1
	}
	node := &ffsNode{name: name, ino: b.allocIno(), mode: mode, uid: uid, gid: gid, parent: parent, modTime: dir.ModTime(), depth: depth}
	children, err := dir.ReadDir()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", guestPath, err)
	}
	for _, child := range sortedImageDirEnts(children) {
		if child.Name == "." || child.Name == ".." {
			continue
		}
		childPath := path.Join(guestPath, child.Name)
		entry, err := dir.Lookup(child.Name)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", childPath, err)
		}
		childNode, err := b.collectEntry(entry, childPath, node, child.Name)
		if err != nil {
			return nil, err
		}
		node.children = append(node.children, childNode)
	}
	return node, nil
}

func (b *ffsBuilder) collectEntry(entry imagefs.Entry, guestPath string, parent *ffsNode, name string) (*ffsNode, error) {
	if len(name) > ffsMaxName {
		return nil, fmt.Errorf("%s name is too long for FFS", guestPath)
	}
	switch {
	case entry.Dir != nil:
		return b.collectDir(entry.Dir, guestPath, parent, name)
	case entry.Symlink != nil:
		uid, gid := entry.Symlink.Owner()
		target := entry.Symlink.Target()
		return &ffsNode{name: name, ino: b.allocIno(), mode: iflnk | uint16(entry.Symlink.Stat().Perm()), uid: uid, gid: gid, target: target, parent: parent, size: uint64(len(target)), modTime: entry.Symlink.ModTime()}, nil
	case entry.File != nil:
		size, mode := entry.File.Stat()
		uid, gid := entry.File.Owner()
		if hardlink, ok := entry.File.(imagefs.HardlinkFile); ok {
			if key := strings.TrimSpace(hardlink.HardlinkKey()); key != "" {
				if master := b.hardlinks[key]; master != nil {
					master.nlink++
					return &ffsNode{name: name, ino: master.ino, mode: master.mode, uid: master.uid, gid: master.gid, rdev: master.rdev, file: master.file, parent: parent, size: master.size, modTime: master.modTime, nlink: master.nlink}, nil
				}
				node := &ffsNode{name: name, ino: b.allocIno(), mode: goModeToFFS(mode), uid: uid, gid: gid, rdev: entry.File.RDev(), file: entry.File, parent: parent, size: size, modTime: entry.File.ModTime(), nlink: 1}
				if node.mode&ifmt == ifreg && mode.Type() != 0 {
					return nil, fmt.Errorf("%s has unsupported file type %s for FFS filesystem", guestPath, mode.Type())
				}
				b.hardlinks[key] = node
				return node, nil
			}
		}
		node := &ffsNode{name: name, ino: b.allocIno(), mode: goModeToFFS(mode), uid: uid, gid: gid, rdev: entry.File.RDev(), file: entry.File, parent: parent, size: size, modTime: entry.File.ModTime()}
		if node.mode&ifmt == ifreg && mode.Type() != 0 {
			return nil, fmt.Errorf("%s has unsupported file type %s for FFS filesystem", guestPath, mode.Type())
		}
		return node, nil
	default:
		return nil, fmt.Errorf("%s has no filesystem entry", guestPath)
	}
}

func (b *ffsBuilder) allocIno() uint32 {
	ino := b.nextIno
	b.nextIno++
	if int(ino) < len(b.usedInos) {
		b.usedInos[ino] = true
	}
	return ino
}

func (b *ffsBuilder) assignData(node *ffsNode) error {
	if b.assigned[node.ino] {
		return nil
	}
	b.assigned[node.ino] = true
	switch node.mode & ifmt {
	case ifdir:
		data := encodeFFSDir(node)
		node.size = uint64(len(data))
		blocks, frags, err := b.allocBlocksForSize(uint64(len(data)))
		if err != nil {
			return fmt.Errorf("allocate directory %q: %w", node.name, err)
		}
		node.blocks, node.blockFrags = blocks, frags
	case ifreg:
		blocks, frags, err := b.allocBlocksForSize(node.size)
		if err != nil {
			return fmt.Errorf("allocate file %q: %w", node.name, err)
		}
		node.blocks, node.blockFrags = blocks, frags
	case iflnk:
		if len(node.target) > 60 {
			blocks, frags, err := b.allocBlocksForSize(uint64(len(node.target)))
			if err != nil {
				return fmt.Errorf("allocate symlink %q: %w", node.name, err)
			}
			node.blocks, node.blockFrags = blocks, frags
		}
	}
	if err := b.allocIndirectBlocks(node); err != nil {
		return err
	}
	for _, child := range node.children {
		if err := b.assignData(child); err != nil {
			return err
		}
	}
	return nil
}

func (b *ffsBuilder) allocIndirectBlocks(node *ffsNode) error {
	blocks := len(node.blocks)
	if blocks <= ffsNDaddr {
		return nil
	}
	remaining := blocks - ffsNDaddr
	block, err := b.allocBlock()
	if err != nil {
		return fmt.Errorf("allocate single-indirect block for %q: %w", node.name, err)
	}
	node.indir[0] = block
	if remaining <= ffsNindir {
		return nil
	}
	remaining -= ffsNindir
	block, err = b.allocBlock()
	if err != nil {
		return fmt.Errorf("allocate double-indirect root for %q: %w", node.name, err)
	}
	node.indir[1] = block
	count := (remaining + ffsNindir - 1) / ffsNindir
	if count > ffsNindir {
		return fmt.Errorf("%s is too large for FFS double-indirect writer: %d blocks", node.name, blocks)
	}
	node.indir2 = make([]uint32, count)
	for i := range node.indir2 {
		block, err := b.allocBlock()
		if err != nil {
			return fmt.Errorf("allocate double-indirect block %d for %q: %w", i, node.name, err)
		}
		node.indir2[i] = block
	}
	return nil
}

func (b *ffsBuilder) allocBlocksForSize(size uint64) ([]uint32, []uint32, error) {
	if size == 0 {
		return nil, nil, nil
	}
	if size > uint64(b.fsSize) {
		return nil, nil, &CapacityError{RequiredBytes: size, AvailableBytes: uint64(b.fsSize)}
	}
	blockCount := (size-1)/ffsBSize + 1
	if blockCount > uint64(math.MaxInt) {
		return nil, nil, fmt.Errorf("FFS block count is too large: %d", blockCount)
	}
	if blockCount > ffsNDaddr {
		count := int(blockCount)
		blocks := make([]uint32, 0, count)
		frags := make([]uint32, 0, count)
		for i := 0; i < count; i++ {
			block, err := b.allocBlock()
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, block)
			frags = append(frags, ffsFrag)
		}
		return blocks, frags, nil
	}
	fullBlocks := int(size / ffsBSize)
	remainder := size % ffsBSize
	count := fullBlocks
	if remainder != 0 {
		count++
	}
	blocks := make([]uint32, 0, count)
	frags := make([]uint32, 0, count)
	for i := 0; i < fullBlocks; i++ {
		block, err := b.allocBlock()
		if err != nil {
			return nil, nil, err
		}
		blocks = append(blocks, block)
		frags = append(frags, ffsFrag)
	}
	if remainder != 0 {
		needed := uint32(roundUp(int64(remainder), ffsFSize) / ffsFSize)
		frag, err := b.allocFragRun(needed)
		if err != nil {
			return nil, nil, err
		}
		blocks = append(blocks, frag)
		frags = append(frags, needed)
	}
	return blocks, frags, nil
}

func (b *ffsBuilder) allocBlock() (uint32, error) {
	return b.allocFragRun(ffsFrag)
}

func (b *ffsBuilder) allocFragRun(count uint32) (uint32, error) {
	if count == 0 || count > ffsFrag {
		return 0, fmt.Errorf("invalid FFS fragment allocation: %d fragments", count)
	}
	frag := (uint64(b.nextFrag) + ffsFrag - 1) / ffsFrag * ffsFrag
	available := uint64(len(b.usedFrags))
	for {
		if frag%ffsFrag+uint64(count) > ffsFrag {
			frag = (frag + ffsFrag - 1) / ffsFrag * ffsFrag
		}
		end := frag + uint64(count)
		if end > available {
			return 0, &CapacityError{
				RequiredBytes:  end * ffsFSize,
				AvailableBytes: available * ffsFSize,
			}
		}
		ok := true
		for i := frag; i < end; i++ {
			if b.usedFrags[int(i)] {
				ok = false
				break
			}
		}
		if ok {
			for i := frag; i < end; i++ {
				b.usedFrags[int(i)] = true
			}
			b.nextFrag = uint32(end)
			return uint32(frag), nil
		}
		frag++
	}
}

func (b *ffsBuilder) writeTree(node *ffsNode) error {
	if !b.written[node.ino] {
		b.written[node.ino] = true
		if node.mode&ifmt == ifdir {
			if err := b.writeNodeData(node, encodeFFSDir(node)); err != nil {
				return err
			}
		}
		if node.mode&ifmt == ifreg {
			if err := b.writeFileData(node); err != nil {
				return err
			}
		}
		if node.mode&ifmt == iflnk && len(node.target) > 60 {
			if err := b.writeNodeData(node, []byte(node.target)); err != nil {
				return err
			}
		}
		b.writeInode(node)
	}
	for _, child := range node.children {
		if err := b.writeTree(child); err != nil {
			return err
		}
	}
	return nil
}

func (b *ffsBuilder) writeFileData(node *ffsNode) error {
	region := imageFileRegion{file: node.file, size: int64(node.size)}
	for i, block := range node.blocks {
		srcOff := int64(i * ffsBSize)
		size := int64(ffsBSize)
		if remaining := int64(node.size) - srcOff; remaining < size {
			size = remaining
		}
		if size <= 0 {
			break
		}
		blockRegion := ffsimgvm.NewTruncatedRegion(ffsimgvm.NewOffsetRegion(region, srcOff), size)
		if err := b.vm.Map(blockRegion, b.fsOffset+int64(block)*ffsFSize); err != nil {
			return err
		}
	}
	return nil
}

type imageFileRegion struct {
	file imagefs.File
	size int64
}

func (r imageFileRegion) Size() int64 { return r.size }

func (r imageFileRegion) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("off < 0")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if int64(len(p)) > r.size-off {
		p = p[:int(r.size-off)]
	}
	data, err := r.file.ReadAt(uint64(off), uint32(len(p)))
	if len(data) > len(p) {
		data = data[:len(p)]
	}
	return copy(p, data), err
}

func (r imageFileRegion) WriteAt([]byte, int64) (int, error) {
	return 0, fmt.Errorf("region is read only")
}

func (b *ffsBuilder) writeNodeData(node *ffsNode, data []byte) error {
	for i, block := range node.blocks {
		start := i * ffsBSize
		end := start + ffsBSize
		if end > len(data) {
			end = len(data)
		}
		buf := make([]byte, ffsBSize)
		copy(buf, data[start:end])
		if _, err := b.vm.WriteAt(buf, b.fsOffset+int64(block)*ffsFSize); err != nil {
			return err
		}
	}
	return nil
}

func (b *ffsBuilder) writeInode(node *ffsNode) {
	var ino UFS1Dinode
	ino.SetMode(node.mode)
	ino.SetNlink(uint16(b.linkCount(node)))
	if node.mode&ifmt == ifdir {
		ino.SetDirDepth(node.depth)
	}
	ino.SetSize(node.size)
	t := uint32(b.now)
	if !node.modTime.IsZero() {
		t = uint32(node.modTime.Unix())
	}
	ino.SetAccessTime(t)
	ino.SetModifyTime(t)
	ino.SetChangeTime(t)
	if node.mode&ifmt == iflnk && len(node.target) <= 60 {
		ino.SetInlineSymlink(node.target)
	} else if node.mode&ifmt == ifchr || node.mode&ifmt == ifblk {
		ino.SetRDev(node.rdev)
	} else {
		for i := 0; i < len(node.blocks) && i < ffsNDaddr; i++ {
			ino.SetDirectBlock(i, node.blocks[i])
		}
		for i, block := range node.indir {
			if block != 0 {
				ino.SetIndirectBlock(i, block)
			}
		}
		b.writeIndirectBlocks(node)
	}
	ino.SetBlocks(b.allocatedSectors(node))
	ino.SetGeneration(node.ino*1103515245 + 12345)
	ino.SetUID(node.uid)
	ino.SetGID(node.gid)
	_, _ = b.vm.WriteAt(ino[:], b.inodeOffset(node.ino))
}

func (b *ffsBuilder) writeIndirectBlocks(node *ffsNode) {
	if node.indir[0] != 0 {
		var indirect [ffsBSize]byte
		start := ffsNDaddr
		end := start + ffsNindir
		if end > len(node.blocks) {
			end = len(node.blocks)
		}
		for i, block := range node.blocks[start:end] {
			binary.LittleEndian.PutUint32(indirect[i*4:i*4+4], block)
		}
		_, _ = b.vm.WriteAt(indirect[:], b.fsOffset+int64(node.indir[0])*ffsFSize)
	}
	if node.indir[1] != 0 {
		var root [ffsBSize]byte
		for i, block := range node.indir2 {
			binary.LittleEndian.PutUint32(root[i*4:i*4+4], block)
		}
		_, _ = b.vm.WriteAt(root[:], b.fsOffset+int64(node.indir[1])*ffsFSize)

		start := ffsNDaddr + ffsNindir
		for i, indirectBlock := range node.indir2 {
			var indirect [ffsBSize]byte
			blockStart := start + i*ffsNindir
			blockEnd := blockStart + ffsNindir
			if blockEnd > len(node.blocks) {
				blockEnd = len(node.blocks)
			}
			for j, block := range node.blocks[blockStart:blockEnd] {
				binary.LittleEndian.PutUint32(indirect[j*4:j*4+4], block)
			}
			_, _ = b.vm.WriteAt(indirect[:], b.fsOffset+int64(indirectBlock)*ffsFSize)
		}
	}
}

func (b *ffsBuilder) indirectBlockCount(node *ffsNode) int {
	n := 0
	for _, block := range node.indir {
		if block != 0 {
			n++
		}
	}
	n += len(node.indir2)
	return n
}

func (b *ffsBuilder) allocatedSectors(node *ffsNode) uint32 {
	var frags uint32
	for _, count := range node.blockFrags {
		frags += count
	}
	frags += uint32(b.indirectBlockCount(node)) * ffsFrag
	return frags * ffsNSPF
}

func (b *ffsBuilder) linkCount(node *ffsNode) int {
	if node.mode&ifmt != ifdir {
		if node.nlink > 0 {
			return node.nlink
		}
		return 1
	}
	n := 2
	for _, child := range node.children {
		if child.mode&ifmt == ifdir {
			n++
		}
	}
	return n
}

func (b *ffsBuilder) writeSuperblocks(rootNode *ffsNode) {
	var sb Superblock
	fsSize := b.fsFrags
	dsize := b.dataFragCount()
	nbfree, nffree := b.freeSummary()
	ndir := uint32(countDirNodes(rootNode))
	nifree := uint32(len(b.usedInos)) - uint32(countUsedInos(b.usedInos))
	sb.SetBlockLayout(b.dataStart)
	sb.SetOpenBSDDefaults()
	sb.SetTimeAndID(b.now)
	sb.SetSize(fsSize, dsize)
	sb.SetCylinderGroups(b.cgCount, b.ipg, b.cgFrags, b.dataStart)
	sb.SetSummary(ndir, nbfree, nifree, nffree)
	_, _ = b.vm.WriteAt(sb[:], b.fsOffset+ffsSBOFF)
	for cgx := uint32(0); cgx < b.cgCount; cgx++ {
		base, end := b.cgRange(cgx)
		if base+ffsSBlkNo+uint32(len(sb))/ffsFSize <= end {
			_, _ = b.vm.WriteAt(sb[:], b.fsOffset+fragByteOffset(base+ffsSBlkNo))
		}
	}
	b.writeCylinderSummary(rootNode)
}

func (b *ffsBuilder) writeCylinderSummary(rootNode *ffsNode) {
	var summary [ffsFSize]byte
	for cgx := uint32(0); cgx < b.cgCount; cgx++ {
		off := int(cgx * 16)
		if off+16 > len(summary) {
			break
		}
		base, cgFrags := b.cgBaseAndFrags(cgx)
		nbfree, nffree := b.freeSummaryRange(base, base+cgFrags)
		nifree := b.ipg - uint32(countUsedInos(b.usedInosForCG(cgx)))
		ndir := uint32(countDirNodesInCG(rootNode, b.ipg, cgx))
		binary.LittleEndian.PutUint32(summary[off:off+4], ndir)
		binary.LittleEndian.PutUint32(summary[off+4:off+8], nbfree)
		binary.LittleEndian.PutUint32(summary[off+8:off+12], nifree)
		binary.LittleEndian.PutUint32(summary[off+12:off+16], nffree)
	}
	_, _ = b.vm.WriteAt(summary[:], b.fsOffset+fragByteOffset(b.dataStart))
}

func (b *ffsBuilder) writeCylinderGroups(rootNode *ffsNode) {
	for cgx := uint32(0); cgx < b.cgCount; cgx++ {
		b.writeCylinderGroup(cgx, rootNode)
	}
}

func (b *ffsBuilder) writeCylinderGroup(cgx uint32, rootNode *ffsNode) {
	var cg CylinderGroup
	base, cgFrags := b.cgBaseAndFrags(cgx)
	nbfree, nffree := b.freeSummaryRange(base, base+cgFrags)
	ndir := uint32(countDirNodesInCG(rootNode, b.ipg, cgx))
	btotoff := uint32(0xa8)
	boff := uint32(0xac)
	iusedoff := uint32(0xae)
	freeoff := iusedoff + uint32((b.ipg+7)/8)
	if freeoff%2 != 0 {
		freeoff++
	}
	nextfreeoff := freeoff + uint32((b.cgFrags+7)/8)
	ncyl := uint16(1)
	if cgx == b.cgCount-1 && cgFrags < b.cgFrags {
		ncyl = uint16((cgFrags + b.cgFrags - 1) / b.cgFrags)
		if ncyl == 0 {
			ncyl = 1
		}
	}
	cg.SetHeader(cgx, b.now, b.ipg, cgFrags, ncyl)
	cg.SetSummary(ndir, nbfree, b.ipg-uint32(countUsedInos(b.usedInosForCG(cgx))), nffree)
	cg.SetRotors(b.dataStart, b.dataStart, b.nextIno%b.ipg)
	cg.SetOffsets(btotoff, boff, iusedoff, freeoff, nextfreeoff)
	for ino, used := range b.usedInosForCG(cgx) {
		if used {
			cg.MarkInodeUsed(iusedoff, ino)
		}
	}
	for frag := uint32(0); frag < cgFrags; frag++ {
		if !b.usedFrags[base+frag] {
			cg.MarkFragFree(freeoff, frag)
		}
	}
	for size, count := range b.freeFragmentRunsRange(base, base+cgFrags) {
		cg.SetFragSummary(uint32(size), count)
	}
	cg.SetBlockTotals(btotoff, boff, nbfree)
	_, _ = b.vm.WriteAt(cg[:], b.fsOffset+fragByteOffset(base+ffsCBlkNo))
}

func (b *ffsBuilder) freeFragmentRunsRange(start, limit uint32) [ffsFrag]uint32 {
	var out [ffsFrag]uint32
	for frag := roundUpFrag(start, ffsFrag); frag < limit; frag += ffsFrag {
		blockEnd := frag + ffsFrag
		if blockEnd > limit {
			blockEnd = limit
		}
		allFree := true
		for i := frag; i < blockEnd; i++ {
			if b.usedFrags[i] {
				allFree = false
				break
			}
		}
		if allFree {
			continue
		}
		for i := frag; i < blockEnd; {
			if b.usedFrags[i] {
				i++
				continue
			}
			start := i
			for i < blockEnd && !b.usedFrags[i] {
				i++
			}
			if run := i - start; run < ffsFrag {
				out[run]++
			}
		}
	}
	return out
}

func (b *ffsBuilder) writeDisklabel() {
	var label Disklabel
	totalSectors := uint32(b.size / ffsSectorSize)
	fsSectors := uint32(b.fsSize / ffsSectorSize)
	label.SetHeader(totalSectors)
	label.SetPartition(0, fsSectors, ffsPartLBA, 7, byte(((15-13)<<3)|4), 16)
	label.SetPartition(2, totalSectors, 0, 0, 0, 0)
	label.UpdateChecksum()
	_, _ = b.vm.WriteAt(label[:], b.fsOffset+ffsSectorSize)
}

func (b *ffsBuilder) writeMBR() {
	var mbr MBR
	sectors := uint32(b.fsSize / ffsSectorSize)
	mbr.SetOpenBSDPartition(ffsPartLBA, sectors)
	_, _ = b.vm.WriteAt(mbr[:], 0)
}

func (b *ffsBuilder) freeSummary() (uint32, uint32) {
	var nbfree, nffree uint32
	for cgx := uint32(0); cgx < b.cgCount; cgx++ {
		base, cgFrags := b.cgBaseAndFrags(cgx)
		cgNbfree, cgNffree := b.freeSummaryRange(base, base+cgFrags)
		nbfree += cgNbfree
		nffree += cgNffree
	}
	return nbfree, nffree
}

func (b *ffsBuilder) freeSummaryRange(start, limit uint32) (uint32, uint32) {
	var nbfree, nffree uint32
	for frag := roundUpFrag(start, ffsFrag); frag+ffsFrag <= limit; frag += ffsFrag {
		free := true
		for i := uint32(0); i < ffsFrag; i++ {
			if b.usedFrags[frag+i] {
				free = false
				break
			}
		}
		if free {
			nbfree++
		} else {
			for i := uint32(0); i < ffsFrag; i++ {
				if !b.usedFrags[frag+i] {
					nffree++
				}
			}
		}
	}
	return nbfree, nffree
}

func (b *ffsBuilder) dataFragCount() uint32 {
	var out uint32
	for cgx := uint32(0); cgx < b.cgCount; cgx++ {
		_, cgFrags := b.cgBaseAndFrags(cgx)
		if cgFrags > b.dataStart {
			reserved := b.dataStart
			if cgx != 0 {
				reserved -= ffsSBlkNo
			}
			out += cgFrags - reserved
		}
	}
	if out > 0 {
		out--
	}
	return out
}

func (b *ffsBuilder) cgBaseAndFrags(cgx uint32) (uint32, uint32) {
	base, end := b.cgRange(cgx)
	return base, end - base
}

func (b *ffsBuilder) cgRange(cgx uint32) (uint32, uint32) {
	base := cgx * b.cgFrags
	end := base + b.cgFrags
	if end > b.fsFrags {
		end = b.fsFrags
	}
	return base, end
}

func (b *ffsBuilder) usedInosForCG(cgx uint32) []bool {
	start := cgx * b.ipg
	end := start + b.ipg
	if end > uint32(len(b.usedInos)) {
		end = uint32(len(b.usedInos))
	}
	return b.usedInos[start:end]
}

func (b *ffsBuilder) inodeOffset(ino uint32) int64 {
	cgx := ino / b.ipg
	local := ino % b.ipg
	return b.fsOffset + fragByteOffset(cgx*b.cgFrags+ffsIBlkNo) + int64(local)*int64(ffsInodeSize)
}

func encodeFFSDir(node *ffsNode) []byte {
	type ent struct {
		ino  uint32
		typ  uint8
		name string
	}
	parentIno := node.ino
	if node.parent != nil {
		parentIno = node.parent.ino
	}
	entries := []ent{{node.ino, dtDIR, "."}, {parentIno, dtDIR, ".."}}
	for _, child := range node.children {
		entries = append(entries, ent{child.ino, ffsDirType(child.mode), child.name})
	}
	var buf []byte
	block := make([]byte, ffsSectorSize)
	off := 0
	prevOff := -1
	for _, e := range entries {
		minReclen := directSize(len(e.name))
		if off != 0 && off+minReclen > ffsSectorSize {
			Direct{buf: block[prevOff:ffsSectorSize]}.SetReclen(uint16(ffsSectorSize - prevOff))
			buf = append(buf, block...)
			block = make([]byte, ffsSectorSize)
			off = 0
			prevOff = -1
		}
		if prevOff >= 0 {
			Direct{buf: block[prevOff:off]}.SetReclen(uint16(off - prevOff))
		}
		ent := Direct{buf: block[off : off+minReclen]}
		ent.SetIno(e.ino)
		ent.SetReclen(uint16(minReclen))
		ent.SetType(e.typ)
		ent.SetName(e.name)
		prevOff = off
		off += minReclen
	}
	if prevOff >= 0 {
		Direct{buf: block[prevOff:ffsSectorSize]}.SetReclen(uint16(ffsSectorSize - prevOff))
		buf = append(buf, block...)
	}
	if len(buf) == 0 {
		return make([]byte, ffsSectorSize)
	}
	return buf
}

func directSize(nameLen int) int {
	return (8 + nameLen + 1 + 3) &^ 3
}

func ffsDirType(mode uint16) uint8 {
	switch mode & ifmt {
	case ifdir:
		return dtDIR
	case iflnk:
		return dtLNK
	case ifchr:
		return dtCHR
	case ifblk:
		return dtBLK
	case ififo:
		return dtFIFO
	case ifsock:
		return dtSOCK
	default:
		return dtREG
	}
}

func goModeToFFS(mode fs.FileMode) uint16 {
	perm := uint16(mode.Perm())
	switch {
	case mode&fs.ModeDevice != 0 && mode&fs.ModeCharDevice != 0:
		return ifchr | perm
	case mode&fs.ModeDevice != 0:
		return ifblk | perm
	case mode&fs.ModeNamedPipe != 0:
		return ififo | perm
	case mode&fs.ModeSocket != 0:
		return ifsock | perm
	default:
		return ifreg | perm
	}
}

func sortedImageDirEnts(entries []imagefs.DirEnt) []imagefs.DirEnt {
	out := append([]imagefs.DirEnt(nil), entries...)
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i].Name, out[j].Name) < 0
	})
	return out
}

func roundUp(v, align int64) int64 {
	return ((v + align - 1) / align) * align
}

func roundUpFrag(v, frag uint32) uint32 {
	return ((v + frag - 1) / frag) * frag
}

func fragByteOffset(frag uint32) int64 {
	return int64(frag) * int64(ffsFSize)
}

func countUsedInos(inos []bool) int {
	n := 0
	for _, used := range inos {
		if used {
			n++
		}
	}
	return n
}

func countDirsFromInodes(inos []bool) int {
	if len(inos) > ffsRootIno && inos[ffsRootIno] {
		return 1
	}
	return 0
}

func countDirNodes(node *ffsNode) int {
	n := 0
	if node.mode&ifmt == ifdir {
		n++
	}
	for _, child := range node.children {
		n += countDirNodes(child)
	}
	return n
}

func countDirNodesInCG(node *ffsNode, ipg, cgx uint32) int {
	if node == nil || ipg == 0 {
		return 0
	}
	n := 0
	if node.mode&ifmt == ifdir && node.ino/ipg == cgx {
		n++
	}
	for _, child := range node.children {
		n += countDirNodesInCG(child, ipg, cgx)
	}
	return n
}

func estimateFFSSize(stats ffsStats, extra int64) int64 {
	size := int64(stats.bytes)
	size += int64(stats.files+stats.dirs+stats.links+stats.devs) * ffsBSize
	size += size / 4
	size += 16 << 20
	size += extra
	if size < defaultFFSSize {
		size = defaultFFSSize
	}
	return size
}

func ffsCleanPath(value string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.TrimSpace(value), "/"))
}
