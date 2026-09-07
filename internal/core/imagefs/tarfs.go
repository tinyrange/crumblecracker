package imagefs

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
)

type TarFS struct {
	root    *tarDir
	backing *os.File
	close   func() error
	tmpDir  string
	closed  bool
}

type TarFSOptions struct {
	Include func(name string, hdr *tar.Header) bool
}

func NewTarFS(ctx context.Context, r io.Reader) (*TarFS, error) {
	return NewTarFSWithOptions(ctx, r, TarFSOptions{})
}

func NewTarFSWithOptions(ctx context.Context, r io.Reader, opts TarFSOptions) (*TarFS, error) {
	tmpDir, err := os.MkdirTemp("", "cc-imagefs-tar-*")
	if err != nil {
		return nil, fmt.Errorf("create tarfs temp dir: %w", err)
	}
	backing, err := os.OpenFile(path.Join(tmpDir, "payloads"), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("create tarfs backing file: %w", err)
	}
	tfs := &TarFS{
		root: &tarDir{
			node: tarNode{
				name:    "",
				mode:    fs.ModeDir | 0o755,
				modTime: time.Unix(0, 0),
			},
			children: map[string]tarEntry{},
		},
		backing: backing,
		tmpDir:  tmpDir,
	}
	tfs.close = func() error {
		err := backing.Close()
		if removeErr := os.RemoveAll(tmpDir); err == nil {
			err = removeErr
		}
		return err
	}
	if err := tfs.read(ctx, r, opts); err != nil {
		_ = tfs.Close()
		return nil, err
	}
	return tfs, nil
}

func NewSeekableTarFS(ctx context.Context, filename string) (*TarFS, error) {
	return NewSeekableTarFSWithOptions(ctx, filename, TarFSOptions{})
}

func NewSeekableTarFSWithOptions(ctx context.Context, filename string, opts TarFSOptions) (*TarFS, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open tarfs backing file: %w", err)
	}
	tfs := &TarFS{
		root: &tarDir{
			node: tarNode{
				name:    "",
				mode:    fs.ModeDir | 0o755,
				modTime: time.Unix(0, 0),
			},
			children: map[string]tarEntry{},
		},
		backing: f,
		close:   f.Close,
	}
	if err := tfs.readSeekable(ctx, f, opts); err != nil {
		_ = tfs.Close()
		return nil, err
	}
	return tfs, nil
}

func (t *TarFS) Root() Directory {
	if t == nil {
		return nil
	}
	return t.root
}

func (t *TarFS) Close() error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true
	if t.close != nil {
		return t.close()
	}
	return nil
}

func (t *TarFS) read(ctx context.Context, r io.Reader, opts TarFSOptions) error {
	byPath := map[string]tarEntry{"/": {Dir: t.root}}
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		metadata, err := validateTarHeaderMetadata(hdr)
		if err != nil {
			return fmt.Errorf("validate %q metadata: %w", hdr.Name, err)
		}
		clean := cleanTarPath(hdr.Name)
		if clean == "/" {
			continue
		}
		if opts.Include != nil && !opts.Include(clean, hdr) {
			continue
		}
		parent, name, err := t.ensureParent(clean, hdr.ModTime, byPath)
		if err != nil {
			return err
		}
		entry, err := t.entryFromHeader(hdr, metadata, tr, clean, byPath)
		if err != nil {
			return err
		}
		parent.children[name] = entry
		byPath[clean] = entry
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func (t *TarFS) readSeekable(ctx context.Context, r io.Reader, opts TarFSOptions) error {
	byPath := map[string]tarEntry{"/": {Dir: t.root}}
	cr := &countingReader{r: r}
	tr := tar.NewReader(cr)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		metadata, err := validateTarHeaderMetadata(hdr)
		if err != nil {
			return fmt.Errorf("validate %q metadata: %w", hdr.Name, err)
		}
		clean := cleanTarPath(hdr.Name)
		if clean == "/" {
			continue
		}
		included := opts.Include == nil || opts.Include(clean, hdr)
		if !included {
			if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
				if err := validateTarPayloadRange(cr.n, hdr.Size); err != nil {
					return fmt.Errorf("validate %s payload: %w", clean, err)
				}
				if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
					return fmt.Errorf("skip %s payload: %w", clean, err)
				}
			}
			continue
		}
		parent, name, err := t.ensureParent(clean, hdr.ModTime, byPath)
		if err != nil {
			return err
		}
		entry, err := t.entryFromSeekableHeader(hdr, metadata, tr, clean, cr.n, byPath)
		if err != nil {
			return err
		}
		parent.children[name] = entry
		byPath[clean] = entry
	}
}

type tarHeaderMetadata struct {
	size uint64
	uid  uint32
	gid  uint32
	rdev uint32
}

func validateTarHeaderMetadata(hdr *tar.Header) (tarHeaderMetadata, error) {
	if hdr.Size < 0 {
		return tarHeaderMetadata{}, fmt.Errorf("negative size")
	}
	if hdr.Uid < 0 || uint64(hdr.Uid) > math.MaxUint32 {
		return tarHeaderMetadata{}, fmt.Errorf("uid is outside uint32 range")
	}
	if hdr.Gid < 0 || uint64(hdr.Gid) > math.MaxUint32 {
		return tarHeaderMetadata{}, fmt.Errorf("gid is outside uint32 range")
	}
	rdev, err := tarRDev(hdr)
	if err != nil {
		return tarHeaderMetadata{}, err
	}
	return tarHeaderMetadata{
		size: uint64(hdr.Size),
		uid:  uint32(hdr.Uid),
		gid:  uint32(hdr.Gid),
		rdev: rdev,
	}, nil
}

func validateTarPayloadRange(offset, size int64) error {
	if offset < 0 {
		return fmt.Errorf("negative backing offset")
	}
	if size < 0 {
		return fmt.Errorf("negative size")
	}
	if size > math.MaxInt64-offset {
		return fmt.Errorf("backing offset and size overflow int64")
	}
	return nil
}

func (t *TarFS) entryFromHeader(hdr *tar.Header, metadata tarHeaderMetadata, tr *tar.Reader, clean string, byPath map[string]tarEntry) (tarEntry, error) {
	mode := linuxModeToGo(fsmeta.LinuxModeFromTarHeader(hdr))
	node := tarNode{
		name:    path.Base(clean),
		mode:    mode,
		uid:     metadata.uid,
		gid:     metadata.gid,
		rdev:    metadata.rdev,
		modTime: hdr.ModTime,
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		dir := &tarDir{node: node, children: map[string]tarEntry{}}
		if existing, ok := byPath[clean]; ok && existing.Dir != nil {
			existing.Dir.node = node
			return existing, nil
		}
		return tarEntry{Dir: dir}, nil
	case tar.TypeSymlink:
		return tarEntry{Symlink: &tarSymlink{node: node, target: fsmeta.NormalizeSymlinkTarget(hdr.Linkname)}}, nil
	case tar.TypeLink:
		target := cleanTarPath(hdr.Linkname)
		entry, ok := byPath[target]
		if !ok || entry.File == nil {
			return tarEntry{}, fmt.Errorf("%s hardlink target %s not found", clean, target)
		}
		entry.File.addLink(clean)
		return tarEntry{File: entry.File}, nil
	case tar.TypeReg, tar.TypeRegA:
		offset, err := t.backing.Seek(0, io.SeekEnd)
		if err != nil {
			return tarEntry{}, fmt.Errorf("seek tarfs backing: %w", err)
		}
		if err := validateTarPayloadRange(offset, hdr.Size); err != nil {
			return tarEntry{}, fmt.Errorf("validate %s payload: %w", clean, err)
		}
		if _, err := io.CopyN(t.backing, tr, hdr.Size); err != nil {
			return tarEntry{}, fmt.Errorf("copy %s payload: %w", clean, err)
		}
		file := &tarFile{
			node:    node,
			backing: t.backing,
			offset:  offset,
			size:    metadata.size,
			key:     clean,
		}
		return tarEntry{File: file}, nil
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return tarEntry{File: &tarFile{node: node, key: clean}}, nil
	default:
		return tarEntry{}, fmt.Errorf("%s has unsupported tar entry type %q", clean, hdr.Typeflag)
	}
}

func (t *TarFS) entryFromSeekableHeader(hdr *tar.Header, metadata tarHeaderMetadata, tr *tar.Reader, clean string, dataOffset int64, byPath map[string]tarEntry) (tarEntry, error) {
	mode := linuxModeToGo(fsmeta.LinuxModeFromTarHeader(hdr))
	node := tarNode{
		name:    path.Base(clean),
		mode:    mode,
		uid:     metadata.uid,
		gid:     metadata.gid,
		rdev:    metadata.rdev,
		modTime: hdr.ModTime,
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		dir := &tarDir{node: node, children: map[string]tarEntry{}}
		if existing, ok := byPath[clean]; ok && existing.Dir != nil {
			existing.Dir.node = node
			return existing, nil
		}
		return tarEntry{Dir: dir}, nil
	case tar.TypeSymlink:
		return tarEntry{Symlink: &tarSymlink{node: node, target: fsmeta.NormalizeSymlinkTarget(hdr.Linkname)}}, nil
	case tar.TypeLink:
		target := cleanTarPath(hdr.Linkname)
		entry, ok := byPath[target]
		if !ok || entry.File == nil {
			return tarEntry{}, fmt.Errorf("%s hardlink target %s not found", clean, target)
		}
		entry.File.addLink(clean)
		return tarEntry{File: entry.File}, nil
	case tar.TypeReg, tar.TypeRegA:
		if err := validateTarPayloadRange(dataOffset, hdr.Size); err != nil {
			return tarEntry{}, fmt.Errorf("validate %s payload: %w", clean, err)
		}
		if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
			return tarEntry{}, fmt.Errorf("skip %s payload: %w", clean, err)
		}
		file := &tarFile{
			node:    node,
			backing: t.backing,
			offset:  dataOffset,
			size:    metadata.size,
			key:     clean,
		}
		return tarEntry{File: file}, nil
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return tarEntry{File: &tarFile{node: node, key: clean}}, nil
	default:
		return tarEntry{}, fmt.Errorf("%s has unsupported tar entry type %q", clean, hdr.Typeflag)
	}
}

func (t *TarFS) ensureParent(clean string, modTime time.Time, byPath map[string]tarEntry) (*tarDir, string, error) {
	parentPath := path.Dir(clean)
	name := path.Base(clean)
	current := t.root
	if parentPath == "/" {
		return current, name, nil
	}
	currentPath := ""
	for _, part := range strings.Split(strings.TrimPrefix(parentPath, "/"), "/") {
		if part == "" {
			continue
		}
		currentPath = path.Join(currentPath, part)
		guestPath := "/" + currentPath
		entry, ok := current.children[part]
		if !ok {
			next := &tarDir{
				node: tarNode{
					name:    part,
					mode:    fs.ModeDir | 0o755,
					modTime: modTime,
				},
				children: map[string]tarEntry{},
			}
			entry := tarEntry{Dir: next}
			current.children[part] = entry
			byPath[guestPath] = entry
			current = next
			continue
		}
		if entry.Dir == nil {
			return nil, "", fmt.Errorf("%s parent %s is not a directory", clean, part)
		}
		current = entry.Dir
	}
	return current, name, nil
}

type tarEntry struct {
	File    *tarFile
	Dir     *tarDir
	Symlink *tarSymlink
}

type tarNode struct {
	name    string
	mode    fs.FileMode
	uid     uint32
	gid     uint32
	rdev    uint32
	modTime time.Time
}

type tarDir struct {
	node     tarNode
	children map[string]tarEntry
}

type tarFile struct {
	node    tarNode
	backing io.ReaderAt
	offset  int64
	size    uint64
	key     string
}

type tarSymlink struct {
	node   tarNode
	target string
}

func (d *tarDir) Stat() fs.FileMode       { return d.node.mode & 0o7777 }
func (d *tarDir) ModTime() time.Time      { return d.node.modTime }
func (d *tarDir) Owner() (uint32, uint32) { return d.node.uid, d.node.gid }
func (d *tarDir) RDev() uint32            { return d.node.rdev }

func (d *tarDir) ReadDir() ([]DirEnt, error) {
	out := make([]DirEnt, 0, len(d.children))
	for name, entry := range d.children {
		switch {
		case entry.Dir != nil:
			out = append(out, DirEnt{Name: name, Mode: fs.ModeDir | entry.Dir.Stat()})
		case entry.Symlink != nil:
			out = append(out, DirEnt{Name: name, Mode: fs.ModeSymlink | entry.Symlink.Stat()})
		case entry.File != nil:
			_, mode := entry.File.Stat()
			out = append(out, DirEnt{Name: name, Mode: mode})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *tarDir) Lookup(name string) (Entry, error) {
	entry, ok := d.children[name]
	if !ok {
		return Entry{}, os.ErrNotExist
	}
	switch {
	case entry.Dir != nil:
		return Entry{Dir: entry.Dir}, nil
	case entry.Symlink != nil:
		return Entry{Symlink: entry.Symlink}, nil
	case entry.File != nil:
		return Entry{File: entry.File}, nil
	default:
		return Entry{}, os.ErrNotExist
	}
}

func (f *tarFile) Stat() (uint64, fs.FileMode) { return f.size, f.node.mode }
func (f *tarFile) ModTime() time.Time          { return f.node.modTime }
func (f *tarFile) Owner() (uint32, uint32)     { return f.node.uid, f.node.gid }
func (f *tarFile) RDev() uint32                { return f.node.rdev }
func (f *tarFile) HardlinkKey() string         { return f.key }

func (f *tarFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	if off >= f.size || size == 0 {
		return nil, nil
	}
	end := off + uint64(size)
	if end > f.size {
		end = f.size
	}
	buf := make([]byte, end-off)
	n, err := f.backing.ReadAt(buf, f.offset+int64(off))
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func (f *tarFile) addLink(clean string) {
	if f.key == "" || clean < f.key {
		f.key = clean
	}
}

func (l *tarSymlink) Stat() fs.FileMode       { return l.node.mode & 0o7777 }
func (l *tarSymlink) ModTime() time.Time      { return l.node.modTime }
func (l *tarSymlink) Target() string          { return l.target }
func (l *tarSymlink) Owner() (uint32, uint32) { return l.node.uid, l.node.gid }
func (l *tarSymlink) RDev() uint32            { return l.node.rdev }

func cleanTarPath(name string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.TrimPrefix(name, "."), "/"))
}

func tarRDev(hdr *tar.Header) (uint32, error) {
	if hdr.Typeflag != tar.TypeChar && hdr.Typeflag != tar.TypeBlock {
		return 0, nil
	}
	if hdr.Devmajor < 0 || hdr.Devmajor > math.MaxUint32>>8 {
		return 0, fmt.Errorf("device major is outside representable range")
	}
	if hdr.Devminor < 0 || hdr.Devminor > math.MaxUint32 {
		return 0, fmt.Errorf("device minor is outside uint32 range")
	}
	return uint32(uint64(hdr.Devmajor)<<8 | uint64(hdr.Devminor)), nil
}
