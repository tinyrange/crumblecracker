//go:build linux

package virtio

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func reclaimFileRange(file *os.File, offset, length int64) error {
	err := unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, offset, length)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return errRangeReclaimUnsupported
	}
	return err
}

func allocatedFileBytes(file *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Blocks) * 512, nil
}
