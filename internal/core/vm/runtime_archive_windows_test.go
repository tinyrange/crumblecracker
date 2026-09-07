//go:build windows

package vm

import (
	"errors"
	"io"
	"os"
)

func writeRuntimePathTar(io.Writer, string, string, os.FileInfo) error {
	return errors.New("runtime archive fixtures require a POSIX host")
}
