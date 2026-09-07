//go:build linux && arm64

package kvm

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	kvmAPIVersion      = 12
	kvmGetAPIVersion   = 0xae00
	kvmGetVcpuMmapSize = 0xae04
)

type Bootstrap struct {
	fd         int
	apiVersion int
}

type ProbeInfo struct {
	APIVersion   int
	VcpuMmapSize int
	IPABytes     uint64
	VMCreateOK   bool
	VCPUCreateOK bool
	VCPUInitOK   bool
}

func Open() (*Bootstrap, error) {
	fd, err := unix.Open("/dev/kvm", unix.O_CLOEXEC|unix.O_RDWR, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, fmt.Errorf("kvm unavailable: /dev/kvm does not exist; hardware virtualization is not available to this host or the KVM kernel module is not loaded")
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
			return nil, fmt.Errorf("kvm unavailable: cannot open /dev/kvm read/write: %w; give the current user access to /dev/kvm, usually by adding the user to the kvm group or adjusting device permissions", err)
		default:
			return nil, fmt.Errorf("kvm unavailable: open /dev/kvm: %w", err)
		}
	}

	version, err := ioctlInt(fd, kvmGetAPIVersion)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("kvm unavailable: query API version: %w", err)
	}
	if version != kvmAPIVersion {
		unix.Close(fd)
		return nil, fmt.Errorf("kvm unavailable: unsupported API version %d, want %d", version, kvmAPIVersion)
	}

	return &Bootstrap{fd: fd, apiVersion: version}, nil
}

func Probe() (ProbeInfo, error) {
	kvm, err := Open()
	if err != nil {
		return ProbeInfo{}, err
	}
	defer kvm.Close()

	vcpuMmapSize, err := kvm.VcpuMmapSize()
	if err != nil {
		return ProbeInfo{}, fmt.Errorf("kvm unavailable: query vcpu mmap size: %w", err)
	}
	ipaBits, err := kvm.CheckExtension(kvmCapArmVmIpaSize)
	if err != nil {
		return ProbeInfo{}, fmt.Errorf("kvm unavailable: query IPA size capability: %w", err)
	}

	info := ProbeInfo{
		APIVersion:   kvm.APIVersion(),
		VcpuMmapSize: vcpuMmapSize,
	}
	if ipaBits > 0 {
		info.IPABytes = 1 << ipaBits
	}
	vmfd, err := kvm.CreateVM()
	if err != nil {
		return ProbeInfo{}, fmt.Errorf("kvm unavailable: create VM: %w", err)
	}
	info.VMCreateOK = true
	vcpufd, err := kvm.CreateVCPU(vmfd, 0)
	if err != nil {
		_ = kvm.CloseVM(vmfd)
		return ProbeInfo{}, fmt.Errorf("kvm unavailable: create vcpu: %w", err)
	}
	info.VCPUCreateOK = true
	if err := kvm.InitVCPU(vmfd, vcpufd); err != nil {
		_ = kvm.CloseVCPU(vcpufd)
		_ = kvm.CloseVM(vmfd)
		return ProbeInfo{}, fmt.Errorf("kvm unavailable: initialize vcpu: %w", err)
	}
	info.VCPUInitOK = true
	if err := kvm.CloseVCPU(vcpufd); err != nil {
		_ = kvm.CloseVM(vmfd)
		return ProbeInfo{}, fmt.Errorf("kvm unavailable: close vcpu: %w", err)
	}
	if err := kvm.CloseVM(vmfd); err != nil {
		return ProbeInfo{}, fmt.Errorf("kvm unavailable: close VM: %w", err)
	}
	return info, nil
}

func (b *Bootstrap) Close() error {
	if b == nil || b.fd < 0 {
		return nil
	}
	err := unix.Close(b.fd)
	b.fd = -1
	return err
}

func (b *Bootstrap) APIVersion() int {
	if b == nil {
		return 0
	}
	return b.apiVersion
}

func (b *Bootstrap) VcpuMmapSize() (int, error) {
	if b == nil || b.fd < 0 {
		return 0, fmt.Errorf("kvm bootstrap is closed")
	}
	return ioctlInt(b.fd, kvmGetVcpuMmapSize)
}

func (b *Bootstrap) CheckExtension(cap int) (uint64, error) {
	if b == nil || b.fd < 0 {
		return 0, fmt.Errorf("kvm bootstrap is closed")
	}
	ret, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(b.fd), uintptr(kvmCheckExtension), uintptr(cap))
	if errno != 0 {
		return 0, errno
	}
	return uint64(ret), nil
}

func (b *Bootstrap) CreateVM() (int, error) {
	if b == nil || b.fd < 0 {
		return 0, fmt.Errorf("kvm bootstrap is closed")
	}
	ipaBits, err := b.CheckExtension(kvmCapArmVmIpaSize)
	if err != nil {
		return 0, err
	}
	vmfd, err := createVM(b.fd, uint32(ipaBits))
	if err != nil {
		return 0, err
	}
	if supported, err := b.CheckExtension(kvmCapArmNISVToUser); err == nil && supported != 0 {
		if err := enableCapability(vmfd, &kvmEnableCapData{Cap: kvmCapArmNISVToUser}); err != nil {
			_ = unix.Close(vmfd)
			return 0, fmt.Errorf("enable arm NISV exits: %w", err)
		}
	}
	return vmfd, nil
}

func (b *Bootstrap) CloseVM(fd int) error {
	if fd < 0 {
		return nil
	}
	return unix.Close(fd)
}

func (b *Bootstrap) CreateVCPU(vmfd int, id int) (int, error) {
	if b == nil || b.fd < 0 {
		return 0, fmt.Errorf("kvm bootstrap is closed")
	}
	return createVCPU(vmfd, id)
}

func (b *Bootstrap) InitVCPU(vmfd, vcpufd int) error {
	if b == nil || b.fd < 0 {
		return fmt.Errorf("kvm bootstrap is closed")
	}
	init, err := armPreferredTarget(vmfd)
	if err != nil {
		return err
	}
	enableArmVCPUFeature(&init, kvmArmVcpuFeaturePsci02)
	return armVCPUInit(vcpufd, &init)
}

func (b *Bootstrap) CloseVCPU(fd int) error {
	if fd < 0 {
		return nil
	}
	return unix.Close(fd)
}

func ioctlInt(fd int, request int) (int, error) {
	ret, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(request), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(ret), nil
}
