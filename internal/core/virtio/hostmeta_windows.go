//go:build windows

package virtio

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tinyrange/crumblecracker/internal/core/hostfile"
	"golang.org/x/sys/windows"
)

// NTFS streams belong to the file, so metadata follows host/guest renames and
// hard links without a path-index database or visible files in shared folders.
const hostMetadataStream = ":crumblecracker.unix.v1"

type hostUnixMetadata struct {
	Version int     `json:"version"`
	Mode    *uint32 `json:"mode,omitempty"`
	UID     *uint32 `json:"uid,omitempty"`
	GID     *uint32 `json:"gid,omitempty"`
}

func withHostMetadata(path string, update func(*hostUnixMetadata)) (hostUnixMetadata, error) {
	flags := os.O_RDONLY
	if update != nil {
		flags = os.O_CREATE | os.O_RDWR
	}
	file, err := hostfile.OpenFile(path+hostMetadataStream, flags, 0600)
	if err != nil {
		if update == nil && errors.Is(err, os.ErrNotExist) {
			return hostUnixMetadata{}, nil
		}
		return hostUnixMetadata{}, err
	}
	defer file.Close()
	lockFlags := uint32(0)
	if update != nil {
		lockFlags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	var offset windows.Overlapped
	handle := windows.Handle(file.Fd())
	if err := windows.LockFileEx(handle, lockFlags, 0, 1, 0, &offset); err != nil {
		return hostUnixMetadata{}, err
	}
	defer windows.UnlockFileEx(handle, 0, 1, 0, &offset)
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return hostUnixMetadata{}, err
	}
	meta := hostUnixMetadata{Version: 1}
	if len(data) > 4096 {
		return meta, fmt.Errorf("shared file Unix metadata too large")
	}
	if len(data) != 0 {
		if err := json.Unmarshal(data, &meta); err != nil {
			return meta, fmt.Errorf("decode shared file Unix metadata: %w", err)
		}
		if meta.Version != 1 {
			return meta, fmt.Errorf("unsupported shared file Unix metadata version")
		}
	}
	if update == nil {
		return meta, nil
	}
	update(&meta)
	data, err = json.Marshal(meta)
	if err != nil {
		return meta, err
	}
	if _, err = file.WriteAt(data, 0); err != nil {
		return meta, err
	}
	if err = file.Truncate(int64(len(data))); err != nil {
		return meta, err
	}
	return meta, file.Sync()
}
func setHostMode(path string, mode uint32) error {
	mode &= linuxPermMask
	_, err := withHostMetadata(path, func(m *hostUnixMetadata) { m.Mode = &mode })
	return err
}
func setHostOwner(path string, valid, uid, gid uint32) error {
	_, err := withHostMetadata(path, func(m *hostUnixMetadata) {
		if valid&fattrUID != 0 {
			m.UID = &uid
		}
		if valid&fattrGID != 0 {
			m.GID = &gid
		}
	})
	return err
}
func initHostMetadata(path string, mode, uid, gid uint32) error {
	mode &= linuxPermMask
	_, err := withHostMetadata(path, func(m *hostUnixMetadata) {
		if m.Mode == nil {
			m.Mode = &mode
		}
		if m.UID == nil {
			m.UID = &uid
		}
		if m.GID == nil {
			m.GID = &gid
		}
	})
	return err
}
func applyHostMetadata(path string, attr *FuseAttr) error {
	meta, err := withHostMetadata(path, nil)
	if err != nil {
		return err
	}
	if meta.Mode != nil {
		attr.Mode = attr.Mode&^linuxPermMask | *meta.Mode&linuxPermMask
	}
	if meta.UID != nil {
		attr.UID = *meta.UID
	}
	if meta.GID != nil {
		attr.GID = *meta.GID
	}
	return nil
}
