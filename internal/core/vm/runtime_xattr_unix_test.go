//go:build linux || freebsd || netbsd || darwin

package vm

import "golang.org/x/sys/unix"

func setRuntimeTestXattr(path, name string, value []byte) error {
	return unix.Setxattr(path, name, value, 0)
}
