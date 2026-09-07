//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !windows

package virtio

import (
	"fmt"
	"os"
)

// Tail truncation and free-page reuse in imageDataStore provide the portable
// recovery path. Platforms with a range deallocation primitive can add it here
// without changing filesystem semantics.
func reclaimFileRange(*os.File, int64, int64) error { return errRangeReclaimUnsupported }

func allocatedFileBytes(file *os.File) (uint64, error) {
	return 0, fmt.Errorf("physical backing allocation is unavailable on this platform")
}
