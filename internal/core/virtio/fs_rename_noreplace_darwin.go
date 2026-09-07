//go:build darwin

package virtio

import "golang.org/x/sys/unix"

func renameNoReplace(oldPath, newPath string) error {
	return unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
}
