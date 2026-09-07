package fsimage

import (
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strings"

	fscommon "github.com/tinyrange/crumblecracker/internal/core/fsimage/common"
	"github.com/tinyrange/crumblecracker/internal/core/fsimage/fat"
	fatvm "github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

const (
	defaultFATSize = 64 << 20
	fatPageSize    = 16 << 10
)

func buildFAT(ctx context.Context, root imagefs.Directory, opts Options) (*fatvm.VirtualMemory, error) {
	if root == nil {
		return nil, fmt.Errorf("root filesystem is required")
	}
	stats, err := scanFAT(ctx, root, "/")
	if err != nil {
		return nil, err
	}
	size := opts.SizeBytes
	if size == 0 {
		size = estimateFATSize(stats, opts.ExtraBytes)
	}
	size = roundUp(size, 4096)
	vm := fatvm.NewVirtualMemory(size, fatPageSize)
	config := fatConfig(opts)
	fatType := strings.TrimSpace(opts.FATType)
	if fatType == "" {
		fatType = "fat32"
	}
	writer, err := fat.CreateFATFileSystemWithTypeAndConfig(vm, size, fatType, config)
	if err != nil {
		return nil, err
	}
	rootNode, err := writer.WritableRootDirectory()
	if err != nil {
		return nil, err
	}
	if err := populateFATDir(ctx, writer, root, rootNode, "/"); err != nil {
		return nil, err
	}
	if err := writer.Finalize(); err != nil {
		return nil, fmt.Errorf("finalize fat filesystem: %w", err)
	}
	return vm, nil
}

func fatConfig(opts Options) *fat.DeterministicConfig {
	if opts.DeterministicTime.IsZero() && opts.FATVolumeSerial == nil {
		return nil
	}
	config := fat.DefaultDeterministicConfig()
	if !opts.DeterministicTime.IsZero() {
		t := opts.DeterministicTime
		config.FixedTimestamp = &t
	}
	if opts.FATVolumeSerial != nil {
		serial := *opts.FATVolumeSerial
		config.VolumeSerial = &serial
	}
	return config
}

type fatStats struct {
	files uint64
	dirs  uint64
	bytes uint64
}

func scanFAT(ctx context.Context, dir imagefs.Directory, guestPath string) (fatStats, error) {
	if err := ctx.Err(); err != nil {
		return fatStats{}, err
	}
	stats := fatStats{dirs: 1}
	children, err := dir.ReadDir()
	if err != nil {
		return fatStats{}, fmt.Errorf("read %s: %w", guestPath, err)
	}
	for _, child := range sortedImageDirEnts(children) {
		if child.Name == "." || child.Name == ".." {
			continue
		}
		childPath := path.Join(guestPath, child.Name)
		entry, err := dir.Lookup(child.Name)
		if err != nil {
			return fatStats{}, fmt.Errorf("lookup %s: %w", childPath, err)
		}
		switch {
		case entry.Dir != nil:
			childStats, err := scanFAT(ctx, entry.Dir, childPath)
			if err != nil {
				return fatStats{}, err
			}
			stats.files += childStats.files
			stats.dirs += childStats.dirs
			stats.bytes += childStats.bytes
		case entry.Symlink != nil:
			return fatStats{}, fmt.Errorf("%s is a symlink; FAT filesystem writer does not support symlinks", childPath)
		case entry.File != nil:
			size, mode := entry.File.Stat()
			if mode.Type() != 0 {
				return fatStats{}, fmt.Errorf("%s has unsupported file type %s for FAT filesystem", childPath, mode.Type())
			}
			if size > math.MaxInt64 {
				return fatStats{}, fmt.Errorf("%s is too large for FAT filesystem writer: %d bytes", childPath, size)
			}
			stats.files++
			stats.bytes += size
		default:
			return fatStats{}, fmt.Errorf("%s has no filesystem entry", childPath)
		}
	}
	return stats, nil
}

func populateFATDir(ctx context.Context, writer *fat.FATWriter, dir imagefs.Directory, node fscommon.WritableNode, guestPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	children, err := dir.ReadDir()
	if err != nil {
		return fmt.Errorf("read %s: %w", guestPath, err)
	}
	childNodes := make([]fscommon.WritableNode, 0, len(children))
	for _, child := range sortedImageDirEnts(children) {
		if child.Name == "." || child.Name == ".." {
			continue
		}
		childPath := path.Join(guestPath, child.Name)
		entry, err := dir.Lookup(child.Name)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", childPath, err)
		}
		childNode, err := writer.AllocateNode()
		if err != nil {
			return fmt.Errorf("allocate %s: %w", childPath, err)
		}
		if err := childNode.SetName(child.Name); err != nil {
			return fmt.Errorf("set FAT name %s: %w", childPath, err)
		}
		switch {
		case entry.Dir != nil:
			if fatNode, ok := childNode.(*fat.FATWritableNode); ok {
				fatNode.SetDir()
			} else {
				return fmt.Errorf("allocate %s: unexpected FAT node type", childPath)
			}
			if err := populateFATDir(ctx, writer, entry.Dir, childNode, childPath); err != nil {
				return err
			}
		case entry.File != nil:
			size, mode := entry.File.Stat()
			if mode.Type() != 0 {
				return fmt.Errorf("%s has unsupported file type %s for FAT filesystem", childPath, mode.Type())
			}
			if err := writer.WriteContents(childNode, fatImageFileRegion{file: entry.File, size: int64(size)}); err != nil {
				return fmt.Errorf("write %s: %w", childPath, err)
			}
		case entry.Symlink != nil:
			return fmt.Errorf("%s is a symlink; FAT filesystem writer does not support symlinks", childPath)
		default:
			return fmt.Errorf("%s has no filesystem entry", childPath)
		}
		childNodes = append(childNodes, childNode)
	}
	if err := writer.WriteDirectory(node, childNodes); err != nil {
		return fmt.Errorf("write directory %s: %w", guestPath, err)
	}
	return nil
}

type fatImageFileRegion struct {
	file imagefs.File
	size int64
}

func (r fatImageFileRegion) Size() int64 { return r.size }

func (r fatImageFileRegion) ReadAt(p []byte, off int64) (int, error) {
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

func (r fatImageFileRegion) WriteAt([]byte, int64) (int, error) {
	return 0, fmt.Errorf("region is read only")
}

func estimateFATSize(stats fatStats, extra int64) int64 {
	size := int64(stats.bytes)
	size += int64(stats.files+stats.dirs) * 8192
	size += size / 4
	size += 16 << 20
	size += extra
	if size < defaultFATSize {
		size = defaultFATSize
	}
	return size
}

func sortedImageDirEnts(entries []imagefs.DirEnt) []imagefs.DirEnt {
	out := append([]imagefs.DirEnt(nil), entries...)
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i].Name, out[j].Name) < 0
	})
	return out
}

var _ fatvm.MemoryRegion = fatImageFileRegion{}
