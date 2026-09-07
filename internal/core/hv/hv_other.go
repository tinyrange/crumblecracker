//go:build (!darwin || !arm64) && (!linux || (!arm64 && !amd64)) && (!windows || (!amd64 && !arm64))

package hv

import (
	"fmt"
	"runtime"
)

func Supports() error {
	return fmt.Errorf("unsupported host: %s/%s", runtime.GOOS, runtime.GOARCH)
}

func NestedVirtualizationSupported() (bool, error) {
	return false, nil
}
