//go:build darwin && arm64

package hv

import "github.com/tinyrange/crumblecracker/internal/core/hv/hvf"

func Supports() error {
	return nil
}

func NestedVirtualizationSupported() (bool, error) {
	return hvf.NestedVirtualizationSupported()
}
