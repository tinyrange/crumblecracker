//go:build !windows

// Package hostfile opens guest-visible host files without denying namespace
// operations to other handles. Persistent-store writer locks are separate.
package hostfile

import "os"

func OpenFile(name string, flags int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flags, mode)
}
