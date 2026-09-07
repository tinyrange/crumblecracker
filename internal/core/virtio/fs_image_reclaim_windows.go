//go:build windows

package virtio

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsFileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  byte
	Directory      byte
	_              [2]byte
}

func reclaimFileRange(*os.File, int64, int64) error { return errRangeReclaimUnsupported }

func allocatedFileBytes(file *os.File) (uint64, error) {
	var info windowsFileStandardInfo
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()), windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		return 0, err
	}
	if info.AllocationSize < 0 {
		return 0, fmt.Errorf("backing file reported a negative allocation size")
	}
	return uint64(info.AllocationSize), nil
}
