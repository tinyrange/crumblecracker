//go:build windows

package virtio

import "golang.org/x/sys/windows"

func renameNoReplace(oldPath, newPath string) error {
	oldPathUTF16, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPathUTF16, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	return windows.MoveFile(oldPathUTF16, newPathUTF16)
}
