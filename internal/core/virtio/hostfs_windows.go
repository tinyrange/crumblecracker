//go:build windows

package virtio

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/tinyrange/crumblecracker/internal/core/hostfile"
	"github.com/tinyrange/crumblecracker/internal/core/linuxabi"
	"golang.org/x/sys/windows"
)

func hostStatFS(root string) (uint64, uint64, uint64, uint64, uint64, uint64, uint64, uint64, int32) {
	dir := root
	if abs, err := filepath.Abs(root); err == nil {
		dir = abs
	}
	ptr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, errnoFromError(err)
	}
	var freeAvail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeAvail, &total, &free); err != nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, errnoFromError(err)
	}
	const blockSize = 4096
	return total / blockSize, free / blockSize, freeAvail / blockSize, 0, 0, blockSize, blockSize, 255, 0
}

func enrichHostFileAttr(hostPath string, info os.FileInfo, attr *FuseAttr) {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	file, err := hostfile.OpenFile(hostPath, os.O_RDONLY, 0)
	if err != nil {
		return
	}
	defer file.Close()
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &handleInfo); err != nil {
		return
	}
	attr.Ino = uint64(handleInfo.FileIndexHigh)<<32 | uint64(handleInfo.FileIndexLow)
	if handleInfo.NumberOfLinks != 0 {
		attr.NLink = handleInfo.NumberOfLinks
	}
}

func mapHostError(err error) (int32, bool) {
	errno, ok := err.(syscall.Errno)
	if !ok {
		return 0, false
	}
	switch errno {
	case windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND:
		return linuxabi.ENOENT, true
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_PRIVILEGE_NOT_HELD:
		return linuxabi.EPERM, true
	case windows.ERROR_ALREADY_EXISTS, windows.ERROR_FILE_EXISTS:
		return linuxabi.EEXIST, true
	case windows.ERROR_TIMEOUT:
		return linuxabi.ETIMEDOUT, true
	case windows.ERROR_DIRECTORY:
		return linuxabi.ENOTDIR, true
	case windows.ERROR_INVALID_PARAMETER, windows.ERROR_INVALID_NAME:
		return linuxabi.EINVAL, true
	case windows.ERROR_INVALID_HANDLE:
		return linuxabi.EBADF, true
	case windows.ERROR_BROKEN_PIPE:
		return linuxabi.EPIPE, true
	case windows.ERROR_NOT_SUPPORTED:
		return linuxabi.ENOSYS, true
	case syscall.ENOSPC, windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL:
		return linuxabi.ENOSPC, true
	default:
		return linuxabi.EIO, true
	}
}
