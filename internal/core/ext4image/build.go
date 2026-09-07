package ext4image

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/ext4image/ext4"
	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

const (
	defaultImageSize = 64 << 20
	defaultPageSize  = 16 << 10
)

type Options struct {
	SizeBytes         int64
	ExtraBytes        int64
	DeterministicTime time.Time
	UUID              ext4.UUID
}

func Build(ctx context.Context, root imagefs.Directory, opts Options) (*vm.VirtualMemory, error) {
	if root == nil {
		return nil, fmt.Errorf("root filesystem is required")
	}
	stats, err := scan(ctx, root, "/")
	if err != nil {
		return nil, err
	}
	size := opts.SizeBytes
	if size == 0 {
		size = estimateSize(stats, opts.ExtraBytes)
	}
	size = roundUp(size, 4096)
	vm := vm.NewVirtualMemory(size, defaultPageSize)
	out, err := ext4.CreateExt4Filesystem(vm, 0, size)
	if err != nil {
		return nil, err
	}
	if !opts.DeterministicTime.IsZero() {
		if err := out.MakeDeterministic(opts.UUID, opts.DeterministicTime); err != nil {
			return nil, err
		}
	}
	if err := populate(ctx, out, root, "/"); err != nil {
		return nil, err
	}
	return vm, nil
}

func Write(ctx context.Context, w io.Writer, root imagefs.Directory, opts Options) error {
	vm, err := Build(ctx, root, opts)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, io.NewSectionReader(vm, 0, vm.Size()))
	return err
}

func WriteFile(ctx context.Context, filename string, root imagefs.Directory, opts Options) error {
	tmp := filename + ".tmp"
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return fmt.Errorf("mkdir image dir: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create ext4 image: %w", err)
	}
	if err := Write(ctx, f, root, opts); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close ext4 image: %w", err)
	}
	if err := os.Rename(tmp, filename); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("activate ext4 image: %w", err)
	}
	return nil
}

type treeStats struct {
	files uint64
	dirs  uint64
	links uint64
	bytes uint64
}

func scan(ctx context.Context, dir imagefs.Directory, guestPath string) (treeStats, error) {
	if err := ctx.Err(); err != nil {
		return treeStats{}, err
	}
	stats := treeStats{dirs: 1}
	children, err := dir.ReadDir()
	if err != nil {
		return treeStats{}, fmt.Errorf("read %s: %w", guestPath, err)
	}
	for _, child := range sortedDirEnts(children) {
		if child.Name == "." || child.Name == ".." {
			continue
		}
		childPath := path.Join(guestPath, child.Name)
		entry, err := dir.Lookup(child.Name)
		if err != nil {
			return treeStats{}, fmt.Errorf("lookup %s: %w", childPath, err)
		}
		switch {
		case entry.Dir != nil:
			childStats, err := scan(ctx, entry.Dir, childPath)
			if err != nil {
				return treeStats{}, err
			}
			stats.files += childStats.files
			stats.dirs += childStats.dirs
			stats.links += childStats.links
			stats.bytes += childStats.bytes
		case entry.Symlink != nil:
			stats.links++
			stats.bytes += uint64(len(entry.Symlink.Target()))
		case entry.File != nil:
			size, mode := entry.File.Stat()
			if mode.Type() == 0 {
				if size > maxSupportedFileSize() {
					extentBytes := uint64(32768 * 4096)
					requiredExtents := int64((size-1)/extentBytes + 1)
					return treeStats{}, fmt.Errorf("%s: %w", childPath, &ext4.ExtentLimitError{
						RequiredExtents:  requiredExtents,
						SupportedExtents: ext4.MaxInlineExtents,
					})
				}
				stats.files++
				stats.bytes += size
			}
		}
	}
	return stats, nil
}

func populate(ctx context.Context, out *ext4.Ext4Filesystem, dir imagefs.Directory, guestPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := applyDirMetadata(out, guestPath, dir); err != nil {
		return err
	}
	children, err := dir.ReadDir()
	if err != nil {
		return fmt.Errorf("read %s: %w", guestPath, err)
	}
	for _, child := range sortedDirEnts(children) {
		if child.Name == "." || child.Name == ".." {
			continue
		}
		childPath := path.Join(guestPath, child.Name)
		entry, err := dir.Lookup(child.Name)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", childPath, err)
		}
		if err := populateEntry(ctx, out, entry, childPath); err != nil {
			return err
		}
	}
	return nil
}

func populateEntry(ctx context.Context, out *ext4.Ext4Filesystem, entry imagefs.Entry, guestPath string) error {
	switch {
	case entry.Dir != nil:
		if guestPath != "/" {
			if err := out.Mkdir(guestPath, true); err != nil && !out.Exists(guestPath) {
				return fmt.Errorf("mkdir %s: %w", guestPath, err)
			}
		}
		return populate(ctx, out, entry.Dir, guestPath)
	case entry.Symlink != nil:
		if err := out.Symlink(guestPath, entry.Symlink.Target()); err != nil {
			return fmt.Errorf("symlink %s: %w", guestPath, err)
		}
		return applySymlinkMetadata(out, guestPath, entry.Symlink)
	case entry.File != nil:
		size, mode := entry.File.Stat()
		if mode.Type() != 0 {
			return nil
		}
		if err := out.CreateFile(guestPath, imageFileRegion{file: entry.File, size: int64(size)}); err != nil {
			return fmt.Errorf("create %s: %w", guestPath, err)
		}
		return applyFileMetadata(out, guestPath, entry.File)
	default:
		return fmt.Errorf("%s has no filesystem entry", guestPath)
	}
}

func applyDirMetadata(out *ext4.Ext4Filesystem, guestPath string, dir imagefs.Directory) error {
	mode := fs.ModeDir | dir.Stat()
	if err := out.Chmod(guestPath, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", guestPath, err)
	}
	uid, gid := dir.Owner()
	if err := chown(out, guestPath, uid, gid); err != nil {
		return err
	}
	return chtime(out, guestPath, dir.ModTime())
}

func applyFileMetadata(out *ext4.Ext4Filesystem, guestPath string, file imagefs.File) error {
	_, mode := file.Stat()
	if err := out.Chmod(guestPath, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", guestPath, err)
	}
	uid, gid := file.Owner()
	if err := chown(out, guestPath, uid, gid); err != nil {
		return err
	}
	return chtime(out, guestPath, file.ModTime())
}

func applySymlinkMetadata(out *ext4.Ext4Filesystem, guestPath string, link imagefs.Symlink) error {
	uid, gid := link.Owner()
	if err := chown(out, guestPath, uid, gid); err != nil {
		return err
	}
	return chtime(out, guestPath, link.ModTime())
}

func chown(out *ext4.Ext4Filesystem, guestPath string, uid, gid uint32) error {
	if uid > math.MaxUint16 || gid > math.MaxUint16 {
		return fmt.Errorf("chown %s: uid/gid exceeds ext4 writer limit: %d:%d", guestPath, uid, gid)
	}
	if err := out.Chown(guestPath, uint16(uid), uint16(gid)); err != nil {
		return fmt.Errorf("chown %s: %w", guestPath, err)
	}
	return nil
}

func chtime(out *ext4.Ext4Filesystem, guestPath string, mod time.Time) error {
	if mod.IsZero() {
		return nil
	}
	if err := out.Chtimes(guestPath, mod); err != nil {
		return fmt.Errorf("chtime %s: %w", guestPath, err)
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

func estimateSize(stats treeStats, extra int64) int64 {
	size := int64(stats.bytes)
	size += int64(stats.files+stats.dirs+stats.links) * 8192
	size += size / 4
	size += 32 << 20
	size += extra
	if size < defaultImageSize {
		size = defaultImageSize
	}
	return size
}

func roundUp(v, align int64) int64 {
	return ((v + align - 1) / align) * align
}

func maxSupportedFileSize() uint64 {
	return uint64(ext4.MaxInlineExtents * 32768 * 4096)
}

func sortedDirEnts(entries []imagefs.DirEnt) []imagefs.DirEnt {
	out := append([]imagefs.DirEnt(nil), entries...)
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i].Name, out[j].Name) < 0
	})
	return out
}
