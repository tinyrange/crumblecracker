//go:build linux && arm64

package hv

import (
	"github.com/tinyrange/crumblecracker/internal/core/hv/kvm"
)

func Supports() error {
	_, err := kvm.Probe()
	return err
}

func NestedVirtualizationSupported() (bool, error) {
	return false, nil
}
