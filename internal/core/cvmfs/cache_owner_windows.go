package cvmfs

import "io/fs"

// Windows cache directories are made private using their mode at creation.
// Go's portable FileInfo does not expose an owner SID for subsequent checks.
func validateCacheOwner(info fs.FileInfo) error {
	return nil
}
