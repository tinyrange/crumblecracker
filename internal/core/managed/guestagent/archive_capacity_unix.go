//go:build linux || darwin

package guestagent

import "golang.org/x/sys/unix"

func archiveFilesystemCapacity(path string) (uint64, uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), stat.Ffree, nil
}
