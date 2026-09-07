//go:build unix

package cvmfs

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func validateCacheOwner(info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("owner information is unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("owned by uid %d, current uid is %d", stat.Uid, os.Geteuid())
	}
	return nil
}
