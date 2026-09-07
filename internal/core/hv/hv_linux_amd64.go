//go:build linux && amd64

package hv

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	kvmAPIVersion    = 12
	kvmGetAPIVersion = 0xae00
)

func Supports() error {
	fd, err := unix.Open("/dev/kvm", unix.O_CLOEXEC|unix.O_RDWR, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return fmt.Errorf("kvm unavailable: /dev/kvm does not exist; hardware virtualization is not available to this host or the KVM kernel module is not loaded")
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
			return fmt.Errorf("kvm unavailable: cannot open /dev/kvm read/write: %w; give the current user access to /dev/kvm, usually by adding the user to the kvm group or adjusting device permissions", err)
		default:
			return fmt.Errorf("kvm unavailable: open /dev/kvm: %w", err)
		}
	}
	defer unix.Close(fd)

	version, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(kvmGetAPIVersion), 0)
	if errno != 0 {
		return fmt.Errorf("kvm unavailable: query API version: %w", errno)
	}
	if int(version) != kvmAPIVersion {
		return fmt.Errorf("kvm unavailable: unsupported API version %d, want %d", version, kvmAPIVersion)
	}
	return nil
}

func NestedVirtualizationSupported() (bool, error) {
	return false, nil
}
