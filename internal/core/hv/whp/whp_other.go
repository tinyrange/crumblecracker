//go:build !windows || (!amd64 && !arm64)

package whp

import "fmt"

func Supports() error {
	return fmt.Errorf("whp unavailable: requires windows/amd64 or windows/arm64")
}
