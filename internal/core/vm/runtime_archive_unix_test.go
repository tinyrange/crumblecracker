//go:build !windows

package vm

import (
	"io"
	"os"

	"github.com/tinyrange/crumblecracker/internal/core/managed/guestagent"
)

func writeRuntimePathTar(w io.Writer, src, rootName string, info os.FileInfo) error {
	return guestagent.WritePathTar(w, src, rootName, info)
}
