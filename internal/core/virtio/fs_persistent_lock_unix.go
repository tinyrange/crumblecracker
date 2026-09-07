//go:build !windows

package virtio

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockPersistentFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockPersistentFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func replacePersistentFile(source, target string) error {
	return os.Rename(source, target)
}

func syncPersistentDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
