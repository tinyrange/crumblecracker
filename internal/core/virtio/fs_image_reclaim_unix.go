//go:build darwin || freebsd || netbsd || openbsd

package virtio

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Portable image compaction is the reclaim path on these hosts. st_blocks is
// nevertheless the allocated-block value required for truthful telemetry of
// sparse backing files; logical length can be much larger.
func reclaimFileRange(*os.File, int64, int64) error { return errRangeReclaimUnsupported }

func allocatedFileBytes(file *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, err
	}
	if stat.Blocks < 0 {
		return 0, errors.New("backing file reported a negative allocated block count")
	}
	return uint64(stat.Blocks) * 512, nil
}
