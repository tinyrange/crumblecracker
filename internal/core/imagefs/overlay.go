package imagefs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

type Overlay struct {
	root *overlayDir
}

func NewOverlay(base Directory) *Overlay {
	return &Overlay{
		root: &overlayDir{
			base:      base,
			mode:      fs.ModeDir | 0o755,
			modTime:   time.Now(),
			overrides: map[string]Entry{},
		},
	}
}

func (o *Overlay) Root() Directory {
	if o == nil {
		return nil
	}
	return o.root
}

func (o *Overlay) AddDir(guestPath string, mode fs.FileMode) error {
	_, err := o.ensureDir(cleanOverlayPath(guestPath), fs.ModeDir|(mode&0o7777))
	return err
}

func (o *Overlay) AddFile(guestPath string, mode fs.FileMode, data []byte) error {
	if o == nil {
		return fmt.Errorf("overlay is nil")
	}
	clean := cleanOverlayPath(guestPath)
	if clean == "/" {
		return fmt.Errorf("cannot replace overlay root with file")
	}
	parentPath := path.Dir(clean)
	name := path.Base(clean)
	parent, err := o.ensureDir(parentPath, fs.ModeDir|0o755)
	if err != nil {
		return err
	}
	parent.overrides[name] = Entry{File: &overlayFile{
		mode:    mode & 0o7777,
		size:    uint64(len(data)),
		data:    append([]byte(nil), data...),
		modTime: time.Now(),
	}}
	return nil
}

func (o *Overlay) AddDevice(guestPath string, mode fs.FileMode, rdev uint32) error {
	if o == nil {
		return fmt.Errorf("overlay is nil")
	}
	clean := cleanOverlayPath(guestPath)
	if clean == "/" {
		return fmt.Errorf("cannot replace overlay root with device")
	}
	parent, err := o.ensureDir(path.Dir(clean), fs.ModeDir|0o755)
	if err != nil {
		return err
	}
	parent.overrides[path.Base(clean)] = Entry{File: &overlayFile{
		mode:    mode,
		rdev:    rdev,
		modTime: time.Now(),
	}}
	return nil
}

func (o *Overlay) AddSymlink(guestPath, target string) error {
	if o == nil {
		return fmt.Errorf("overlay is nil")
	}
	clean := cleanOverlayPath(guestPath)
	if clean == "/" {
		return fmt.Errorf("cannot replace overlay root with symlink")
	}
	parent, err := o.ensureDir(path.Dir(clean), fs.ModeDir|0o755)
	if err != nil {
		return err
	}
	parent.overrides[path.Base(clean)] = Entry{Symlink: &overlaySymlink{
		mode:    fs.ModeSymlink | 0o777,
		target:  target,
		modTime: time.Now(),
	}}
	return nil
}

type overlayDir struct {
	base      Directory
	mode      fs.FileMode
	uid       uint32
	gid       uint32
	rdev      uint32
	modTime   time.Time
	overrides map[string]Entry
}

func (o *Overlay) ensureDir(guestPath string, mode fs.FileMode) (*overlayDir, error) {
	if o == nil {
		return nil, fmt.Errorf("overlay is nil")
	}
	clean := cleanOverlayPath(guestPath)
	current := o.root
	if clean == "/" {
		return current, nil
	}
	for _, name := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if name == "" {
			continue
		}
		if entry, ok := current.overrides[name]; ok {
			if entry.Dir == nil {
				return nil, fmt.Errorf("%q is not a directory", path.Join("/", strings.TrimPrefix(clean, "/")))
			}
			next, ok := entry.Dir.(*overlayDir)
			if !ok {
				return nil, fmt.Errorf("%q is not an overlay directory", name)
			}
			current = next
			continue
		}
		var baseDir Directory
		if current.base != nil {
			entry, err := current.base.Lookup(name)
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if err == nil {
				if entry.Dir == nil {
					return nil, fmt.Errorf("%q is not a directory", path.Join("/", name))
				}
				baseDir = entry.Dir
			}
		}
		next := &overlayDir{
			base:      baseDir,
			mode:      mode,
			modTime:   time.Now(),
			overrides: map[string]Entry{},
		}
		current.overrides[name] = Entry{Dir: next}
		current = next
	}
	return current, nil
}

func (d *overlayDir) Stat() fs.FileMode {
	if d.base != nil {
		return d.base.Stat()
	}
	return d.mode & 0o7777
}

func (d *overlayDir) ModTime() time.Time {
	if d.base != nil {
		return d.base.ModTime()
	}
	return d.modTime
}

func (d *overlayDir) Owner() (uint32, uint32) {
	if d.base != nil {
		return d.base.Owner()
	}
	return d.uid, d.gid
}

func (d *overlayDir) RDev() uint32 {
	if d.base != nil {
		return d.base.RDev()
	}
	return d.rdev
}

func (d *overlayDir) ReadDir() ([]DirEnt, error) {
	return d.ReadDirContext(context.Background())
}

func (d *overlayDir) ReadDirContext(ctx context.Context) ([]DirEnt, error) {
	merged := map[string]fs.FileMode{}
	if d.base != nil {
		baseEntries, err := ReadDirContext(ctx, d.base)
		if err != nil {
			return nil, err
		}
		for _, entry := range baseEntries {
			merged[entry.Name] = entry.Mode
		}
	}
	for name, entry := range d.overrides {
		switch {
		case entry.Dir != nil:
			merged[name] = fs.ModeDir | entry.Dir.Stat()
		case entry.Symlink != nil:
			merged[name] = fs.ModeSymlink | entry.Symlink.Stat()
		case entry.File != nil:
			_, mode := entry.File.Stat()
			merged[name] = mode
		}
	}
	out := make([]DirEnt, 0, len(merged))
	for name, mode := range merged {
		out = append(out, DirEnt{Name: name, Mode: mode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *overlayDir) Lookup(name string) (Entry, error) {
	return d.LookupContext(context.Background(), name)
}

func (d *overlayDir) LookupContext(ctx context.Context, name string) (Entry, error) {
	if entry, ok := d.overrides[name]; ok {
		return entry, nil
	}
	if d.base == nil {
		return Entry{}, os.ErrNotExist
	}
	return LookupContext(ctx, d.base, name)
}

type overlayFile struct {
	mode    fs.FileMode
	uid     uint32
	gid     uint32
	rdev    uint32
	size    uint64
	data    []byte
	modTime time.Time
}

type overlaySymlink struct {
	mode    fs.FileMode
	uid     uint32
	gid     uint32
	rdev    uint32
	target  string
	modTime time.Time
}

func (f *overlayFile) Stat() (uint64, fs.FileMode) { return f.size, f.mode }
func (f *overlayFile) ModTime() time.Time          { return f.modTime }
func (f *overlayFile) Owner() (uint32, uint32)     { return f.uid, f.gid }
func (f *overlayFile) RDev() uint32                { return f.rdev }
func (f *overlayFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	if off >= f.size || size == 0 {
		return nil, nil
	}
	end := off + uint64(size)
	if end > f.size {
		end = f.size
	}
	return append([]byte(nil), f.data[off:end]...), nil
}

func (l *overlaySymlink) Stat() fs.FileMode       { return l.mode & 0o7777 }
func (l *overlaySymlink) ModTime() time.Time      { return l.modTime }
func (l *overlaySymlink) Target() string          { return l.target }
func (l *overlaySymlink) Owner() (uint32, uint32) { return l.uid, l.gid }
func (l *overlaySymlink) RDev() uint32            { return l.rdev }

func cleanOverlayPath(guestPath string) string {
	return path.Clean("/" + strings.TrimPrefix(guestPath, "/"))
}
