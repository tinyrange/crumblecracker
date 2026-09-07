//go:build !windows

package guestagent

import (
	"os"

	"golang.org/x/sys/unix"
)

func openTarRegularFile(path string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_TRUNC|unix.O_WRONLY|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
