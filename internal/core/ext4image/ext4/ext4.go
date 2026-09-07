// Based on: https://www.kernel.org/doc/html/latest/filesystems/ext4/index.html

package ext4

import (
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	goFs "io/fs"
	"math"
	"os"
	"path"
	"strings"
	"time"

	"log/slog"

	vm "github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

const DEFAULT_MODE = goFs.FileMode(0755)

const INODE_SIZE = 256

type UUID [16]byte

type integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr
}

type InodeFlags uint32

const (
	InodeFlag_SECRM            InodeFlags = 0b1
	InodeFlag_UNRM             InodeFlags = 0b10
	InodeFlag_COMPR            InodeFlags = 0b100
	InodeFlag_SYNC             InodeFlags = 0b1000
	InodeFlag_IMMUTABLE        InodeFlags = 0b10000
	InodeFlag_APPEND           InodeFlags = 0b100000
	InodeFlag_NODUMP           InodeFlags = 0b1000000
	InodeFlag_NOATIME          InodeFlags = 0b10000000
	InodeFlag_DIRTY            InodeFlags = 0b100000000
	InodeFlag_COMPRBLK         InodeFlags = 0b1000000000
	InodeFlag_NOCOMPR          InodeFlags = 0b10000000000
	InodeFlag_ENCRYPT          InodeFlags = 0b100000000000
	InodeFlag_INDEX            InodeFlags = 0b1000000000000
	InodeFlag_IMAGIC           InodeFlags = 0b10000000000000
	InodeFlag_JOURNAL_DATA     InodeFlags = 0b100000000000000
	InodeFlag_NOTAIL           InodeFlags = 0b1000000000000000
	InodeFlag_DIRSYNC          InodeFlags = 0b10000000000000000
	InodeFlag_TOPDIR           InodeFlags = 0b100000000000000000
	InodeFlag_HUGE_FILE        InodeFlags = 0b1000000000000000000
	InodeFlag_EXTENTS          InodeFlags = 0b10000000000000000000
	InodeFlag_EA_INODE         InodeFlags = 0b1000000000000000000000
	InodeFlag_EOFBLOCKS        InodeFlags = 0b10000000000000000000000
	InodeFlag_SNAPFILE         InodeFlags = 0b1000000000000000000000000
	InodeFlag_SNAPFILE_DELETED InodeFlags = 0b100000000000000000000000000
	InodeFlag_SNAPFILE_SHRUNK  InodeFlags = 0b1000000000000000000000000000
	InodeFlag_INLINE_DATA      InodeFlags = 0b10000000000000000000000000000
	InodeFlag_PROJINHERIT      InodeFlags = 0b100000000000000000000000000000
	InodeFlag_RESERVED         InodeFlags = 0b10000000000000000000000000000000
)

type Feature_compat uint32

const (
	Feature_compat_COMPAT_DIR_PREALLOC   Feature_compat = 0b1
	Feature_compat_COMPAT_IMAGIC_INODES  Feature_compat = 0b10
	Feature_compat_COMPAT_HAS_JOURNAL    Feature_compat = 0b100
	Feature_compat_COMPAT_EXT_ATTR       Feature_compat = 0b1000
	Feature_compat_COMPAT_RESIZE_INODE   Feature_compat = 0b10000
	Feature_compat_COMPAT_DIR_INDEX      Feature_compat = 0b100000
	Feature_compat_COMPAT_LAZY_BG        Feature_compat = 0b1000000
	Feature_compat_COMPAT_EXCLUDE_INODE  Feature_compat = 0b10000000
	Feature_compat_COMPAT_EXCLUDE_BITMAP Feature_compat = 0b100000000
	Feature_compat_COMPAT_SPARSE_SUPER2  Feature_compat = 0b1000000000
)

type Feature_incompat uint32

const (
	Feature_incompat_INCOMPAT_COMPRESSION Feature_incompat = 0x1
	Feature_incompat_INCOMPAT_FILETYPE    Feature_incompat = 0x2
	Feature_incompat_INCOMPAT_RECOVER     Feature_incompat = 0x4
	Feature_incompat_INCOMPAT_JOURNAL_DEV Feature_incompat = 0x8
	Feature_incompat_INCOMPAT_META_BG     Feature_incompat = 0x10
	Feature_incompat_INCOMPAT_EXTENTS     Feature_incompat = 0x40
	Feature_incompat_INCOMPAT_64BIT       Feature_incompat = 0b10000000
	Feature_incompat_INCOMPAT_MMP         Feature_incompat = 0b100000000
	Feature_incompat_INCOMPAT_FLEX_BG     Feature_incompat = 0b1000000000
	Feature_incompat_INCOMPAT_EA_INODE    Feature_incompat = 0b10000000000
	Feature_incompat_INCOMPAT_DIRDATA     Feature_incompat = 0b1000000000000
	Feature_incompat_INCOMPAT_CSUM_SEED   Feature_incompat = 0b10000000000000
	Feature_incompat_INCOMPAT_LARGEDIR    Feature_incompat = 0b100000000000000
	Feature_incompat_INCOMPAT_INLINE_DATA Feature_incompat = 0b1000000000000000
	Feature_incompat_INCOMPAT_ENCRYPT     Feature_incompat = 0b10000000000000000
)

type Feature_ro_compat uint32

const (
	Feature_ro_compat_RO_COMPAT_SPARSE_SUPER  Feature_ro_compat = 0b1
	Feature_ro_compat_RO_COMPAT_LARGE_FILE    Feature_ro_compat = 0b10
	Feature_ro_compat_RO_COMPAT_BTREE_DIR     Feature_ro_compat = 0b100
	Feature_ro_compat_RO_COMPAT_HUGE_FILE     Feature_ro_compat = 0b1000
	Feature_ro_compat_RO_COMPAT_GDT_CSUM      Feature_ro_compat = 0b10000
	Feature_ro_compat_RO_COMPAT_DIR_NLINK     Feature_ro_compat = 0b100000
	Feature_ro_compat_RO_COMPAT_EXTRA_ISIZE   Feature_ro_compat = 0b1000000
	Feature_ro_compat_RO_COMPAT_HAS_SNAPSHOT  Feature_ro_compat = 0b10000000
	Feature_ro_compat_RO_COMPAT_QUOTA         Feature_ro_compat = 0b100000000
	Feature_ro_compat_RO_COMPAT_BIGALLOC      Feature_ro_compat = 0b1000000000
	Feature_ro_compat_RO_COMPAT_METADATA_CSUM Feature_ro_compat = 0b10000000000
	Feature_ro_compat_RO_COMPAT_REPLICA       Feature_ro_compat = 0b100000000000
	Feature_ro_compat_RO_COMPAT_READONLY      Feature_ro_compat = 0b1000000000000
	Feature_ro_compat_RO_COMPAT_PROJECT       Feature_ro_compat = 0b10000000000000
)

type Directory interface {
	String() string
	AddEntry(child *InodeWrapper, name string) error
	GetChild(name string) (*InodeWrapper, error)
}

type DirectoryEntry struct {
	*vm.PaddedRegion

	offset int64
	ent    *DirEntry2
	name   string
	target *InodeWrapper
}

func (ent DirectoryEntry) String() string {
	return fmt.Sprintf("\"% 30s\":0x%X:%s", ent.name, ent.offset, ent.ent.String())
}

func (ent DirectoryEntry) recordLength() uint16 {
	recLen := uint16(ent.ent.Size()) + uint16(len(ent.name))

	recLen = roundUpDiv(recLen, 4) * 4

	return recLen
}

func (ent *DirectoryEntry) setRecordLength(recLen uint16) {
	ent.ent.SetRecLen(recLen)

	ent.PaddedRegion = vm.NewPaddedRegion(vm.NewRegionArray[vm.MemoryRegion](
		ent.ent,
		vm.RawRegion(ent.name),
	), int64(recLen))
}

func newDirectoryEntry(target *InodeWrapper, childNode uint32, typ uint8, name string, recLen uint16) *DirectoryEntry {
	ent := &DirectoryEntry{
		target: target,
		ent:    &DirEntry2{},
		name:   name,
	}

	ent.ent.SetRecLen(recLen)
	ent.ent.SetInode(childNode)
	ent.ent.SetFileType(typ)
	ent.ent.SetNameLen(uint8(len(ent.name)))

	ent.PaddedRegion = vm.NewPaddedRegion(vm.NewRegionArray[vm.MemoryRegion](
		ent.ent,
		vm.RawRegion(name),
	), int64(recLen))

	return ent
}

type LinearDirectoryBlock struct {
	ents         *vm.RegionArray[*DirectoryEntry]
	paddedRegion *vm.PaddedRegion
}

type LinearDirectory struct {
	fs         *Ext4Filesystem
	extentTree ExtentTree
	inode      *InodeWrapper
	ents       map[string]*DirectoryEntry
	blocks     []*LinearDirectoryBlock
}

// GetChild implements Directory.
func (d *LinearDirectory) GetChild(name string) (*InodeWrapper, error) {
	ent, ok := d.ents[name]
	if !ok {
		return nil, os.ErrNotExist
	}

	return ent.target, nil
}

// AddEntry implements Directory.
func (d *LinearDirectory) AddEntry(child *InodeWrapper, name string) error {
	if _, exists := d.ents[name]; exists {
		return fmt.Errorf("entry %s already exists", name)
	}

	blockSize := child.fs.sb.blockSize()

	childMode := child.Mode()

	typ := 0x1

	if childMode&goFs.ModeSymlink != 0 {
		// Check if this is a symbolic link.
		typ = 0x7
	} else if childMode.IsDir() {
		typ = 0x2
	}

	block := d.blocks[len(d.blocks)-1]

	var ent *DirectoryEntry

	if block.ents.Len() == 0 {
		ent = newDirectoryEntry(child, uint32(child.num), uint8(typ), name, uint16(blockSize))
		block.ents.Append(ent)
	} else {
		lastEnt := block.ents.Get(block.ents.Len() - 1)
		currentLen := lastEnt.ent.RecLen()

		requiredLen := roundUpDiv(8+len(name), 4) * 4

		lastRecLen := lastEnt.recordLength()

		// Make sure the directory entry has enough room to add the new entry.
		if int(currentLen-lastRecLen) < requiredLen {
			// Otherwise increase the size.
			if err := d.increaseSize(); err != nil {
				return fmt.Errorf("increase directory size: %w", err)
			}

			// Call the method again.
			return d.AddEntry(child, name)
		}

		ent = newDirectoryEntry(child, uint32(child.num), uint8(typ), name, currentLen-lastRecLen)

		block.ents.Append(ent)

		lastEnt.setRecordLength(lastRecLen)
	}

	d.ents[name] = ent

	return nil
}

func (d LinearDirectory) String() string {
	ret := "Directory ["

	for _, block := range d.blocks {
		ret += "\n   Block ["
		block.ents.Range(func(_ int, ent *DirectoryEntry) bool {
			ret += "\n    " + ent.String() + ","
			return true
		})
		ret += "\n   ]"
	}

	return ret + "\n  ]"
}

func (d *LinearDirectory) increaseSize() error {
	// Get the extents.
	extents, err := d.extentTree.Extents()
	if err != nil {
		return err
	}

	// Split the extents into a list of blocks.
	extents = splitExtentIntoBlocks(extents)

	if len(extents) == len(d.blocks) {
		// Allocate more blocks.
		// The block allocation goes 1,4,16,64 blocks.
		if err = d.extentTree.AllocateBlocks(int64(len(d.blocks) * 3)); err != nil {
			slog.Debug("failed to allocate blocks", "blocks", int64(len(d.blocks)*2))
			return err
		}

		// Get the new list of extents.
		extents, err = d.extentTree.Extents()
		if err != nil {
			return err
		}

		// For each new node mark it with a empty directory entry filling up the entire node.
		lastExtent := extents[len(extents)-1]

		lastExtentBlocks := splitExtentIntoBlocks([]Extent{lastExtent})

		for i, block := range lastExtentBlocks {
			if i == 0 {
				continue // skip the first one as a optimization.
			}

			ent := &DirEntry2{}

			ent.SetRecLen(uint16(d.fs.sb.blockSize()))

			if err := d.fs.mapRawExtent(
				ent,
				&block,
			); err != nil {
				return err
			}
		}

		// Split the extents into a list of blocks.
		extents = splitExtentIntoBlocks(extents)
	}

	// Get the next extent for the directory block.
	nextExtent := extents[len(d.blocks)]

	// Add the block to the directory.
	if err := d.addBlock(nextExtent); err != nil {
		return err
	}

	// Update the size in the node.
	d.inode.node.SetNSize(uint64(len(extents)) * d.fs.sb.blockSize())
	d.inode.node.SetBlocks((uint64(len(extents)) * d.fs.sb.blockSize()) / 512)

	return nil
}

func (dir *LinearDirectory) addBlock(extent Extent) error {
	block := &LinearDirectoryBlock{ents: vm.NewRegionArray[*DirectoryEntry]()}

	// Create a padded region to store the directory data.
	block.paddedRegion = vm.NewPaddedRegion(
		block.ents,
		int64(extent.Length)*int64(dir.fs.sb.blockSize()),
	)

	// Map the directory to the virtual filesystem.
	if err := dir.fs.mapRawExtent(
		block.paddedRegion,
		&extent,
	); err != nil {
		return err
	}

	dir.blocks = append(dir.blocks, block)

	return nil
}

func mapLinearDirectory(fs *Ext4Filesystem, tree ExtentTree) (*LinearDirectory, error) {
	dir := &LinearDirectory{
		fs:         fs,
		extentTree: tree,
		blocks: []*LinearDirectoryBlock{
			{ents: vm.NewRegionArray[*DirectoryEntry]()},
		},
		ents: make(map[string]*DirectoryEntry),
	}

	extents, err := tree.Extents()
	if err != nil {
		return nil, fmt.Errorf("read directory extents: %w", err)
	}

	if len(extents) != 1 {
		return nil, fmt.Errorf("directories with multiple extents are not implemented")
	}

	if extents[0].FirstFileBlock != 0 {
		return nil, fmt.Errorf("sparse extents are not implemented")
	}

	extent := extents[0]

	startOffset := extent.StartBlock * fs.sb.blockSize()
	currentOffset := extent.StartBlock * fs.sb.blockSize()

	for {
		if (currentOffset - startOffset) >= 1024 {
			break
		}

		ent := &DirectoryEntry{ent: &DirEntry2{}, offset: int64(currentOffset)}

		if err := fs.vm.Reinterpret(ent.ent, int64(currentOffset)); err != nil {
			return nil, fmt.Errorf("map directory entry at offset %d: %w", currentOffset, err)
		}

		// if err := fs.vm.Reinterpret(ent.ent, int64(currentOffset)); err != nil {
		// 	return nil, fmt.Errorf("TODO: %v", err)
		// }

		nameOffset := currentOffset + uint64(ent.ent.Size())

		name := make([]byte, ent.ent.NameLen())

		if _, err := fs.vm.ReadAt(name, int64(nameOffset)); err != nil {
			return nil, fmt.Errorf("read directory entry name at offset %d: %w", nameOffset, err)
		}

		ent.name = string(name)

		if ent.ent.RecLen() == 0 {
			slog.Warn("zero length record", "offset", currentOffset)
			break
		}

		currentOffset += uint64(ent.ent.RecLen())

		dir.blocks[0].ents.Append(ent)
	}

	return dir, nil
}

func newLinearDirectory(fs *Ext4Filesystem, inode *InodeWrapper, tree ExtentTree) (*LinearDirectory, error) {
	dir := &LinearDirectory{
		fs:         fs,
		extentTree: tree,
		inode:      inode,
		blocks:     []*LinearDirectoryBlock{},
		ents:       make(map[string]*DirectoryEntry),
	}

	// Get the extent list.
	extents, err := tree.Extents()
	if err != nil {
		return nil, err
	}

	// Assume that there is only 1 extent in the list.
	if len(extents) != 1 {
		return nil, fmt.Errorf("newLinearDirectory does not support multi-extent inodes")
	}

	extent := extents[0]

	if err := dir.addBlock(extent); err != nil {
		return nil, err
	}

	return dir, nil
}

type HashedDirectory struct {
}

// GetChild implements Directory.
func (*HashedDirectory) GetChild(name string) (*InodeWrapper, error) {
	return nil, fmt.Errorf("unimplemented")
}

// AddEntry implements Directory.
func (*HashedDirectory) AddEntry(child *InodeWrapper, name string) error {
	return fmt.Errorf("not implemented")
}

func (d HashedDirectory) String() string {
	ret := "Directory["

	ret += "\n  HASHED"

	return ret + "\n  ]"
}

func mapHashedDirectory(fs *Ext4Filesystem, tree ExtentTree) (*HashedDirectory, error) {
	return &HashedDirectory{}, nil
}

var (
	_ Directory = &LinearDirectory{}
	_ Directory = &HashedDirectory{}
)

type Extent struct {
	FirstFileBlock uint32
	StartBlock     uint64
	Length         uint16
}

func (e *Extent) String() string {
	return fmt.Sprintf("Extent{%d %d-%d}", e.FirstFileBlock, e.StartBlock, e.Length)
}

func NewExtent(firstFileBlock uint32, startBlock uint64, length uint16) (Extent, error) {
	ext := Extent{
		FirstFileBlock: firstFileBlock,
		StartBlock:     startBlock,
		Length:         length,
	}

	if startBlock == 0 {
		return ext, fmt.Errorf("NewExtent startBlock == 0 (null pointer)")
	}

	return ext, nil
}

// Split a list of extents into a series of virtual extents with 1 extent per block.
func splitExtentIntoBlocks(ext []Extent) []Extent {
	var ret []Extent

	for _, extent := range ext {
		for i := 0; i < int(extent.Length); i++ {
			ret = append(ret, Extent{
				FirstFileBlock: extent.FirstFileBlock + uint32(i),
				StartBlock:     extent.StartBlock + uint64(i),
				Length:         1,
			})
		}
	}

	return ret
}

type ExtentTree interface {
	Extents() ([]Extent, error)
	AllocateBlocks(blocks int64) error
}

type ExtentTree1 struct {
	header *ExtentTreeHeader

	indexNodes vm.RegionArray[*ExtentTreeIdx]
	leafNodes  vm.RegionArray[*ExtentTreeNode]

	region *vm.PaddedRegion
	arr    *vm.RegionArray[vm.MemoryRegion]
}

const MaxInlineExtents int64 = 4

type ExtentLimitError struct {
	RequiredExtents  int64
	SupportedExtents int64
}

func (e *ExtentLimitError) Error() string {
	return fmt.Sprintf("ext4 layout requires %d extents; writer supports at most %d inline extents", e.RequiredExtents, e.SupportedExtents)
}

func newExtentLimitError(required int64) error {
	return &ExtentLimitError{RequiredExtents: required, SupportedExtents: MaxInlineExtents}
}

// AllocateBlocks implements ExtentTree.
func (e *ExtentTree1) AllocateBlocks(blocks int64) error {
	return fmt.Errorf("not implemented")
}

func (e *ExtentTree1) Extents() ([]Extent, error) {
	if e.indexNodes.Len() != 0 {
		return nil, fmt.Errorf("extents with index nodes not implemented")
	}

	var ret []Extent

	for i := 0; i < e.leafNodes.Len(); i++ {
		leaf := e.leafNodes.Get(i)
		ext, err := NewExtent(leaf.Block(), leaf.Start(), leaf.Len())
		if err != nil {
			return nil, err
		}

		ret = append(ret, ext)
	}

	return ret, nil
}

func (e *ExtentTree1) String() string {
	ret := "ExtentTree{"

	ret += "header=" + e.header.String() + " "

	ret += "leafs=["
	for i := 0; i < e.leafNodes.Len(); i++ {
		leaf := e.leafNodes.Get(i)
		ret += leaf.String()
	}
	ret += "] "

	return ret + "}"
}

func mapExtentTree(vm *vm.VirtualMemory, base uint64) (*ExtentTree1, error) {
	tree := &ExtentTree1{header: &ExtentTreeHeader{}}

	if err := vm.Reinterpret(tree.header, int64(base)); err != nil {
		return nil, fmt.Errorf("map extent tree header at offset %d: %w", base, err)
	}
	base += uint64(tree.header.Size())

	if tree.header.Magic() == 0 {
		return nil, nil
	}

	if tree.header.Magic() != 0xF30A {
		slog.Warn("invalid extent header", "hdr", tree.header, "raw", tree.header[:])
		return nil, nil
	}

	// feature_compat=512 feature_incompat=706 feature_ro_compat=11

	if tree.header.Depth() == 0 {
		for i := 0; i < int(tree.header.Entries()); i++ {
			leaf := &ExtentTreeNode{}

			if err := vm.Reinterpret(leaf, int64(base)); err != nil {
				return nil, fmt.Errorf("map extent tree leaf: %w", err)
			}
			base += uint64(leaf.Size())

			tree.leafNodes.Append(leaf)
		}

		if tree.leafNodes.Len() > 1 {
			slog.Warn("tree has more than 1 leaf node")
		}
	} else {
		slog.Warn("internal nodes not implemented")
		return nil, nil
	}

	return tree, nil
}

func newExtentTree1(fs *Ext4Filesystem, base uint64, blocks int64) (*ExtentTree1, error) {
	tree := &ExtentTree1{header: &ExtentTreeHeader{}}

	// Allocate blocks.
	extents, err := fs.allocateMultiExtentBlocks(blocks)
	if err != nil {
		return nil, err
	}

	if len(extents) <= int(MaxInlineExtents) {
		// Set the fields on the header.
		tree.header.SetMagic(0xF30A)
		tree.header.SetEntries(uint16(len(extents)))
		tree.header.SetMax(4)
		tree.header.SetDepth(0)

		for _, extent := range extents {
			leaf := &ExtentTreeNode{}

			// Set the fields on the leaf.
			leaf.SetBlock(extent.FirstFileBlock)
			leaf.SetLen(extent.Length)
			leaf.SetStart(extent.StartBlock)

			tree.leafNodes.Append(leaf)
		}

		tree.arr = vm.NewRegionArray[vm.MemoryRegion](
			tree.header,
			&tree.leafNodes,
		)

		// Make a padded region to extend the region to 60 bytes.
		tree.region = vm.NewPaddedRegion(tree.arr, 60)

		// Finally map the region to the base.
		if err := fs.mapRegion(tree.region, int64(base)); err != nil {
			return nil, err
		}
	} else {
		return nil, newExtentLimitError(int64(len(extents)))
	}

	return tree, nil
}

type ExtentTree2 struct {
	i     *InodeWrapper
	count int
}

// AllocateBlocks implements ExtentTree.
func (t *ExtentTree2) AllocateBlocks(blocks int64) error {
	// Check that we have enough space in the extentTree to allocate blocks.
	if t.count >= int(MaxInlineExtents) {
		return newExtentLimitError(int64(t.count + 1))
	}
	requiredExtents := roundUpDiv(blocks, int64(t.i.fs.sb.BlocksPerGroup()))
	if int64(t.count)+requiredExtents > MaxInlineExtents {
		return newExtentLimitError(int64(t.count) + requiredExtents)
	}

	// Allocate more extents.
	extents, err := t.i.fs.allocateMultiExtentBlocks(blocks)
	if err != nil {
		return err
	}

	if len(extents)+t.count > int(MaxInlineExtents) {
		return newExtentLimitError(int64(len(extents) + t.count))
	}

	i := 0

	if t.count == 1 && i < len(extents) {
		extent := extents[i]

		// Set the fields on the leaf.
		t.i.node.SetBlock1Block(t.i.node.Block0Block() + uint32(t.i.node.Block0Len()) + extent.FirstFileBlock)
		t.i.node.SetBlock1Len(extent.Length)
		t.i.node.SetBlock1Start(extent.StartBlock)

		t.i.node.SetBlockEntries(2)

		i += 1
		t.count += 1
	}

	if t.count == 2 && i < len(extents) {
		extent := extents[i]

		// Set the fields on the leaf.
		t.i.node.SetBlock2Block(t.i.node.Block1Block() + uint32(t.i.node.Block1Len()) + extent.FirstFileBlock)
		t.i.node.SetBlock2Len(extent.Length)
		t.i.node.SetBlock2Start(extent.StartBlock)

		t.i.node.SetBlockEntries(3)

		i += 1
		t.count += 1
	}

	if t.count == 3 && i < len(extents) {
		extent := extents[i]

		// Set the fields on the leaf.
		t.i.node.SetBlock3Block(t.i.node.Block2Block() + uint32(t.i.node.Block2Len()) + extent.FirstFileBlock)
		t.i.node.SetBlock3Len(extent.Length)
		t.i.node.SetBlock3Start(extent.StartBlock)

		t.i.node.SetBlockEntries(4)

		t.count += 1
	}

	return nil
}

// Extents implements ExtentTree.
func (t *ExtentTree2) Extents() ([]Extent, error) {
	var ret []Extent

	if t.count > 0 {
		ext, err := NewExtent(
			t.i.node.Block0Block(),
			t.i.node.Block0Start(),
			t.i.node.Block0Len(),
		)
		if err != nil {
			return nil, err
		}
		ret = append(ret, ext)
	}

	if t.count > 1 {
		ext, err := NewExtent(
			t.i.node.Block1Block(),
			t.i.node.Block1Start(),
			t.i.node.Block1Len(),
		)
		if err != nil {
			return nil, err
		}
		ret = append(ret, ext)
	}

	if t.count > 2 {
		ext, err := NewExtent(
			t.i.node.Block2Block(),
			t.i.node.Block2Start(),
			t.i.node.Block2Len(),
		)
		if err != nil {
			return nil, err
		}
		ret = append(ret, ext)
	}

	if t.count > 3 {
		ext, err := NewExtent(
			t.i.node.Block3Block(),
			t.i.node.Block3Start(),
			t.i.node.Block3Len(),
		)
		if err != nil {
			return nil, err
		}
		ret = append(ret, ext)
	}

	return ret, nil
}

func newExtentTree2(fs *Ext4Filesystem, i *InodeWrapper, blocks int64) (*ExtentTree2, error) {
	tree := &ExtentTree2{i: i, count: 1}

	// Allocate blocks.
	extents, err := fs.allocateMultiExtentBlocks(blocks)
	if err != nil {
		return nil, err
	}

	if len(extents) > int(MaxInlineExtents) {
		return nil, newExtentLimitError(int64(len(extents)))
	}

	// Set the fields on the header.
	i.node.SetBlockMagic(0xF30A)
	i.node.SetBlockEntries(uint16(len(extents)))
	i.node.SetBlockMax(4)
	i.node.SetBlockDepth(0)

	if len(extents) > 0 {
		extent := extents[0]

		// Set the fields on the leaf.
		i.node.SetBlock0Block(extent.FirstFileBlock)
		i.node.SetBlock0Len(extent.Length)
		i.node.SetBlock0Start(extent.StartBlock)
	}

	if len(extents) > 1 {
		extent := extents[1]

		// Set the fields on the leaf.
		i.node.SetBlock1Block(extent.FirstFileBlock)
		i.node.SetBlock1Len(extent.Length)
		i.node.SetBlock1Start(extent.StartBlock)
	}

	if len(extents) > 2 {
		extent := extents[2]

		// Set the fields on the leaf.
		i.node.SetBlock2Block(extent.FirstFileBlock)
		i.node.SetBlock2Len(extent.Length)
		i.node.SetBlock2Start(extent.StartBlock)
	}

	if len(extents) > 3 {
		extent := extents[3]

		// Set the fields on the leaf.
		i.node.SetBlock3Block(extent.FirstFileBlock)
		i.node.SetBlock3Len(extent.Length)
		i.node.SetBlock3Start(extent.StartBlock)
	}

	tree.count = len(extents)

	return tree, nil
}

var (
	_ ExtentTree = &ExtentTree1{}
	_ ExtentTree = &ExtentTree2{}
)

func newExtentTree(fs *Ext4Filesystem, i *InodeWrapper, blocks int64) (ExtentTree, error) {
	blockGroupSize := int64(fs.sb.BlocksPerGroup())

	requiredBlockGroups := roundUpDiv(blocks, blockGroupSize)

	// slog.Debug("", "requiredBlockGroups", requiredBlockGroups)

	if requiredBlockGroups <= MaxInlineExtents {
		return newExtentTree2(fs, i, blocks)
	} else {
		return nil, newExtentLimitError(requiredBlockGroups)
	}
}

type InodeWrapper struct {
	fs         *Ext4Filesystem
	bg         *BlockGroup
	offset     uint64
	num        int
	node       *Inode
	extentTree ExtentTree
	dir        Directory
}

const (
	S_IFBLK  = 0x6000
	S_IFCHR  = 0x2000
	S_IFDIR  = 0x4000
	S_IFIFO  = 0x1000
	S_IFLNK  = 0xa000
	S_IFMT   = 0xf000
	S_IFREG  = 0x8000
	S_IFSOCK = 0xc000
	S_ISGID  = 0x400
	S_ISUID  = 0x800
	S_ISVTX  = 0x200
)

func (i InodeWrapper) getModeType() goFs.FileMode {
	sysMode := i.node.Mode()

	mode := goFs.FileMode(0)

	switch sysMode & S_IFMT {
	case S_IFBLK:
		mode |= goFs.ModeDevice
	case S_IFCHR:
		mode |= goFs.ModeDevice | goFs.ModeCharDevice
	case S_IFDIR:
		mode |= goFs.ModeDir
	case S_IFIFO:
		mode |= goFs.ModeNamedPipe
	case S_IFLNK:
		mode |= goFs.ModeSymlink
	case S_IFREG:
		// nothing to do
	case S_IFSOCK:
		mode |= goFs.ModeSocket
	}
	if sysMode&S_ISGID != 0 {
		mode |= goFs.ModeSetgid
	}
	if sysMode&S_ISUID != 0 {
		mode |= goFs.ModeSetuid
	}
	if sysMode&S_ISVTX != 0 {
		mode |= goFs.ModeSticky
	}

	return mode
}

func (i InodeWrapper) Mode() goFs.FileMode {
	sysMode := i.node.Mode()

	mode := goFs.FileMode(sysMode&0777) | i.getModeType()

	return mode
}

func (i InodeWrapper) Flags() InodeFlags {
	return InodeFlags(i.node.Flags())
}

func (i InodeWrapper) String() string {
	mode := i.Mode()
	return fmt.Sprintf(
		"Inode {\n  inode = %d, offset = %d, size = %d, mode = %s, isDir = %+v, blocks = %d, flags = %032b\n  tree = %+v,\n  dir = %s,\n  fields = %s\n}",
		i.num, i.offset, i.node.NSize(), mode, mode.IsDir(), i.node.Blocks(), i.Flags(), i.extentTree, i.dir, i.node.String(),
	)
}

func (i *InodeWrapper) allocateExtent(blocks int64) error {
	extentTree, err := newExtentTree(i.fs, i, blocks)
	if err != nil {
		return err
	}

	i.extentTree = extentTree

	// Set the flag to identify that this node uses extents.
	i.node.SetFlags(0x80000)

	return nil
}

func (i *InodeWrapper) allocateDirectory(parent *InodeWrapper) error {
	// set the directory flag.
	i.node.SetMode(i.node.Mode() | S_IFDIR)

	// slog.Debug("", "mode", fmt.Sprintf("%X", i.node.Mode()))

	if !i.fs.deterministicTime.IsZero() {
		i.node.SetCtime(uint32(i.fs.deterministicTime.Unix()))
		i.node.SetMtime(uint32(i.fs.deterministicTime.Unix()))
		i.node.SetAtime(uint32(i.fs.deterministicTime.Unix()))
	} else {
		i.node.SetCtime(uint32(time.Now().Unix()))
		i.node.SetMtime(uint32(time.Now().Unix()))
		i.node.SetAtime(uint32(time.Now().Unix()))
	}

	// allocate a block to store the directory listing.
	if err := i.allocateExtent(1); err != nil {
		return err
	}

	i.node.SetNSize(4096)
	i.node.SetBlocks(8)

	// Map the extent as a directory.
	dir, err := newLinearDirectory(i.fs, i, i.extentTree)
	if err != nil {
		return err
	}
	i.dir = dir

	// Add the `.` and `..` directories.
	if err := i.addDirectoryEntry(i, "."); err != nil {
		return err
	}
	if err := i.addDirectoryEntry(parent, ".."); err != nil {
		return err
	}

	// Update the directory count in the bgd.
	i.bg.desc.SetUsedDirsCount(i.bg.desc.UsedDirsCount() + 1)

	return nil
}

func (i *InodeWrapper) getChild(name string) (*InodeWrapper, error) {
	if !i.Mode().IsDir() {
		return nil, goFs.ErrInvalid
	}

	child, err := i.dir.GetChild(name)
	if err != nil {
		return nil, err
	}

	return child, nil
}

var (
	totalAddContents int64 = 0
)

func (i *InodeWrapper) addContents(contents vm.MemoryRegion, symlink bool) error {
	start := time.Now()

	// set the file flag.
	if symlink {
		i.node.SetMode(i.node.Mode() | S_IFLNK)
	} else {
		i.node.SetMode(i.node.Mode() | S_IFREG)
	}

	// Update the times.
	if !i.fs.deterministicTime.IsZero() {
		i.node.SetCtime(uint32(i.fs.deterministicTime.Unix()))
		i.node.SetMtime(uint32(i.fs.deterministicTime.Unix()))
		i.node.SetAtime(uint32(i.fs.deterministicTime.Unix()))
	} else {
		i.node.SetCtime(uint32(time.Now().Unix()))
		i.node.SetMtime(uint32(time.Now().Unix()))
		i.node.SetAtime(uint32(time.Now().Unix()))
	}

	if symlink && contents.Size() < 60 {
		if err := i.fs.mapRegion(vm.NewPaddedRegion(contents, 60), int64(i.offset)+40); err != nil {
			return fmt.Errorf("map inline file data: %w", err)
		}

		i.node.SetNSize(uint64(contents.Size()))
	} else {
		blocks := roundUpDiv(int(contents.Size()), int(i.fs.sb.blockSize()))

		if err := i.allocateExtent(int64(blocks)); err != nil {
			return fmt.Errorf("allocate file extent: %w", err)
		}

		ext, err := i.extentTree.Extents()
		if err != nil {
			return fmt.Errorf("read file extents: %w", err)
		}

		i.node.SetNSize(uint64(contents.Size()))
		i.node.SetBlocks((uint64(blocks) * i.fs.sb.blockSize()) / 512)

		for _, extent := range ext {
			if err := i.fs.mapExtent(contents, &extent); err != nil {
				return fmt.Errorf("map file extent contents: %w", err)
			}
		}
	}

	totalAddContents += int64(time.Since(start).Nanoseconds())

	return nil
}

func (i *InodeWrapper) chmod(mode fs.FileMode) error {
	oldMode := i.node.Mode()

	sysPart := oldMode & ^uint16(0777)

	newMode := sysPart | uint16(mode&goFs.ModePerm)

	if mode&fs.ModeSetuid != 0 {
		newMode |= S_ISUID
	}
	if mode&fs.ModeSetgid != 0 {
		newMode |= S_ISGID
	}

	i.node.SetMode(newMode)

	// slog.Debug("", "mode", fmt.Sprintf("%X", i.node.Mode()))

	return nil
}

func (i *InodeWrapper) chown(uid uint16, gid uint16) error {
	i.node.SetUid(uid)
	i.node.SetGid(gid)

	return nil
}

func (i *InodeWrapper) chtime(mod time.Time) error {
	i.node.SetMtime(uint32(mod.Unix()))

	return nil
}

func (i *InodeWrapper) addDirectoryEntry(child *InodeWrapper, name string) error {
	child.node.SetLinksCount(child.node.LinksCount() + 1)

	return i.dir.AddEntry(child, name)
}

type BlockGroup struct {
	fs *Ext4Filesystem

	num         int
	offset      int64
	desc        *BlockGroupDescriptor
	inodeBitmap *vm.BitmapRegion
	blockBitmap *vm.BitmapRegion
	firstBlock  int64

	inodeCount uint32
	blockCount uint32

	firstFreeInode uint32
	firstFreeBlock uint32
}

func (bg *BlockGroup) allocateBlocks(blocks uint32) (*Extent, error) {
	start := bg.firstFreeBlock
	var noFreeBlocks = true

	// Check if this block group even has a chance of fitting all the blocks.
	if bg.desc.FreeBlocksCount() < blocks {
		return nil, nil
	}

	// Check if this is a full block group allocation.
	if blocks == bg.blockCount && bg.desc.FreeBlocksCount() == bg.blockCount {
		if bg.firstFreeBlock != 0 {
			return nil, nil
		}

		// Set the first free block to the number of blocks.
		bg.firstFreeBlock = bg.blockCount

		// Set the free block count to 0.
		bg.desc.SetFreeBlocksCount(0)

		// Fill the entire block bitmap.
		if err := bg.blockBitmap.SetAll(true); err != nil {
			return nil, fmt.Errorf("mark all blocks allocated in block group %d: %w", bg.num, err)
		}

		// Return a new extent.
		ext, err := NewExtent(0, uint64(bg.firstBlock), uint16(blocks))
		if err != nil {
			return nil, fmt.Errorf("create full block-group extent: %w", err)
		}

		return &ext, nil
	}

	for i := bg.firstFreeBlock; i < bg.blockCount; i++ {
		used, err := bg.blockBitmap.Get(uint64(i))
		if err != nil {
			return nil, fmt.Errorf("read block %d allocation bit in block group %d: %w", i, bg.num, err)
		}

		if used {
			if noFreeBlocks {
				bg.firstFreeBlock = i + 1
			}

			start = i + 1

			continue
		} else {
			noFreeBlocks = false
		}

		if i-start+1 != blocks {
			continue
		}

		// We found the blocks we need.

		// Reserve them in the bitmap.
		for x := start; x <= i; x++ {
			if err := bg.blockBitmap.Set(uint64(x), true); err != nil {
				return nil, fmt.Errorf("mark block %d allocated in block group %d: %w", x, bg.num, err)
			}
		}

		// Update the block count.
		bg.desc.SetFreeBlocksCount(bg.desc.FreeBlocksCount() - uint32(blocks))

		// slog.Debug("allocated", "start", start, "blocks", blocks)

		// Return the extent.
		ext, err := NewExtent(0, uint64(bg.firstBlock)+uint64(start), uint16(blocks))
		if err != nil {
			// slog.Debug("",
			// 	"firstBlock", bg.firstBlock,
			// 	"start", start,
			// 	"blocks", blocks,
			// 	"freeBlocks", bg.desc.FreeBlocksCount(),
			// )
			return nil, fmt.Errorf("create block-group extent: %w", err)
		}

		return &ext, nil
	}

	return nil, nil
}

func (bg *BlockGroup) allocateInode() (*InodeWrapper, error) {
	if bg.desc.FreeInodesCount() == 0 {
		return nil, nil
	}

	for i := bg.firstFreeInode; i < bg.inodeCount; i++ {
		used, err := bg.inodeBitmap.Get(uint64(i))
		if err != nil {
			return nil, fmt.Errorf("read inode %d allocation bit in block group %d: %w", i, bg.num, err)
		}

		if used {
			bg.firstFreeInode = i + 1

			continue
		}

		// Found the inode. Set it in the bitmap.
		if err := bg.inodeBitmap.Set(uint64(i), true); err != nil {
			return nil, fmt.Errorf("mark inode %d allocated in block group %d: %w", i, bg.num, err)
		}

		// Calculate the inode number and the offset into the inode table.
		inodeNumber := (bg.inodeCount * uint32(bg.num)) + i + 1
		inodeTableStart := bg.desc.InodeTable() * bg.fs.sb.blockSize()
		inodeOffset := inodeTableStart + uint64(i*INODE_SIZE)

		// Make the wrapper.
		inode := &InodeWrapper{
			fs:     bg.fs,
			bg:     bg,
			offset: uint64(inodeOffset),
			num:    int(inodeNumber),
			node:   &Inode{},
		}

		if err := inode.chmod(DEFAULT_MODE); err != nil {
			return nil, err
		}

		// slog.Debug("map inode", "off", inodeOffset, "num", inodeNumber)
		// Map the inode data.
		if err := bg.fs.mapRegion(inode.node, int64(inodeOffset)); err != nil {
			return nil, err
		}

		// Update the inode count.
		bg.desc.SetFreeInodesCount(bg.desc.FreeInodesCount() - 1)

		// Return the wrapper.
		return inode, nil
	}

	return nil, nil
}

type Ext4Filesystem struct {
	vm *vm.VirtualMemory

	sb *Superblock

	bgs        []*BlockGroup
	inodes     map[int]*InodeWrapper
	inodeCache map[string]*InodeWrapper

	deterministicTime time.Time
}

func (fs *Ext4Filesystem) allocateMultiExtentBlocks(blocks int64) ([]*Extent, error) {
	blockGroupSize := int64(fs.sb.BlocksPerGroup())
	totalBlockGroups := blocks / blockGroupSize
	remainingBlocks := blocks % blockGroupSize

	var currentBlock uint32 = 0

	var ret []*Extent
	for i := 0; i < int(totalBlockGroups); i++ {
		// TODO(joshua): Add optimized full block group allocator.
		ext, err := fs.allocateBlocks(blockGroupSize)
		if err != nil {
			return nil, err
		}

		ext.FirstFileBlock = currentBlock

		currentBlock += uint32(ext.Length)

		ret = append(ret, ext)
	}

	if remainingBlocks > 0 {
		ext, err := fs.allocateBlocks(remainingBlocks)
		if err != nil {
			return nil, err
		}

		ext.FirstFileBlock = currentBlock

		ret = append(ret, ext)
	}

	// if len(ret) > 1 {
	// 	slog.Debug("multi extent",
	// 		"totalBlockGroups", totalBlockGroups,
	// 		"remainingBlocks", remainingBlocks,
	// 		"ret", ret,
	// 	)
	// }

	return ret, nil
}

var (
	totalAllocateBlocks int64 = 0
)

func (fs *Ext4Filesystem) allocateBlocks(blocks int64) (*Extent, error) {
	start := time.Now()

	if blocks > int64(fs.sb.BlocksPerGroup()) {
		return nil, fmt.Errorf("requested blocks is larger than can fit in a single group. Use allocateMultiExtentBlocks")
	}

	// Always tries to map contiguous blocks.
	for _, bg := range fs.bgs {
		ext, err := bg.allocateBlocks(uint32(blocks))
		if err != nil {
			return nil, err
		}

		if ext != nil {
			// Update the free block count.
			fs.sb.SetFreeBlocksCount(fs.sb.FreeBlocksCount() - uint64(blocks))

			totalAllocateBlocks += int64(time.Since(start).Nanoseconds())

			return ext, nil
		}
	}

	// TODO(joshua): Add a fallback for mapping from fragments.

	return nil, fmt.Errorf("filesystem is full or fragmented")
}

func (fs *Ext4Filesystem) allocateBlocksForBytes(size int64) (*Extent, error) {
	blocks := roundUpDiv(size, int64(fs.sb.blockSize()))

	return fs.allocateBlocks(blocks)
}

func (fs *Ext4Filesystem) reserveSuperblockBlock() error {
	if len(fs.bgs) == 0 {
		return fmt.Errorf("no block group contains the superblock")
	}
	bg := fs.bgs[0]
	if bg.firstBlock != 0 || bg.blockCount == 0 || bg.desc.FreeBlocksCount() == 0 {
		return fmt.Errorf("block zero is unavailable in block group 0")
	}
	used, err := bg.blockBitmap.Get(0)
	if err != nil {
		return fmt.Errorf("read superblock allocation bit: %w", err)
	}
	if used {
		return fmt.Errorf("block zero is already allocated")
	}
	if err := bg.blockBitmap.Set(0, true); err != nil {
		return fmt.Errorf("mark superblock block allocated: %w", err)
	}
	bg.firstFreeBlock = 1
	bg.desc.SetFreeBlocksCount(bg.desc.FreeBlocksCount() - 1)
	fs.sb.SetFreeBlocksCount(fs.sb.FreeBlocksCount() - 1)
	return nil
}

var (
	totalAllocateInode int64 = 0
)

func (fs *Ext4Filesystem) allocateInode() (*InodeWrapper, error) {
	start := time.Now()

	var inode *InodeWrapper = nil
	var err error

	for _, bg := range fs.bgs {
		inode, err = bg.allocateInode()
		if err != nil {
			return nil, err
		}

		if inode != nil {
			break
		}
	}

	if inode == nil {
		return nil, fmt.Errorf("filesystem has run out of inodes")
	}

	// Add the inode to the main inode index.
	fs.inodes[inode.num] = inode

	// Update the free inode count.
	fs.sb.SetFreeInodesCount(fs.sb.FreeInodesCount() - 1)

	totalAllocateInode += int64(time.Since(start).Nanoseconds())

	return inode, nil
}

func (fs *Ext4Filesystem) root() (*InodeWrapper, error) {
	return fs.inodes[2], nil
}

func (fs *Ext4Filesystem) getNode(filename string, debug bool, mkdir bool) (*InodeWrapper, error) {
	filename = strings.TrimSuffix(filename, "/")

	if inode, ok := fs.inodeCache[filename]; ok {
		return inode, nil
	}

	parentName := path.Dir(filename)

	if parent, ok := fs.inodeCache[parentName]; ok {
		child, err := parent.getChild(path.Base(filename))
		if err == os.ErrNotExist && mkdir {
			token := path.Base(filename)

			d, err := fs.allocateInode()
			if err != nil {
				return nil, err
			}

			if err := d.allocateDirectory(parent); err != nil {
				return nil, err
			}

			if err := parent.addDirectoryEntry(d, token); err != nil {
				return nil, err
			}

			child, err = parent.getChild(token)
			if err != nil {
				return nil, fmt.Errorf("failed to get child %s: %s", token, err)
			}
		} else if err != nil {
			return nil, fmt.Errorf("failed to get child (cached) %s: %s", path.Base(filename), err)
		}

		fs.inodeCache[filename] = child

		return child, nil
	}

	tokens := strings.Split(filename, "/")

	if debug {
		slog.Debug("", "tokens", tokens)
	}

	currentNode, err := fs.root()
	if err != nil {
		return nil, err
	}

	for _, token := range tokens[1:] {
		if token == "" {
			continue
		}

		child, err := currentNode.getChild(token)
		if err == os.ErrNotExist && mkdir {
			d, err := fs.allocateInode()
			if err != nil {
				return nil, err
			}

			if err := d.allocateDirectory(currentNode); err != nil {
				return nil, err
			}

			if err := currentNode.addDirectoryEntry(d, token); err != nil {
				return nil, err
			}

			child, err = currentNode.getChild(token)
			if err != nil {
				return nil, fmt.Errorf("failed to get child %s: %s", token, err)
			}
		} else if err != nil {
			return nil, fmt.Errorf("failed to get child %s: %s", token, err)
		}

		if debug {
			slog.Debug("", "name", token, "child", child)
		}

		currentNode = child
	}

	fs.inodeCache[filename] = currentNode

	return currentNode, nil
}

func (fs *Ext4Filesystem) Mkdir(filename string, all bool) error {
	parentName := path.Dir(filename)
	newDirName := path.Base(filename)

	node, err := fs.getNode(parentName, false, all)
	if err != nil {
		return err
	}

	d, err := fs.allocateInode()
	if err != nil {
		return err
	}

	if err := d.allocateDirectory(node); err != nil {
		return err
	}

	if err := node.addDirectoryEntry(d, path.Base(newDirName)); err != nil {
		return err
	}

	fs.inodeCache[filename] = d

	return nil
}

func (fs *Ext4Filesystem) CreateFile(filename string, content vm.MemoryRegion) error {
	node, err := fs.getNode(path.Dir(filename), false, false)
	if err != nil {
		return err
	}

	if !node.Mode().IsDir() {
		return goFs.ErrInvalid
	}

	f, err := fs.allocateInode()
	if err != nil {
		return fmt.Errorf("allocate inode for %s: %w", filename, err)
	}

	if err := f.addContents(content, false); err != nil {
		return fmt.Errorf("add contents for %s: %w", filename, err)
	}

	if err := node.addDirectoryEntry(f, path.Base(filename)); err != nil {
		return fmt.Errorf("add directory entry for %s: %w", filename, err)
	}

	return nil
}

func (fs *Ext4Filesystem) Link(filename string, target string) error {
	if !strings.HasPrefix(target, "/") {
		return fmt.Errorf("hard links must use absolute paths")
	}

	node, err := fs.getNode(path.Dir(filename), false, false)
	if err != nil {
		return err
	}

	if !node.Mode().IsDir() {
		return goFs.ErrInvalid
	}

	targetNode, err := fs.getNode(target, false, false)
	if err != nil {
		return err
	}

	if err := node.addDirectoryEntry(targetNode, path.Base(filename)); err != nil {
		return err
	}

	return nil
}

func (fs *Ext4Filesystem) Symlink(filename string, target string) error {
	node, err := fs.getNode(path.Dir(filename), false, false)
	if err != nil {
		return err
	}

	if !node.Mode().IsDir() {
		return goFs.ErrInvalid
	}

	f, err := fs.allocateInode()
	if err != nil {
		return err
	}

	if err := f.addContents(vm.RawRegion(target), true); err != nil {
		return err
	}

	if err := node.addDirectoryEntry(f, path.Base(filename)); err != nil {
		return err
	}

	return nil
}

func (fs *Ext4Filesystem) Exists(filename string) bool {
	_, err := fs.getNode(filename, false, false)
	return err == nil
}

func (fs *Ext4Filesystem) Chmod(filename string, mode goFs.FileMode) error {
	node, err := fs.getNode(filename, false, false)
	if err != nil {
		return err
	}

	return node.chmod(mode)
}

func (fs *Ext4Filesystem) Chown(filename string, uid uint16, gid uint16) error {
	node, err := fs.getNode(filename, false, false)
	if err != nil {
		return err
	}

	return node.chown(uid, gid)
}

func (fs *Ext4Filesystem) Chtimes(filename string, mod time.Time) error {
	node, err := fs.getNode(filename, false, false)
	if err != nil {
		return err
	}

	return node.chtime(mod)
}

var (
	totalMapRegion int64 = 0
)

func (fs *Ext4Filesystem) mapRegion(region vm.MemoryRegion, offset int64) error {
	if offset == 0 {
		return fmt.Errorf("mapRegion offset == 0 (null pointer)")
	}

	start := time.Now()

	err := fs.vm.Map(region, offset)
	if err != nil {
		return err
	}

	totalMapRegion += int64(time.Since(start).Nanoseconds())

	return nil
}

func (fs *Ext4Filesystem) mapExtent(region vm.MemoryRegion, extent *Extent) error {
	return fs.mapRawExtent(
		vm.NewOffsetRegion(region, int64(extent.FirstFileBlock)*int64(fs.sb.blockSize())),
		extent,
	)
}

func (fs *Ext4Filesystem) mapRawExtent(region vm.MemoryRegion, extent *Extent) error {
	return fs.mapRegion(
		vm.NewTruncatedRegion(region, int64(extent.Length)*int64(fs.sb.blockSize())),
		int64(extent.StartBlock)*int64(fs.sb.blockSize()),
	)
}

func (fs *Ext4Filesystem) DumpDebug(filename string) {
	ent, err := fs.getNode(filename, true, false)
	if err != nil {
		slog.Error("file does not exist", "filename", filename)
		return
	}

	slog.Debug("DumpDebug", "ent", ent)
}

func (fs *Ext4Filesystem) DumpInodeMap(out io.Writer) error {
	for num := 0; num < len(fs.inodes); num++ {
		node, ok := fs.inodes[num]
		if !ok {
			continue
		}

		nodeString := node.String()

		if _, err := fmt.Fprintf(out, "%08d: %s\n", num, nodeString); err != nil {
			return err
		}
	}
	return nil
}

func (fs *Ext4Filesystem) MakeDeterministic(fsUuid UUID, createTime time.Time) error {
	fs.sb.SetLastcheck(uint32(createTime.Unix()))
	fs.sb.SetMkfsTime(uint32(createTime.Unix()))

	if err := writeFullAt(fs.sb, fsUuid[:], 104); err != nil {
		return fmt.Errorf("write deterministic filesystem UUID: %w", err)
	}

	rootNode := fs.inodes[2]

	rootNode.node.SetCtime(uint32(createTime.Unix()))
	rootNode.node.SetMtime(uint32(createTime.Unix()))
	rootNode.node.SetAtime(uint32(createTime.Unix()))

	lostAndFoundNode := fs.inodes[11]

	lostAndFoundNode.node.SetCtime(uint32(createTime.Unix()))
	lostAndFoundNode.node.SetMtime(uint32(createTime.Unix()))
	lostAndFoundNode.node.SetAtime(uint32(createTime.Unix()))

	fs.deterministicTime = createTime

	return nil
}

func (fs *Ext4Filesystem) PrintStats() {
	slog.Debug("ext4 stats",
		"totalMapRegion", float64(totalMapRegion)/1000/1000,
		"totalAllocateInode", float64(totalAllocateInode)/1000/1000,
		"totalAddContents", float64(totalAddContents)/1000/1000,
		"totalAllocateBlocks", float64(totalAllocateBlocks)/1000/1000,
	)
}

func roundUpDiv[T integer](x, y T) T {
	return 1 + (x-1)/y
}

func (sb *Superblock) blockGroupCount() uint64 {
	return roundUpDiv(sb.BlocksCount(), uint64(sb.BlocksPerGroup()))
}

func (sb *Superblock) blockSize() uint64 {
	return uint64(math.Pow(2, float64(10+sb.LogBlockSize())))
}

// This function reinterprets the relevant regions of the existing memory map to match with the Ext4 structure.
func MountExt4Filesystem(_vm *vm.VirtualMemory, offset int64) (*Ext4Filesystem, error) {
	currentOffset := offset + 1024

	fs := &Ext4Filesystem{
		vm:     _vm,
		sb:     &Superblock{},
		inodes: make(map[int]*InodeWrapper),
	}

	// Map the super block.
	if err := fs.vm.Reinterpret(fs.sb, currentOffset); err != nil {
		return nil, fmt.Errorf("map existing superblock: %w", err)
	}
	if fs.sb.blockSize() == 1024 {
		currentOffset += fs.sb.Size()
	} else {
		currentOffset = int64(fs.sb.blockSize())
	}

	slog.Debug("superblock",
		"blockSize", fs.sb.blockSize(),
		"blockGroupCount", fs.sb.blockGroupCount(),
		"sb", fs.sb,
	)

	// pb := progressbar.Default(int64(fs.sb.InodesCount()))
	// defer pb.Close()

	inodeNumber := 1

	// Parse all the block group descriptors.
	for i := 0; i < int(fs.sb.blockGroupCount()); i++ {
		bg := &BlockGroup{
			num:         i,
			offset:      currentOffset,
			desc:        &BlockGroupDescriptor{},
			inodeBitmap: vm.NewBitmap(uint64(fs.sb.InodesPerGroup())),
			blockBitmap: vm.NewBitmap(uint64(fs.sb.BlocksPerGroup())),
			firstBlock:  int64(i) * int64(fs.sb.BlocksPerGroup()),
		}

		if err := fs.vm.Reinterpret(bg.desc, currentOffset); err != nil {
			return nil, fmt.Errorf("map existing block group %d: %w", i, err)
		}
		currentOffset += bg.desc.Size()

		slog.Debug("blockGroup",
			"offset", bg.offset,
			"i", i,
			"inodeTable", bg.desc.InodeTable(),
			"blockBitmap", bg.desc.BlockBitmap(),
			"inodeBitmap", bg.desc.InodeBitmap(),
			"bgd", bg.desc,
		)

		if bg.desc.BlockBitmap() == 0 {
			return nil, fmt.Errorf("invalid block bitmap location")
		}

		// map the inode bitmap and block bitmap.
		if err := fs.vm.Reinterpret(bg.blockBitmap, int64(bg.desc.BlockBitmap()*fs.sb.blockSize())); err != nil {
			return nil, err
		}
		if err := fs.vm.Reinterpret(bg.inodeBitmap, int64(bg.desc.InodeBitmap()*fs.sb.blockSize())); err != nil {
			return nil, err
		}

		inodeOffset := (bg.desc.InodeTable() * fs.sb.blockSize())

		for y := 0; y < int(fs.sb.InodesPerGroup()); y++ {
			ok, err := bg.inodeBitmap.Get(uint64(y))
			if err != nil {
				return nil, err
			}

			if !ok {
				continue
			}

			inode := &InodeWrapper{
				fs:     fs,
				bg:     bg,
				offset: inodeOffset,
				num:    inodeNumber,
				node:   &Inode{},
			}

			inodeNumber += 1

			if err := fs.vm.Reinterpret(inode.node, int64(inode.offset)); err != nil {
				return nil, err
			}
			inodeOffset += uint64(inode.node.Size())

			if inode.Mode().IsRegular() || inode.Mode().IsDir() {
				extent, err := mapExtentTree(fs.vm, inode.offset+40)
				if err != nil {
					return nil, err
				}

				inode.extentTree = extent

				if inode.Mode().IsDir() && extent != nil {
					if inode.Flags()&InodeFlag_INDEX != 0 {
						dir, err := mapHashedDirectory(fs, extent)
						if err != nil {
							return nil, err
						}

						inode.dir = dir
					} else {
						dir, err := mapLinearDirectory(fs, extent)
						if err != nil {
							return nil, err
						}

						inode.dir = dir
					}
				}
			}

			slog.Debug("", "inode", inode)

			fs.inodes[inode.num] = inode

			// pb.Add(1)
		}

		fs.bgs = append(fs.bgs, bg)
	}

	return fs, nil
}

func CreateExt4Filesystem(_vm *vm.VirtualMemory, offset int64, size int64) (*Ext4Filesystem, error) {
	start := time.Now()

	inodesPerGroup := 8192
	blockSize := 4096
	blockCount := roundUpDiv(size, int64(blockSize))
	blocksPerGroup := 32768
	blockGroupCount := roundUpDiv(blockCount, int64(blocksPerGroup))
	inodeCount := blockGroupCount * int64(inodesPerGroup)

	slog.Debug("making exr4 filesystem", "vmPageSize", _vm.PageSize(), "blocks", blockCount, "inodes", inodeCount, "blockGroups", blockGroupCount)

	fs := &Ext4Filesystem{
		vm:         _vm,
		sb:         &Superblock{},
		inodes:     make(map[int]*InodeWrapper),
		inodeCache: make(map[string]*InodeWrapper),
	}

	// Map the superblock.
	if err := fs.mapRegion(fs.sb, 1024); err != nil {
		return nil, err
	}

	// Initialize the superblock.
	fs.sb.SetMagic(61267)
	fs.sb.SetBlocksCount(uint64(blockCount))
	fs.sb.SetInodesCount(uint32(inodeCount))
	fs.sb.SetRBlocksCount(3276)
	fs.sb.SetLogBlockSize(2)
	fs.sb.SetLogClusterSize(2)
	fs.sb.SetBlocksPerGroup(uint32(blocksPerGroup))
	fs.sb.SetClustersPerGroup(uint32(blocksPerGroup))
	fs.sb.SetInodesPerGroup(uint32(inodesPerGroup))
	fs.sb.SetFreeInodesCount(uint32(inodeCount))
	fs.sb.SetFreeBlocksCount(uint64(blockCount) - 1)
	fs.sb.SetMaxMntCount(65535)
	fs.sb.SetLastcheck(uint32(time.Now().Unix()))
	fs.sb.SetMkfsTime(uint32(time.Now().Unix()))
	fs.sb.SetInodeSize(INODE_SIZE)
	fs.sb.SetDescSize(64)
	fs.sb.SetRevLevel(1)
	fs.sb.SetFirstIno(11)
	fs.sb.SetState(1)
	fs.sb.SetErrors(2)
	fs.sb.SetLogGroupsPerFlex(4)
	fs.sb.SetFirstDataBlock(0)

	uuid, err := randomUUID()
	if err != nil {
		return nil, err
	}

	// Set uuid
	if err := writeFullAt(fs.sb, uuid[:], 104); err != nil {
		return nil, fmt.Errorf("write filesystem UUID: %w", err)
	}

	// Set feature flags.
	fs.sb.SetFeatureCompat(
		uint32(Feature_compat_COMPAT_SPARSE_SUPER2),
	)
	fs.sb.SetFeatureIncompat(
		uint32(Feature_incompat_INCOMPAT_64BIT) |
			uint32(Feature_incompat_INCOMPAT_FILETYPE) |
			uint32(Feature_incompat_INCOMPAT_EXTENTS) |
			uint32(Feature_incompat_INCOMPAT_FLEX_BG),
	)
	fs.sb.SetFeatureRoCompat(
		uint32(Feature_ro_compat_RO_COMPAT_SPARSE_SUPER) |
			uint32(Feature_ro_compat_RO_COMPAT_LARGE_FILE) |
			uint32(Feature_ro_compat_RO_COMPAT_HUGE_FILE),
	)

	// Map and initialize all the block group descriptors.
	var blockGroupOffset int64
	if fs.sb.blockSize() == 1024 {
		return nil, fmt.Errorf("block size of 1024 not implemented")
	} else {
		blockGroupOffset = int64(fs.sb.blockSize())
	}

	for i := 0; i < int(blockGroupCount); i++ {
		inodeBitmapSize := roundUpDiv(uint64(fs.sb.InodesPerGroup())/8, fs.sb.blockSize()) * fs.sb.blockSize() * 8
		blockBitmapSize := roundUpDiv(uint64(fs.sb.BlocksPerGroup())/8, fs.sb.blockSize()) * fs.sb.blockSize() * 8

		bg := &BlockGroup{
			fs: fs,

			num:        i,
			offset:     blockGroupOffset,
			desc:       &BlockGroupDescriptor{},
			firstBlock: int64(i) * int64(blocksPerGroup),

			inodeBitmap: vm.NewBitmap(inodeBitmapSize),
			blockBitmap: vm.NewBitmap(blockBitmapSize),

			inodeCount: uint32(min(inodeCount, int64(fs.sb.InodesPerGroup()))),
			blockCount: uint32(min(blockCount, int64(fs.sb.BlocksPerGroup()))),
		}

		for x := bg.inodeCount; x < uint32(inodeBitmapSize); x += 1 {
			if err := bg.inodeBitmap.Set(uint64(x), true); err != nil {
				return nil, err
			}
		}

		for x := bg.blockCount; x < uint32(blockBitmapSize); x += 1 {
			if err := bg.blockBitmap.Set(uint64(x), true); err != nil {
				return nil, err
			}
		}
		bg.desc.SetFreeBlocksCount(uint32(bg.blockCount))
		bg.desc.SetFreeInodesCount(uint32(bg.inodeCount))
		bg.desc.SetFlags(4)

		fs.bgs = append(fs.bgs, bg)

		if i == 0 {
			if fs.sb.blockSize() == 1024 {
				return nil, fmt.Errorf("block size of 1024 not implemented")
			} else {
				// Reserve block zero for the superblock. It cannot be represented as an
				// extent because zero is the null physical-block pointer.
				if err := fs.reserveSuperblockBlock(); err != nil {
					return nil, fmt.Errorf("reserve superblock block: %w", err)
				}

				_, err = fs.allocateBlocksForBytes(blockGroupCount * BlockGroupDescriptor{}.Size())
				if err != nil {
					return nil, fmt.Errorf("allocate block group descriptor blocks: %w", err)
				}
			}
		}

		if err := fs.mapRegion(bg.desc, blockGroupOffset); err != nil {
			return nil, fmt.Errorf("map block group %d descriptor: %w", i, err)
		}
		blockGroupOffset += bg.desc.Size()

		var extent *Extent
		var err error

		// map the inode bitmap and block bitmap.
		extent, err = fs.allocateBlocksForBytes(bg.blockBitmap.Size())
		if err != nil {
			return nil, err
		}
		bg.desc.SetBlockBitmap(extent.StartBlock)
		if err := fs.mapExtent(bg.blockBitmap, extent); err != nil {
			return nil, err
		}

		extent, err = fs.allocateBlocksForBytes(bg.inodeBitmap.Size())
		if err != nil {
			return nil, err
		}
		bg.desc.SetInodeBitmap(extent.StartBlock)
		if err := fs.mapExtent(bg.inodeBitmap, extent); err != nil {
			return nil, err
		}

		// map the inode table.
		extent, err = fs.allocateBlocksForBytes(int64(fs.sb.InodeSize()) * int64(bg.inodeCount))
		if err != nil {
			return nil, err
		}
		bg.desc.SetInodeTable(extent.StartBlock)

		// slog.Debug("", "block bitmap", bg.desc.blockBitmapBlock(), "inode bitmap", bg.desc.inodeBitmapBlock(), "inode table", bg.desc.inodeTableBlock())
	}

	// Create the set of default inodes and the root directory.
	var root *InodeWrapper

	for i := 0; i < 11; i++ {
		inode, err := fs.allocateInode()
		if err != nil {
			return nil, err
		}

		if inode.num == 2 {
			// root directory.

			err := inode.allocateDirectory(inode)
			if err != nil {
				return nil, err
			}

			root = inode

			if err := root.chmod(goFs.FileMode(0755)); err != nil {
				return nil, err
			}
		} else if inode.num == 11 {
			// lost + found directory

			err := inode.allocateDirectory(root)
			if err != nil {
				return nil, err
			}

			if err := inode.chmod(goFs.FileMode(0700)); err != nil {
				return nil, err
			}

			if err := root.addDirectoryEntry(inode, "lost+found"); err != nil {
				return nil, err
			}
		}
	}

	// Map a page at the very end of the storage to set the size.
	if err := fs.mapRegion(make(vm.RawRegion, 1), size-1); err != nil {
		return nil, err
	}

	slog.Debug("created fs", "time", time.Since(start))

	return fs, nil
}

func writeFullAt(dst io.WriterAt, p []byte, off int64) error {
	n, err := dst.WriteAt(p, off)
	if err != nil {
		return fmt.Errorf("write at offset %d: %w", off, err)
	}
	if n != len(p) {
		return fmt.Errorf("write at offset %d wrote %d of %d bytes: %w", off, n, len(p), io.ErrShortWrite)
	}
	return nil
}

func randomUUID() (UUID, error) {
	var id UUID
	if _, err := rand.Read(id[:]); err != nil {
		return UUID{}, fmt.Errorf("generate filesystem uuid: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id, nil
}
