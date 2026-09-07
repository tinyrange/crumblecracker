//go:build linux && arm64

package kvm

const (
	kvmRegArm64         uint64 = 0x6000000000000000
	kvmRegSizeU64       uint64 = 0x0030000000000000
	kvmRegArmCoproShift        = 16
	kvmRegArmCore       uint64 = 0x0010 << kvmRegArmCoproShift
	kvmRegArm64SysReg   uint64 = 0x0013 << kvmRegArmCoproShift

	kvmRegArm64SysRegOp0Mask  uint64 = 0x000000000000c000
	kvmRegArm64SysRegOp0Shift        = 14
	kvmRegArm64SysRegOp1Mask  uint64 = 0x0000000000003800
	kvmRegArm64SysRegOp1Shift        = 11
	kvmRegArm64SysRegCrnMask  uint64 = 0x0000000000000780
	kvmRegArm64SysRegCrnShift        = 7
	kvmRegArm64SysRegCrmMask  uint64 = 0x0000000000000078
	kvmRegArm64SysRegCrmShift        = 3
	kvmRegArm64SysRegOp2Mask  uint64 = 0x0000000000000007
	kvmRegArm64SysRegOp2Shift        = 0
)

func arm64CoreRegister(offsetBytes uintptr) uint64 {
	return kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | uint64(offsetBytes/4)
}

func arm64SysReg(op0, op1, crn, crm, op2 uint64) uint64 {
	return kvmRegArm64 | kvmRegSizeU64 | kvmRegArm64SysReg |
		((op0 << kvmRegArm64SysRegOp0Shift) & kvmRegArm64SysRegOp0Mask) |
		((op1 << kvmRegArm64SysRegOp1Shift) & kvmRegArm64SysRegOp1Mask) |
		((crn << kvmRegArm64SysRegCrnShift) & kvmRegArm64SysRegCrnMask) |
		((crm << kvmRegArm64SysRegCrmShift) & kvmRegArm64SysRegCrmMask) |
		((op2 << kvmRegArm64SysRegOp2Shift) & kvmRegArm64SysRegOp2Mask)
}

var (
	regPC     = arm64CoreRegister(uintptr(32 * 8))
	regPState = arm64CoreRegister(uintptr(33 * 8))
	regSP     = arm64CoreRegister(uintptr(31 * 8))
	regSpEl1  = arm64SysReg(3, 4, 4, 1, 0)
)

func regX(index int) uint64 {
	return arm64CoreRegister(uintptr(index * 8))
}

func enableArmVCPUFeature(init *kvmVcpuInit, feature uint32) {
	word := feature / 32
	bit := feature % 32
	if word >= kvmArmVcpuInitFeatureWords {
		return
	}
	init.Features[word] |= 1 << bit
}
