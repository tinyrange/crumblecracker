//go:build linux && arm64

package kvm

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
)

var errVGICUnsupported = errors.New("kvm: vgic device unsupported")

func initVGIC(vmfd int) (int, uint32, error) {
	fd, err := initVGICv3(vmfd)
	if err == nil {
		return fd, kvmDevTypeArmVgicV3, nil
	}
	if !errors.Is(err, errVGICUnsupported) {
		return 0, 0, err
	}
	fd, err = initVGICv2(vmfd)
	return fd, kvmDevTypeArmVgicV2, err
}

func finalizeVGIC(vgicfd int) error {
	return setDeviceAttr(vgicfd, &kvmDeviceAttr{
		Group: kvmDevArmVgicGrpCtrl,
		Attr:  kvmDevArmVgicCtrlInit,
	})
}

func initVGICv3(vmfd int) (int, error) {
	dev := kvmCreateDeviceArgs{Type: kvmDevTypeArmVgicV3}
	if err := createDevice(vmfd, &dev); err != nil {
		if errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EOPNOTSUPP) {
			return 0, errVGICUnsupported
		}
		return 0, fmt.Errorf("create vgicv3: %w", err)
	}
	fd := int(dev.Fd)
	if err := setDeviceAttrU32(fd, kvmDevArmVgicGrpNrIrqs, 0, 256); err != nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("set vgicv3 irq count: %w", err)
	}
	if err := setDeviceAttrU64(fd, kvmDevArmVgicGrpAddr, kvmVgicV3AddrTypeDist, arm64vm.GICDistributorMin); err != nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("set vgicv3 dist addr: %w", err)
	}
	if err := setDeviceAttrU64(fd, kvmDevArmVgicGrpAddr, kvmVgicV3AddrTypeRedist, arm64vm.GICRedistributorMin); err != nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("set vgicv3 redist addr: %w", err)
	}
	return fd, nil
}

func initVGICv2(vmfd int) (int, error) {
	dev := kvmCreateDeviceArgs{Type: kvmDevTypeArmVgicV2}
	if err := createDevice(vmfd, &dev); err != nil {
		return 0, fmt.Errorf("create vgicv2: %w", err)
	}
	fd := int(dev.Fd)
	if err := setDeviceAttrU32(fd, kvmDevArmVgicGrpNrIrqs, 0, 256); err != nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("set vgicv2 irq count: %w", err)
	}
	if err := setDeviceAttrU64(fd, kvmDevArmVgicGrpAddr, kvmVgicV2AddrTypeDist, arm64vm.GICDistributorMin); err != nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("set vgicv2 dist addr: %w", err)
	}
	if err := setDeviceAttrU64(fd, kvmDevArmVgicGrpAddr, kvmVgicV2AddrTypeCPU, arm64vm.GICDistributorMax); err != nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("set vgicv2 cpu addr: %w", err)
	}
	return fd, nil
}

func setDeviceAttrU32(fd int, group uint32, attr uint64, value uint32) error {
	v := value
	return setDeviceAttr(fd, &kvmDeviceAttr{
		Group: group,
		Attr:  attr,
		Addr:  uint64(uintptr(unsafe.Pointer(&v))),
	})
}

func setDeviceAttrU64(fd int, group uint32, attr uint64, value uint64) error {
	v := value
	return setDeviceAttr(fd, &kvmDeviceAttr{
		Group: group,
		Attr:  attr,
		Addr:  uint64(uintptr(unsafe.Pointer(&v))),
	})
}
