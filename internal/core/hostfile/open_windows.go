//go:build windows

package hostfile

import (
	"golang.org/x/sys/windows"
	"os"
)

// OpenFile permits concurrent reads, writes, and rename/delete, like a Unix
// descriptor. Guest Unix mode bits are stored separately from Windows ACLs.
func OpenFile(name string, flags int, _ os.FileMode) (*os.File, error) {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	access := uint32(windows.GENERIC_READ)
	switch flags & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if flags&os.O_APPEND != 0 {
		access &^= windows.GENERIC_WRITE
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE
	}
	disposition := uint32(windows.OPEN_EXISTING)
	switch {
	case flags&os.O_CREATE != 0 && flags&os.O_EXCL != 0:
		disposition = windows.CREATE_NEW
	case flags&os.O_CREATE != 0 && flags&os.O_TRUNC != 0:
		disposition = windows.CREATE_ALWAYS
	case flags&os.O_CREATE != 0:
		disposition = windows.OPEN_ALWAYS
	case flags&os.O_TRUNC != 0:
		disposition = windows.TRUNCATE_EXISTING
	}
	attributes := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_BACKUP_SEMANTICS)
	if flags&os.O_SYNC != 0 {
		attributes |= windows.FILE_FLAG_WRITE_THROUGH
	}
	handle, err := windows.CreateFile(path, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, disposition, attributes, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(handle), name), nil
}
