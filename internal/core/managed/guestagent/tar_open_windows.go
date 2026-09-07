//go:build windows

package guestagent

import "os"

func openTarRegularFile(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
}
