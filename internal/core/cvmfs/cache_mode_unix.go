//go:build unix

package cvmfs

import (
	"fmt"
	"io/fs"
	"os"
)

func secureCacheDirectoryMode(path string, info fs.FileInfo) error {
	if info.Mode().Perm() == 0o700 {
		return nil
	}
	return os.Chmod(path, 0o700)
}

func validateCacheFileMode(path string, info fs.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("CVMFS cache entry %q has public mode %04o", path, info.Mode().Perm())
	}
	return nil
}
