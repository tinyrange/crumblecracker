//go:build linux || freebsd || netbsd || darwin

package guestagent

import "golang.org/x/sys/unix"

func archiveListXattrs(path string, dest []byte) (int, error) {
	return unix.Llistxattr(path, dest)
}

func archiveGetXattr(path, name string, dest []byte) (int, error) {
	return unix.Lgetxattr(path, name, dest)
}

func archiveSetXattr(path, name string, value []byte, symlink bool) error {
	if symlink {
		return unix.Lsetxattr(path, name, value, 0)
	}
	return unix.Setxattr(path, name, value, 0)
}
