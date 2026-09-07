package imagefs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
	"github.com/tinyrange/crumblecracker/internal/core/linuxabi"
)

type File interface {
	Stat() (size uint64, mode fs.FileMode)
	ModTime() time.Time
	ReadAt(off uint64, size uint32) ([]byte, error)
	Owner() (uid, gid uint32)
	RDev() uint32
}

type OpenReaderFile interface {
	OpenReader() (io.ReaderAt, io.Closer, error)
}

type HardlinkFile interface {
	HardlinkKey() string
}

type Directory interface {
	Stat() fs.FileMode
	ModTime() time.Time
	ReadDir() ([]DirEnt, error)
	Lookup(name string) (Entry, error)
	Owner() (uid, gid uint32)
	RDev() uint32
}

// ContextDirectory lets snapshot materialization cancel lower-filesystem work
// during VM teardown. Directory remains the compatibility interface; callers
// keep legacy implementations owned until their operation returns.
type ContextDirectory interface {
	Directory
	ReadDirContext(context.Context) ([]DirEnt, error)
	LookupContext(context.Context, string) (Entry, error)
}

func ReadDirContext(ctx context.Context, directory Directory) ([]DirEnt, error) {
	if contextual, ok := directory.(ContextDirectory); ok {
		return contextual.ReadDirContext(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return directory.ReadDir()
}

func LookupContext(ctx context.Context, directory Directory, name string) (Entry, error) {
	if contextual, ok := directory.(ContextDirectory); ok {
		return contextual.LookupContext(ctx, name)
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	return directory.Lookup(name)
}

type Symlink interface {
	Stat() fs.FileMode
	ModTime() time.Time
	Target() string
	Owner() (uid, gid uint32)
	RDev() uint32
}

type Entry struct {
	File    File
	Dir     Directory
	Symlink Symlink
}

type DirEnt struct {
	Name string
	Mode fs.FileMode
}

// Namespace is an immutable, process-wide index of every node in an image.
// Node IDs are stable for the lifetime of the namespace and may be shared by
// any number of VM-specific copy-on-write filesystems.
type Namespace struct {
	Nodes []*NamespaceNode

	cacheMu sync.Mutex
	cache   map[any]any
}

type NamespaceNode struct {
	ID       uint64
	Parent   uint64
	Name     string
	Entry    Entry
	Children map[string]uint64
}

type NamespaceDirectory interface {
	Directory
	Namespace() *Namespace
}

func DirectoryNamespace(root Directory) *Namespace {
	if indexed, ok := root.(NamespaceDirectory); ok {
		return indexed.Namespace()
	}
	return nil
}

// Cached lets backend implementations attach derived immutable indexes to the
// namespace without introducing a dependency from imagefs back to a backend.
func (n *Namespace) Cached(key any, build func() any) any {
	if n == nil {
		return build()
	}
	n.cacheMu.Lock()
	defer n.cacheMu.Unlock()
	if value := n.cache[key]; value != nil {
		return value
	}
	if n.cache == nil {
		n.cache = make(map[any]any)
	}
	value := build()
	n.cache[key] = value
	return value
}

func BuildNamespace(root Directory) (*Namespace, error) {
	if root == nil {
		return nil, fmt.Errorf("root filesystem is nil")
	}
	namespace := &Namespace{Nodes: []*NamespaceNode{nil}}
	rootNode := &NamespaceNode{
		ID:       1,
		Parent:   1,
		Name:     "/",
		Entry:    Entry{Dir: root},
		Children: make(map[string]uint64),
	}
	namespace.Nodes = append(namespace.Nodes, rootNode)
	var addDirectory func(*NamespaceNode, Directory) error
	addDirectory = func(parent *NamespaceNode, directory Directory) error {
		entries, err := directory.ReadDir()
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		for _, dirent := range entries {
			if dirent.Name == "." || dirent.Name == ".." {
				continue
			}
			entry, err := directory.Lookup(dirent.Name)
			if err != nil {
				return fmt.Errorf("lookup %q: %w", dirent.Name, err)
			}
			id := uint64(len(namespace.Nodes))
			node := &NamespaceNode{
				ID:     id,
				Parent: parent.ID,
				Name:   dirent.Name,
				Entry:  entry,
			}
			if entry.Dir != nil {
				node.Children = make(map[string]uint64)
			}
			namespace.Nodes = append(namespace.Nodes, node)
			parent.Children[dirent.Name] = id
			if entry.Dir != nil {
				if err := addDirectory(node, entry.Dir); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := addDirectory(rootNode, root); err != nil {
		return nil, err
	}
	return namespace, nil
}

func NewHostFS(root string, meta map[string]fsmeta.Entry) Directory {
	rootMode := fs.ModeDir | 0o755
	rootUID := uint32(0)
	rootGID := uint32(0)
	rootRDev := uint32(0)
	if entry, ok := meta["/"]; ok {
		rootMode = linuxModeToGo(fsmeta.NormalizeLinuxMode(entry.Mode, fs.ModeDir|0o755))
		rootUID = entry.UID
		rootGID = entry.GID
		rootRDev = entry.RDev
	}
	rootModTime := time.Unix(0, 0)
	if info, err := os.Lstat(root); err == nil {
		rootModTime = info.ModTime()
	}
	return &hostDir{
		rootPath: root,
		hostPath: root,
		mode:     rootMode,
		uid:      rootUID,
		gid:      rootGID,
		rdev:     rootRDev,
		modTime:  rootModTime,
		meta:     meta,
	}
}

func LookupPath(root Directory, guestPath string) (Entry, error) {
	if root == nil {
		return Entry{}, fmt.Errorf("root filesystem is nil")
	}
	clean := path.Clean("/" + strings.TrimPrefix(guestPath, "/"))
	if clean == "/" {
		return Entry{Dir: root}, nil
	}
	current := root
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	for i, part := range parts {
		entry, err := current.Lookup(part)
		if err != nil {
			return Entry{}, err
		}
		if i == len(parts)-1 {
			return entry, nil
		}
		if entry.Dir == nil {
			return Entry{}, fmt.Errorf("%q is not a directory", "/"+strings.Join(parts[:i+1], "/"))
		}
		current = entry.Dir
	}
	return Entry{}, fmt.Errorf("empty path")
}

func ResolveCommand(root Directory, command []string, env []string) ([]string, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("command is empty")
	}
	if strings.Contains(command[0], "/") {
		_, entry, err := ResolvePath(root, command[0])
		if err != nil {
			return nil, fmt.Errorf("resolve command %q: %w", command[0], err)
		}
		if entry.Dir != nil {
			return nil, fmt.Errorf("command %q is a directory", command[0])
		}
		if entry.File != nil {
			_, mode := entry.File.Stat()
			if mode&0o111 == 0 {
				return nil, fmt.Errorf("command %q is not executable", command[0])
			}
		}
		out := append([]string(nil), command...)
		out[0] = command[0]
		return out, nil
	}
	pathEnv := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			pathEnv = strings.TrimPrefix(kv, "PATH=")
			break
		}
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			continue
		}
		guestPath := path.Join(dir, command[0])
		_, entry, err := ResolvePath(root, guestPath)
		if err != nil || entry.Dir != nil {
			continue
		}
		if entry.File != nil {
			_, mode := entry.File.Stat()
			if mode&0o111 != 0 {
				return append([]string{guestPath}, command[1:]...), nil
			}
		}
	}
	return nil, fmt.Errorf("resolve command %q in PATH", command[0])
}

func ResolvePath(root Directory, guestPath string) (string, Entry, error) {
	clean := path.Clean("/" + strings.TrimPrefix(guestPath, "/"))
	for depth := 0; depth < 40; depth++ {
		resolved, entry, err := resolvePathOnce(root, clean)
		if err != nil {
			return "", Entry{}, err
		}
		if entry.Symlink == nil {
			return resolved, entry, nil
		}
		clean = resolved
	}
	return "", Entry{}, fmt.Errorf("%q has too many symlink levels", guestPath)
}

func resolvePathOnce(root Directory, guestPath string) (string, Entry, error) {
	if root == nil {
		return "", Entry{}, fmt.Errorf("root filesystem is nil")
	}
	clean := path.Clean("/" + strings.TrimPrefix(guestPath, "/"))
	if clean == "/" {
		return clean, Entry{Dir: root}, nil
	}
	current := root
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	for i, part := range parts {
		entry, err := current.Lookup(part)
		if err != nil {
			return "", Entry{}, err
		}
		currentPath := "/" + strings.Join(parts[:i+1], "/")
		if entry.Symlink != nil {
			target := fsmeta.NormalizeSymlinkTarget(entry.Symlink.Target())
			if target == "" {
				return "", Entry{}, fmt.Errorf("%q symlink target is empty", currentPath)
			}
			remainder := path.Join(parts[i+1:]...)
			var next string
			if strings.HasPrefix(target, "/") {
				next = target
			} else {
				next = path.Join(path.Dir(currentPath), target)
			}
			if remainder != "" && remainder != "." {
				next = path.Join(next, remainder)
			}
			return path.Clean(next), entry, nil
		}
		if i == len(parts)-1 {
			return currentPath, entry, nil
		}
		if entry.Dir == nil {
			return "", Entry{}, fmt.Errorf("%q is not a directory", currentPath)
		}
		current = entry.Dir
	}
	return "", Entry{}, fmt.Errorf("empty path")
}

type hostFile struct {
	hostPath string
	mode     fs.FileMode
	uid      uint32
	gid      uint32
	rdev     uint32
	size     uint64
	modTime  time.Time
}

type hostDir struct {
	rootPath string
	hostPath string
	mode     fs.FileMode
	uid      uint32
	gid      uint32
	rdev     uint32
	modTime  time.Time
	meta     map[string]fsmeta.Entry
}

type hostSymlink struct {
	hostPath string
	mode     fs.FileMode
	uid      uint32
	gid      uint32
	rdev     uint32
	target   string
	modTime  time.Time
}

func (f *hostFile) Stat() (uint64, fs.FileMode) { return f.size, f.mode }
func (f *hostFile) ModTime() time.Time          { return f.modTime }
func (f *hostFile) Owner() (uint32, uint32)     { return f.uid, f.gid }
func (f *hostFile) RDev() uint32                { return f.rdev }
func (f *hostFile) OpenReader() (io.ReaderAt, io.Closer, error) {
	file, err := os.Open(f.hostPath)
	if err != nil {
		return nil, nil, err
	}
	return file, file, nil
}
func (f *hostFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	file, err := os.Open(f.hostPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, size)
	n, err := file.ReadAt(buf, int64(off))
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func (d *hostDir) Stat() fs.FileMode       { return d.mode & linuxPermMask }
func (d *hostDir) ModTime() time.Time      { return d.modTime }
func (d *hostDir) Owner() (uint32, uint32) { return d.uid, d.gid }
func (d *hostDir) RDev() uint32            { return d.rdev }
func (d *hostDir) ReadDir() ([]DirEnt, error) {
	entries, err := os.ReadDir(d.hostPath)
	if err != nil {
		return nil, err
	}
	out := make([]DirEnt, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, DirEnt{Name: entry.Name(), Mode: info.Mode()})
	}
	return out, nil
}

func (d *hostDir) ReadDirContext(ctx context.Context) ([]DirEnt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.ReadDir()
}

func (d *hostDir) Lookup(name string) (Entry, error) {
	host := filepath.Join(d.hostPath, filepath.FromSlash(name))
	info, err := os.Lstat(host)
	if err != nil {
		return Entry{}, err
	}
	rel, err := filepath.Rel(d.rootPath, host)
	if err != nil {
		return Entry{}, err
	}
	guest := fsmeta.Normalize(filepath.ToSlash(rel))
	meta := d.meta[guest]
	mode := linuxModeToGo(fsmeta.NormalizeLinuxMode(meta.Mode, info.Mode()))
	modTime := info.ModTime()
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(host)
		if err != nil {
			return Entry{}, err
		}
		if meta.LinkTarget != "" {
			target = meta.LinkTarget
		}
		target = fsmeta.NormalizeSymlinkTarget(target)
		return Entry{Symlink: &hostSymlink{hostPath: host, mode: mode, uid: meta.UID, gid: meta.GID, rdev: meta.RDev, target: target, modTime: modTime}}, nil
	case info.IsDir():
		return Entry{Dir: &hostDir{rootPath: d.rootPath, hostPath: host, mode: mode, uid: meta.UID, gid: meta.GID, rdev: meta.RDev, modTime: modTime, meta: d.meta}}, nil
	default:
		return Entry{File: &hostFile{hostPath: host, mode: mode, uid: meta.UID, gid: meta.GID, rdev: meta.RDev, size: uint64(info.Size()), modTime: modTime}}, nil
	}
}

func (d *hostDir) LookupContext(ctx context.Context, name string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	return d.Lookup(name)
}

func (l *hostSymlink) Stat() fs.FileMode       { return l.mode & linuxPermMask }
func (l *hostSymlink) ModTime() time.Time      { return l.modTime }
func (l *hostSymlink) Target() string          { return l.target }
func (l *hostSymlink) Owner() (uint32, uint32) { return l.uid, l.gid }
func (l *hostSymlink) RDev() uint32            { return l.rdev }

const (
	linuxSIFMT    = linuxabi.SIFMT
	linuxSIFSOCK  = linuxabi.SIFSOCK
	linuxSIFLNK   = linuxabi.SIFLNK
	linuxSIFREG   = linuxabi.SIFREG
	linuxSIFBLK   = linuxabi.SIFBLK
	linuxSIFDIR   = linuxabi.SIFDIR
	linuxSIFCHR   = linuxabi.SIFCHR
	linuxSIFIFO   = linuxabi.SIFIFO
	linuxPermMask = linuxabi.PermMask
)

func linuxModeToGo(mode uint32) fs.FileMode {
	perm := fs.FileMode(mode & linuxPermMask)
	switch mode & linuxSIFMT {
	case linuxSIFDIR:
		perm |= fs.ModeDir
	case linuxSIFLNK:
		perm |= fs.ModeSymlink
	case linuxSIFIFO:
		perm |= fs.ModeNamedPipe
	case linuxSIFCHR:
		perm |= fs.ModeDevice | fs.ModeCharDevice
	case linuxSIFBLK:
		perm |= fs.ModeDevice
	case linuxSIFSOCK:
		perm |= fs.ModeSocket
	}
	return perm
}
