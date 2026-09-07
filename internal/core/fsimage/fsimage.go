package fsimage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/ext4image"
	"github.com/tinyrange/crumblecracker/internal/core/ext4image/ext4"
	ffsimage "github.com/tinyrange/crumblecracker/internal/core/fsimage/ffs"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

type Type string

const (
	TypeExt4    Type = "ext4"
	TypeVFAT    Type = "vfat"
	TypeFFS     Type = "ffs"
	TypeISO9660 Type = "iso9660"
)

type Options struct {
	Type              Type
	SizeBytes         int64
	ExtraBytes        int64
	DeterministicTime time.Time
	Ext4UUID          ext4.UUID
	FFSLayout         ffsimage.Layout
	FATType           string
	FATVolumeSerial   *uint32
}

type FilesystemRegion interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
}

func ParseType(value string) (Type, error) {
	switch typ := Type(strings.ToLower(strings.TrimSpace(value))); typ {
	case "", TypeExt4:
		return TypeExt4, nil
	case TypeVFAT, TypeFFS, TypeISO9660:
		return typ, nil
	default:
		return "", fmt.Errorf("unsupported rootfs image type %q", value)
	}
}

func (typ Type) String() string {
	if typ == "" {
		return string(TypeExt4)
	}
	return string(typ)
}

func (typ Type) InitramfsPath() string {
	return "/ccx3/rootfs." + typ.String()
}

func Build(ctx context.Context, root imagefs.Directory, opts Options) (FilesystemRegion, error) {
	typ, err := ParseType(opts.Type.String())
	if err != nil {
		return nil, err
	}
	switch typ {
	case TypeExt4:
		return ext4image.Build(ctx, root, ext4Options(opts))
	case TypeVFAT:
		return buildFAT(ctx, root, opts)
	case TypeFFS:
		return ffsimage.Build(ctx, root, ffsOptions(opts))
	default:
		return nil, fmt.Errorf("rootfs image writer for type %q is not implemented", typ)
	}
}

func Write(ctx context.Context, w io.Writer, root imagefs.Directory, opts Options) error {
	typ, err := ParseType(opts.Type.String())
	if err != nil {
		return err
	}
	switch typ {
	case TypeExt4:
		return ext4image.Write(ctx, w, root, ext4Options(opts))
	case TypeVFAT:
		region, err := buildFAT(ctx, root, opts)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, io.NewSectionReader(region, 0, region.Size()))
		return err
	case TypeFFS:
		return ffsimage.Write(ctx, w, root, ffsOptions(opts))
	default:
		return fmt.Errorf("rootfs image writer for type %q is not implemented", typ)
	}
}

func WriteFile(ctx context.Context, filename string, root imagefs.Directory, opts Options) error {
	tmp := filename + ".tmp"
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return fmt.Errorf("mkdir image dir: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s image: %w", opts.Type.String(), err)
	}
	if err := Write(ctx, f, root, opts); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s image: %w", opts.Type.String(), err)
	}
	if err := os.Rename(tmp, filename); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("activate %s image: %w", opts.Type.String(), err)
	}
	return nil
}

func ext4Options(opts Options) ext4image.Options {
	return ext4image.Options{
		SizeBytes:         opts.SizeBytes,
		ExtraBytes:        opts.ExtraBytes,
		DeterministicTime: opts.DeterministicTime,
		UUID:              opts.Ext4UUID,
	}
}

func ffsOptions(opts Options) ffsimage.Options {
	return ffsimage.Options{
		SizeBytes:         opts.SizeBytes,
		ExtraBytes:        opts.ExtraBytes,
		DeterministicTime: opts.DeterministicTime,
		Layout:            opts.FFSLayout,
	}
}

func roundUp(v, align int64) int64 {
	return ((v + align - 1) / align) * align
}
