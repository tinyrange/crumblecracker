package cvmfs

import (
	"fmt"
	"io/fs"
	"path"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

// NewImageFS exposes a CVMFS target as a lazy imagefs tree. Directory and file
// data are resolved only when the guest looks them up or reads them.
func NewImageFS(client *Client, target string) (imagefs.Directory, error) {
	if client == nil {
		return nil, fmt.Errorf("CVMFS client is required")
	}
	parsed, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	// Keep mount creation offline-safe. The first lookup validates the remote
	// repository and reports any network or catalog error to the guest.
	entry := Entry{Path: parsed.Path, Mode: fs.ModeDir | 0o555}
	return &imageDirectory{client: client, target: parsed, entry: entry}, nil
}

type imageDirectory struct {
	client *Client
	target Target
	entry  Entry
}

func (d *imageDirectory) Stat() fs.FileMode       { return d.entry.Mode }
func (d *imageDirectory) ModTime() time.Time      { return d.entry.ModTime }
func (d *imageDirectory) Owner() (uint32, uint32) { return d.entry.UID, d.entry.GID }
func (d *imageDirectory) RDev() uint32            { return d.entry.RDev }

func (d *imageDirectory) ReadDir() ([]imagefs.DirEnt, error) {
	target, err := FormatTarget(d.target)
	if err != nil {
		return nil, err
	}
	entries, err := d.client.ReadDir(target)
	if err != nil {
		return nil, err
	}
	out := make([]imagefs.DirEnt, 0, len(entries))
	for _, entry := range entries {
		out = append(out, imagefs.DirEnt{Name: entry.Name, Mode: entry.Mode})
	}
	return out, nil
}

func (d *imageDirectory) Lookup(name string) (imagefs.Entry, error) {
	if name == "" || name == "." || name == ".." || path.Base(name) != name {
		return imagefs.Entry{}, fs.ErrNotExist
	}
	child := d.target
	child.Path = path.Join(d.target.Path, name)
	target, err := FormatTarget(child)
	if err != nil {
		return imagefs.Entry{}, err
	}
	entry, err := d.client.Stat(target)
	if err != nil {
		return imagefs.Entry{}, err
	}
	switch {
	case entry.Mode.IsDir():
		return imagefs.Entry{Dir: &imageDirectory{client: d.client, target: child, entry: entry}}, nil
	case entry.Mode&fs.ModeSymlink != 0:
		return imagefs.Entry{Symlink: imageSymlink{entry: entry}}, nil
	default:
		return imagefs.Entry{File: &imageFile{client: d.client, target: target, entry: entry}}, nil
	}
}

type imageFile struct {
	client *Client
	target string
	entry  Entry
}

func (f *imageFile) Stat() (uint64, fs.FileMode) { return uint64(max(0, f.entry.Size)), f.entry.Mode }
func (f *imageFile) ModTime() time.Time          { return f.entry.ModTime }
func (f *imageFile) Owner() (uint32, uint32)     { return f.entry.UID, f.entry.GID }
func (f *imageFile) RDev() uint32                { return f.entry.RDev }
func (f *imageFile) ReadAt(off uint64, size uint32) ([]byte, error) {
	data, _, err := f.client.ReadFileRange(f.target, int64(off), int64(size))
	return data, err
}

type imageSymlink struct{ entry Entry }

func (s imageSymlink) Stat() fs.FileMode       { return s.entry.Mode }
func (s imageSymlink) ModTime() time.Time      { return s.entry.ModTime }
func (s imageSymlink) Target() string          { return s.entry.Symlink }
func (s imageSymlink) Owner() (uint32, uint32) { return s.entry.UID, s.entry.GID }
func (s imageSymlink) RDev() uint32            { return s.entry.RDev }
