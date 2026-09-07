package mounts

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func BuildPersistentImageMounts(storeRoot string, image *oci.Image, requested []client.PersistentMount) ([]virtio.ShareMount, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	if image == nil || image.RootFS == nil {
		return nil, fmt.Errorf("persistent mounts require an image root filesystem")
	}
	if strings.TrimSpace(storeRoot) == "" {
		return nil, fmt.Errorf("persistent mount store root is required")
	}
	prepared := make([]virtio.ShareMount, 0, len(requested))
	cleanup := true
	defer func() {
		if cleanup {
			_ = closePersistentMounts(prepared)
		}
	}()
	seen := make(map[string]struct{}, len(requested))
	for i, spec := range requested {
		name, err := canonicalPersistentMountName(spec.Name)
		if err != nil {
			return nil, fmt.Errorf("persistent mount %d: %w", i, err)
		}
		mount := strings.TrimSpace(spec.Mount)
		if mount == "" {
			mount, err = imageHomeDirectory(image)
			if err != nil {
				return nil, fmt.Errorf("persistent mount %d: %w", i, err)
			}
		}
		mount = path.Clean("/" + strings.TrimPrefix(mount, "/"))
		if mount == "/" {
			return nil, fmt.Errorf("persistent mount %d: mount must name an image directory below /", i)
		}
		if _, ok := seen[mount]; ok {
			return nil, fmt.Errorf("persistent mount path %q is duplicated", mount)
		}
		seen[mount] = struct{}{}
		entry, err := imagefs.LookupPath(image.RootFS, mount)
		if err != nil {
			return nil, fmt.Errorf("persistent mount %d: resolve image directory %q: %w", i, mount, err)
		}
		if entry.Dir == nil {
			return nil, fmt.Errorf("persistent mount %d: image path %q is not a directory", i, mount)
		}
		storeDir := filepath.Join(storeRoot, name)
		// OCI indexed subdirectories expose the image-wide namespace. Hide that
		// optimization here: the persistent mount root must be the selected
		// home directory, not inode 1 of the complete image namespace.
		lower := persistentLowerDirectory{Directory: entry.Dir}
		backend, err := virtio.NewPersistentImageFS(lower, virtio.PersistentImageFSOptions{
			StoreDir:   storeDir,
			Name:       name,
			Mount:      mount,
			LowerID:    persistentImageLowerID(image),
			StatFSPath: storeDir,
			OwnerUID:   spec.OwnerUID,
			OwnerGID:   spec.OwnerGID,
			MapOwner:   spec.MapOwner,
		})
		if err != nil {
			return nil, fmt.Errorf("persistent mount %d: %w", i, err)
		}
		prepared = append(prepared, virtio.ShareMount{
			GuestPath: mount,
			Backend:   backend,
			Writable:  true,
			// The lower image is immutable and the upper is exclusively owned by
			// this guest while its store lock is held. Guest-side mutations keep
			// the kernel's own dentries coherent, so retaining lookup and attr
			// results avoids turning package extraction into millions of RPCs.
			CacheMode: "aggressive",
		})
	}
	cleanup = false
	return prepared, nil
}

type persistentLowerDirectory struct {
	imagefs.Directory
}

func (d persistentLowerDirectory) ReadDirContext(ctx context.Context) ([]imagefs.DirEnt, error) {
	return imagefs.ReadDirContext(ctx, d.Directory)
}

func (d persistentLowerDirectory) LookupContext(ctx context.Context, name string) (imagefs.Entry, error) {
	return imagefs.LookupContext(ctx, d.Directory, name)
}

func imageHomeDirectory(image *oci.Image) (string, error) {
	// Desktop images commonly boot their session as an ordinary user while
	// retaining root as the OCI process user. In that case HOME is the image's
	// declaration of the desktop home and /etc/passwd's root entry is not.
	for i := len(image.Config.Env) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(image.Config.Env[i], "=")
		if !ok || key != "HOME" {
			continue
		}
		home := path.Clean(value)
		if !strings.HasPrefix(home, "/") || home == "/" {
			break
		}
		entry, err := imagefs.LookupPath(image.RootFS, home)
		if err == nil && entry.Dir != nil {
			return home, nil
		}
		break
	}

	user := strings.TrimSpace(image.Config.User)
	if user == "" {
		user = "0"
	}
	if value, _, ok := strings.Cut(user, ":"); ok {
		user = value
	}
	entry, err := imagefs.LookupPath(image.RootFS, "/etc/passwd")
	if err != nil || entry.File == nil {
		if user == "0" || user == "root" {
			return "/root", nil
		}
		return "", fmt.Errorf("resolve home for image user %q: /etc/passwd is unavailable", user)
	}
	size, _ := entry.File.Stat()
	if size > 8<<20 {
		return "", fmt.Errorf("resolve home for image user %q: /etc/passwd is unexpectedly large", user)
	}
	content, err := entry.File.ReadAt(0, uint32(size))
	if err != nil {
		return "", fmt.Errorf("read /etc/passwd: %w", err)
	}
	numeric := false
	if _, err := strconv.ParseUint(user, 10, 32); err == nil {
		numeric = true
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		if numeric && fields[2] != user || !numeric && fields[0] != user {
			continue
		}
		home := path.Clean(fields[5])
		if !strings.HasPrefix(home, "/") || home == "/" {
			return "", fmt.Errorf("image user %q has invalid home %q", user, fields[5])
		}
		return home, nil
	}
	return "", fmt.Errorf("image user %q is absent from /etc/passwd", user)
}

func canonicalPersistentMountName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return "", fmt.Errorf("name is required")
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("name %q contains unsupported character %q", value, r)
	}
	return value, nil
}

func persistentImageLowerID(image *oci.Image) string {
	for _, value := range []string{image.Source, image.Name, image.Architecture} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "image"
}

func closePersistentMounts(mounts []virtio.ShareMount) error {
	var first error
	for _, mount := range mounts {
		if closer, ok := mount.Backend.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
