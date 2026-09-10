//go:build windows

package virtio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
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

// Alternate fixed-size JSON records keep the previous durable value available
// when a write is interrupted. The lock serializes readers and writers across
// mounts and processes; the checksum rejects a torn record.
const hostMetadataSlotSize = 512

type hostMetadataRecord struct {
	Generation uint64           `json:"generation"`
	Metadata   hostUnixMetadata `json:"metadata"`
	Checksum   uint32           `json:"checksum"`
}

func (r hostMetadataRecord) checksum() uint32 {
	r.Checksum = 0
	data, _ := json.Marshal(r)
	return crc32.ChecksumIEEE(data)
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
	data, err := io.ReadAll(io.LimitReader(file, 2*hostMetadataSlotSize+1))
	if err != nil {
		return hostUnixMetadata{}, err
	}
	if len(data) > 2*hostMetadataSlotSize {
		return hostUnixMetadata{}, fmt.Errorf("shared file Unix metadata too large")
	}
	current := hostMetadataRecord{Metadata: hostUnixMetadata{Version: 1}}
	for slot := 0; slot < 2; slot++ {
		begin := slot * hostMetadataSlotSize
		if begin >= len(data) {
			continue
		}
		raw := bytes.TrimRight(data[begin:min(begin+hostMetadataSlotSize, len(data))], "\x00")
		var record hostMetadataRecord
		if json.Unmarshal(raw, &record) == nil && record.Metadata.Version == 1 && record.Generation > current.Generation && record.Checksum == record.checksum() {
			current = record
		}
	}
	if len(data) != 0 && current.Generation == 0 {
		return current.Metadata, fmt.Errorf("shared file Unix metadata has no valid record")
	}
	meta := current.Metadata
	if update == nil {
		return meta, nil
	}
	update(&meta)
	next := hostMetadataRecord{Generation: current.Generation + 1, Metadata: meta}
	next.Checksum = next.checksum()
	encoded, err := json.Marshal(next)
	if err != nil {
		return meta, err
	}
	if len(encoded) > hostMetadataSlotSize {
		return meta, fmt.Errorf("shared file Unix metadata exceeds record capacity")
	}
	var record [hostMetadataSlotSize]byte
	copy(record[:], encoded)
	if _, err := file.WriteAt(record[:], int64(next.Generation%2)*hostMetadataSlotSize); err != nil {
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
