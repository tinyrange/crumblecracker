//go:build windows

package cvmfs

import "io/fs"

// Go's portable Windows FileMode reports synthesized POSIX bits rather than
// the file's ACL. Ownership and ACL validation need a separate Windows policy;
// do not reject private cache entries based on those synthesized bits.
func secureCacheDirectoryMode(string, fs.FileInfo) error { return nil }
func validateCacheFileMode(string, fs.FileInfo) error    { return nil }
