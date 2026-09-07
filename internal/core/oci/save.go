package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

var savedImageExcludedPaths = map[string]bool{
	"/dev":   true,
	"/host":  true,
	"/proc":  true,
	"/run":   true,
	"/sys":   true,
	"/tmp":   true,
	"/.ccx3": true,
}

func (s *Store) SaveRootFS(ctx context.Context, name string, root imagefs.Directory, opts SaveOptions) (client.ImageState, error) {
	if strings.TrimSpace(name) == "" {
		return client.ImageState{}, fmt.Errorf("image name is required")
	}
	if err := validateSavedImageName(name); err != nil {
		return client.ImageState{}, err
	}
	if root == nil {
		return client.ImageState{}, fmt.Errorf("root filesystem is required")
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return client.ImageState{}, fmt.Errorf("create image store: %w", err)
	}
	imageDir := s.imageDir(name)
	tmpDir := imageDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return client.ImageState{}, fmt.Errorf("remove temp image dir: %w", err)
	}
	rootDir := filepath.Join(tmpDir, "rootfs")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return client.ImageState{}, fmt.Errorf("create rootfs dir: %w", err)
	}
	entries := map[string]fsmeta.Entry{}
	if err := exportImageDirectory(ctx, root, "/", rootDir, entries); err != nil {
		return client.ImageState{}, err
	}
	fsMetaBuf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return client.ImageState{}, fmt.Errorf("marshal fs metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "rootfs.metadata.json"), fsMetaBuf, 0o644); err != nil {
		return client.ImageState{}, fmt.Errorf("write fs metadata: %w", err)
	}
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		source = "saved:" + name
	}
	meta := metadata{
		Name:         name,
		Source:       source,
		SourceKind:   SourceKindSaved,
		Architecture: normalizeArchitecture(opts.Architecture),
		RootFSDir:    filepath.Join(imageDir, "rootfs"),
		MetadataPath: filepath.Join(imageDir, "rootfs.metadata.json"),
		Env:          append([]string(nil), opts.Config.Env...),
		Entrypoint:   append([]string(nil), opts.Config.Entrypoint...),
		Cmd:          append([]string(nil), opts.Config.Cmd...),
		WorkingDir:   opts.Config.WorkingDir,
		User:         opts.Config.User,
		Labels:       labelPairsFromMap(opts.Config.Labels),
	}
	metaBuf, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return client.ImageState{}, fmt.Errorf("marshal image metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "image.json"), metaBuf, 0o644); err != nil {
		return client.ImageState{}, fmt.Errorf("write image metadata: %w", err)
	}
	if err := os.RemoveAll(imageDir); err != nil && !os.IsNotExist(err) {
		return client.ImageState{}, fmt.Errorf("remove old image dir: %w", err)
	}
	if err := os.Rename(tmpDir, imageDir); err != nil {
		return client.ImageState{}, fmt.Errorf("activate saved image: %w", err)
	}
	s.mu.Lock()
	delete(s.opened, name)
	s.mu.Unlock()
	return client.ImageState{Name: name, Source: source, SourceKind: SourceKindSaved, Status: "downloaded"}, nil
}

func validateSavedImageName(name string) error {
	return validateImageStoreName(name)
}

func exportImageDirectory(ctx context.Context, dir imagefs.Directory, guestPath, hostPath string, entries map[string]fsmeta.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if shouldExcludeSavedPath(guestPath) {
		return nil
	}
	mode := fs.ModeDir | dir.Stat()
	uid, gid := dir.Owner()
	entries[fsmeta.Normalize(guestPath)] = fsmeta.Entry{
		UID:  uid,
		GID:  gid,
		Mode: fsmeta.LinuxModeFromFileMode(mode),
		RDev: dir.RDev(),
	}
	if err := os.MkdirAll(hostPath, mode.Perm()); err != nil {
		return fmt.Errorf("mkdir %s: %w", guestPath, err)
	}
	mtime := dir.ModTime()
	if !mtime.IsZero() {
		_ = os.Chtimes(hostPath, mtime, mtime)
	}
	children, err := dir.ReadDir()
	if err != nil {
		return fmt.Errorf("read %s: %w", guestPath, err)
	}
	for _, child := range children {
		if child.Name == "." || child.Name == ".." {
			continue
		}
		childGuest := path.Join(guestPath, child.Name)
		if shouldExcludeSavedPath(childGuest) {
			continue
		}
		entry, err := dir.Lookup(child.Name)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", childGuest, err)
		}
		childHost := filepath.Join(hostPath, filepath.FromSlash(child.Name))
		if err := exportImageEntry(ctx, entry, childGuest, childHost, entries); err != nil {
			return err
		}
	}
	return nil
}

func exportImageEntry(ctx context.Context, entry imagefs.Entry, guestPath, hostPath string, entries map[string]fsmeta.Entry) error {
	switch {
	case entry.Dir != nil:
		return exportImageDirectory(ctx, entry.Dir, guestPath, hostPath, entries)
	case entry.Symlink != nil:
		mode := fs.ModeSymlink | entry.Symlink.Stat()
		uid, gid := entry.Symlink.Owner()
		target := fsmeta.NormalizeSymlinkTarget(entry.Symlink.Target())
		entries[fsmeta.Normalize(guestPath)] = fsmeta.Entry{
			UID:        uid,
			GID:        gid,
			Mode:       fsmeta.LinuxModeFromFileMode(mode),
			RDev:       entry.Symlink.RDev(),
			LinkTarget: target,
		}
		if err := os.Symlink(target, hostPath); err != nil {
			return fmt.Errorf("symlink %s: %w", guestPath, err)
		}
		return nil
	case entry.File != nil:
		return exportImageFile(ctx, entry.File, guestPath, hostPath, entries)
	default:
		return fmt.Errorf("%s has no filesystem entry", guestPath)
	}
}

func exportImageFile(ctx context.Context, file imagefs.File, guestPath, hostPath string, entries map[string]fsmeta.Entry) error {
	size, mode := file.Stat()
	uid, gid := file.Owner()
	entries[fsmeta.Normalize(guestPath)] = fsmeta.Entry{
		UID:  uid,
		GID:  gid,
		Mode: fsmeta.LinuxModeFromFileMode(mode),
		RDev: file.RDev(),
	}
	linuxMode := fsmeta.LinuxModeFromFileMode(mode)
	if linuxMode&0o170000 != 0o100000 {
		return os.WriteFile(hostPath, nil, 0o644)
	}
	out, err := os.OpenFile(hostPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", guestPath, err)
	}
	const chunkSize = 1 << 20
	for off := uint64(0); off < size; {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			return err
		}
		want := uint32(chunkSize)
		if remain := size - off; remain < uint64(want) {
			want = uint32(remain)
		}
		data, err := file.ReadAt(off, want)
		if err != nil && err != io.EOF {
			_ = out.Close()
			return fmt.Errorf("read %s: %w", guestPath, err)
		}
		if len(data) == 0 {
			break
		}
		if _, err := out.Write(data); err != nil {
			_ = out.Close()
			return fmt.Errorf("write %s: %w", guestPath, err)
		}
		off += uint64(len(data))
	}
	mtime := file.ModTime()
	if mtime.IsZero() {
		mtime = time.Unix(0, 0)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", guestPath, err)
	}
	_ = os.Chtimes(hostPath, mtime, mtime)
	return nil
}

func shouldExcludeSavedPath(guestPath string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(guestPath, "/"))
	if clean == "/" {
		return false
	}
	if savedImageExcludedPaths[clean] {
		return true
	}
	for excluded := range savedImageExcludedPaths {
		if strings.HasPrefix(clean, excluded+"/") {
			return true
		}
	}
	return false
}
