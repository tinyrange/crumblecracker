//go:build windows && amd64

package hv

import "github.com/tinyrange/crumblecracker/internal/core/hv/whp"

func Supports() error {
	return whp.Supports()
}

func NestedVirtualizationSupported() (bool, error) {
	return false, nil
}
