//go:build linux || freebsd || darwin

package guestagent

import "golang.org/x/sys/unix"

func archiveSeekData(fd int, offset int64) (int64, error) {
	return unix.Seek(fd, offset, unix.SEEK_DATA)
}

func archiveSeekHole(fd int, offset int64) (int64, error) {
	return unix.Seek(fd, offset, unix.SEEK_HOLE)
}
