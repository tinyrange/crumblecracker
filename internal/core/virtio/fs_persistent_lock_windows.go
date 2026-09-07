//go:build windows

package virtio

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockPersistentFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
}

func unlockPersistentFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

func replacePersistentFile(source, target string) error {
	_ = os.Remove(target)
	return os.Rename(source, target)
}

func syncPersistentDirectory(string) error {
	return nil
}
