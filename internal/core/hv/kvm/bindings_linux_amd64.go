//go:build linux && amd64

package kvm

import (
	"math/bits"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const cpuidMaxEntries = 255

const kvmCPUIDFlagSignificantIndex = 1

const (
	kvmCreateVM            = 0xae01
	kvmCheckExtension      = 0xae03
	kvmGetVcpuMmapSize     = 0xae04
	kvmGetSupportedCpuid   = 0xc008ae05
	kvmSetUserMemoryRegion = 0x4020ae46
	kvmIrqLine             = 0x4008ae61
	kvmCreateVCPU          = 0xae41
	kvmSetTssAddr          = 0xae47
	kvmCreateIrqchip       = 0xae60
	kvmGetIRQChip          = 0xc208ae62
	kvmSetIRQChip          = 0x8208ae63
	kvmCreatePit2          = 0x4040ae77
	kvmSetClock            = 0x4030ae7b
	kvmGetClock            = 0x8030ae7c
	kvmRun                 = 0xae80
	kvmGetRegs             = 0x8090ae81
	kvmSetRegs             = 0x4090ae82
	kvmGetSregs            = 0x8138ae83
	kvmSetSregs            = 0x4138ae84
	kvmGetMSRS             = 0xc008ae88
	kvmSetMSRS             = 0x4008ae89
	kvmGetFPU              = 0x81a0ae8c
	kvmSetFPU              = 0x41a0ae8d
	kvmGetLAPIC            = 0x8400ae8e
	kvmSetLAPIC            = 0x4400ae8f
	kvmSetCpuid2           = 0x4008ae90
	kvmGetMPState          = 0x8004ae98
	kvmSetMPState          = 0x4004ae99
	kvmGetVCPUEvents       = 0x8040ae9f
	kvmGetPIT2             = 0x8070ae9f
	kvmSetVCPUEvents       = 0x4040aea0
	kvmSetPIT2             = 0x4070aea0
	kvmGetDebugRegs        = 0x8080aea1
	kvmSetDebugRegs        = 0x4080aea2
	kvmGetXSAVE            = 0x9000aea4
	kvmSetXSAVE            = 0x5000aea5
	kvmGetXCRS             = 0x8188aea6
	kvmSetXCRS             = 0x4188aea7
	kvmGetTSCKHz           = 0xaea3
	kvmGetXSAVE2           = 0x9000aecf
)

const kvmCapXSAVE2 = 208

const (
	ia32TSCMSR        = 0x00000010
	ia32BIOSSignIDMSR = 0x0000008b
	ia32MiscEnableMSR = 0x000001a0
	ia32TSCAuxMSR     = 0xc0000103
)

const ia32MiscEnableDefault = (1 << 0) | (1 << 3) | (1 << 11) | (1 << 12) | (1 << 16) | (1 << 18) | (1 << 23)

func ioctl(fd uintptr, request uint64, arg uintptr) (uintptr, error) {
	v1, _, err := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(request), arg)
	if err != 0 {
		return 0, err
	}
	return v1, nil
}

func ioctlWithRetry(fd uintptr, request uint64, arg uintptr) (uintptr, error) {
	for {
		v1, err := ioctl(fd, request, arg)
		if err == unix.EINTR {
			continue
		}
		return v1, err
	}
}

func checkExtension(fd int, cap int) (uint64, error) {
	v1, _, err := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(kvmCheckExtension), uintptr(cap))
	if err != 0 {
		return 0, err
	}
	return uint64(v1), nil
}

func ioctlRunVCPU(fd uintptr) (uintptr, error) {
	for {
		v1, err := ioctl(fd, uint64(kvmRun), 0)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		return v1, err
	}
}

func ioctlRunVCPUInterruptible(fd uintptr) (uintptr, error) {
	for {
		v1, err := ioctl(fd, uint64(kvmRun), 0)
		if err == unix.EAGAIN {
			continue
		}
		return v1, err
	}
}

func createVM(fd int, machineType uint32) (int, error) {
	v1, err := ioctlWithRetry(uintptr(fd), uint64(kvmCreateVM), uintptr(machineType))
	if err != nil {
		return 0, err
	}
	return int(v1), nil
}

func createVCPU(fd int, id int) (int, error) {
	v1, err := ioctlWithRetry(uintptr(fd), uint64(kvmCreateVCPU), uintptr(id))
	if err != nil {
		return 0, err
	}
	return int(v1), nil
}

func setUserMemoryRegion(fd int, region *kvmUserspaceMemoryRegion) error {
	_, err := ioctlWithRetry(uintptr(fd), uint64(kvmSetUserMemoryRegion), uintptr(unsafe.Pointer(region)))
	return err
}

func irqLevel(vmFd int, irq uint32, high bool) error {
	level := kvmIRQLevel{IRQ: irq}
	if high {
		level.Level = 1
	}
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmIrqLine), uintptr(unsafe.Pointer(&level)))
	return err
}

func setTSSAddr(vmFd int, addr uint64) error {
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmSetTssAddr), uintptr(addr))
	return err
}

func createIRQChip(vmFd int) error {
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmCreateIrqchip), 0)
	return err
}

func createPIT(vmFd int) error {
	var cfg kvmPitConfig
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmCreatePit2), uintptr(unsafe.Pointer(&cfg)))
	return err
}

func getIRQChip(vmFd int, chipID uint32) (kvmIRQChip, error) {
	chip := kvmIRQChip{ChipID: chipID}
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmGetIRQChip), uintptr(unsafe.Pointer(&chip)))
	return chip, err
}

func setIRQChip(vmFd int, chip *kvmIRQChip) error {
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmSetIRQChip), uintptr(unsafe.Pointer(chip)))
	return err
}

func getPIT2(vmFd int) (kvmPITState2, error) {
	var state kvmPITState2
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmGetPIT2), uintptr(unsafe.Pointer(&state)))
	return state, err
}

func setPIT2(vmFd int, state *kvmPITState2) error {
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmSetPIT2), uintptr(unsafe.Pointer(state)))
	return err
}

func getClock(vmFd int) (kvmClockData, error) {
	var clock kvmClockData
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmGetClock), uintptr(unsafe.Pointer(&clock)))
	return clock, err
}

func setClock(vmFd int, clock *kvmClockData) error {
	_, err := ioctlWithRetry(uintptr(vmFd), uint64(kvmSetClock), uintptr(unsafe.Pointer(clock)))
	return err
}

func getSupportedCPUID(kvmFd int) (*kvmCPUID2, error) {
	size := unsafe.Sizeof(kvmCPUID2{}) + unsafe.Sizeof(kvmCPUIDEntry2{})*cpuidMaxEntries
	buf := make([]byte, size)
	cpuid := (*kvmCPUID2)(unsafe.Pointer(&buf[0]))
	cpuid.Nr = cpuidMaxEntries
	if _, err := ioctlWithRetry(uintptr(kvmFd), uint64(kvmGetSupportedCpuid), uintptr(unsafe.Pointer(cpuid))); err != nil {
		return nil, err
	}
	return cpuid, nil
}

func setVCPUID(vcpuFd int, cpuid *kvmCPUID2) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetCpuid2), uintptr(unsafe.Pointer(cpuid)))
	return err
}

func getVCPUTSCKHz(vcpuFd int) (uint32, error) {
	value, err := ioctlInt(vcpuFd, kvmGetTSCKHz)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, unix.EINVAL
	}
	return uint32(value), nil
}

func getVCPUMSR(vcpuFd int, index uint32) (uint64, error) {
	msrs := kvmMSRs1{
		NMSRs: 1,
		Entry: kvmMSREntry{
			Index: index,
		},
	}
	ret, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetMSRS), uintptr(unsafe.Pointer(&msrs)))
	if err != nil {
		return 0, err
	}
	if ret != 1 {
		return 0, unix.EIO
	}
	return msrs.Entry.Data, nil
}

func setVCPUMSR(vcpuFd int, index uint32, value uint64) error {
	msrs := kvmMSRs1{
		NMSRs: 1,
		Entry: kvmMSREntry{
			Index: index,
			Data:  value,
		},
	}
	ret, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetMSRS), uintptr(unsafe.Pointer(&msrs)))
	if err != nil {
		return err
	}
	if ret != 1 {
		return unix.EIO
	}
	return nil
}

func getFPU(vcpuFd int) (kvmFPU, error) {
	var fpu kvmFPU
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetFPU), uintptr(unsafe.Pointer(&fpu))); err != nil {
		return kvmFPU{}, err
	}
	return fpu, nil
}

func setFPU(vcpuFd int, fpu *kvmFPU) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetFPU), uintptr(unsafe.Pointer(fpu)))
	return err
}

func getLAPIC(vcpuFd int) (kvmLAPICState, error) {
	var lapic kvmLAPICState
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetLAPIC), uintptr(unsafe.Pointer(&lapic))); err != nil {
		return kvmLAPICState{}, err
	}
	return lapic, nil
}

func setLAPIC(vcpuFd int, lapic *kvmLAPICState) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetLAPIC), uintptr(unsafe.Pointer(lapic)))
	return err
}

func getMPState(vcpuFd int) (kvmMPState, error) {
	var state kvmMPState
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetMPState), uintptr(unsafe.Pointer(&state))); err != nil {
		return kvmMPState{}, err
	}
	return state, nil
}

func setMPState(vcpuFd int, state *kvmMPState) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetMPState), uintptr(unsafe.Pointer(state)))
	return err
}

func getVCPUEvents(vcpuFd int) (kvmVCPUEvents, error) {
	var events kvmVCPUEvents
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetVCPUEvents), uintptr(unsafe.Pointer(&events))); err != nil {
		return kvmVCPUEvents{}, err
	}
	return events, nil
}

func setVCPUEvents(vcpuFd int, events *kvmVCPUEvents) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetVCPUEvents), uintptr(unsafe.Pointer(events)))
	return err
}

func getDebugRegs(vcpuFd int) (kvmDebugRegs, error) {
	var regs kvmDebugRegs
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetDebugRegs), uintptr(unsafe.Pointer(&regs))); err != nil {
		return kvmDebugRegs{}, err
	}
	return regs, nil
}

func setDebugRegs(vcpuFd int, regs *kvmDebugRegs) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetDebugRegs), uintptr(unsafe.Pointer(regs)))
	return err
}

func getXSAVE(vcpuFd int) (kvmXSAVE, error) {
	var xsave kvmXSAVE
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetXSAVE), uintptr(unsafe.Pointer(&xsave))); err != nil {
		return kvmXSAVE{}, err
	}
	return xsave, nil
}

func setXSAVE(vcpuFd int, xsave *kvmXSAVE) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetXSAVE), uintptr(unsafe.Pointer(xsave)))
	return err
}

func getXSAVEBytes(vmFd int, vcpuFd int) ([]byte, error) {
	size := kvmXSAVEBufferSize(vmFd)
	buf := make([]byte, size)
	request := uint64(kvmGetXSAVE)
	if size > int(unsafe.Sizeof(kvmXSAVE{})) {
		request = uint64(kvmGetXSAVE2)
	}
	if _, err := ioctlWithRetry(uintptr(vcpuFd), request, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return nil, err
	}
	return buf, nil
}

func setXSAVEBytes(vmFd int, vcpuFd int, data []byte) error {
	size := kvmXSAVEBufferSize(vmFd)
	if len(data) > size {
		return unix.EINVAL
	}
	buf := make([]byte, size)
	copy(buf, data)
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetXSAVE), uintptr(unsafe.Pointer(&buf[0])))
	return err
}

func legacyXSAVEBytes(xsave *kvmXSAVE) []byte {
	if xsave == nil {
		return nil
	}
	size := int(unsafe.Sizeof(*xsave))
	out := make([]byte, size)
	src := unsafe.Slice((*byte)(unsafe.Pointer(xsave)), size)
	copy(out, src)
	return out
}

func kvmXSAVEBufferSize(vmFd int) int {
	const legacySize = int(unsafe.Sizeof(kvmXSAVE{}))
	size, err := checkExtension(vmFd, kvmCapXSAVE2)
	if err != nil || size < uint64(legacySize) {
		return legacySize
	}
	return int(size)
}

func getXCRS(vcpuFd int) (kvmXCRS, error) {
	var xcrs kvmXCRS
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetXCRS), uintptr(unsafe.Pointer(&xcrs))); err != nil {
		return kvmXCRS{}, err
	}
	return xcrs, nil
}

func setXCRS(vcpuFd int, xcrs *kvmXCRS) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetXCRS), uintptr(unsafe.Pointer(xcrs)))
	return err
}

func setCPUIDTopology(cpuid *kvmCPUID2, id, cpus int) {
	if cpuid == nil {
		return
	}
	if cpus < 1 {
		cpus = 1
	}
	if cpus > 255 {
		cpus = 255
	}
	logicalCount := uint32(cpus)
	apicIDSpace := uint32(1)
	if cpus > 1 {
		apicIDSpace = uint32(1 << bits.Len(uint(cpus-1)))
	}
	apicIDShift := uint32(bits.Len(uint(apicIDSpace - 1)))
	ensureCPUIDEntry(cpuid, 0xb, 0)
	ensureCPUIDEntry(cpuid, 0xb, 1)
	ensureCPUIDEntry(cpuid, 0xb, 2)
	configureExtendedTopology := func(e *kvmCPUIDEntry2) {
		switch e.Index {
		case 0:
			e.Eax = 0
			e.Ebx = 1
			e.Ecx = (1 << 8) | e.Index
			e.Edx = uint32(id)
		case 1:
			e.Eax = apicIDShift
			e.Ebx = logicalCount
			e.Ecx = (2 << 8) | e.Index
			e.Edx = uint32(id)
		default:
			e.Eax, e.Ebx, e.Ecx, e.Edx = 0, 0, e.Index, uint32(id)
		}
	}
	entries := cpuidEntries(cpuid)
	for i := range entries {
		e := &entries[i]
		switch e.Function {
		case 7:
			switch e.Index {
			case 0:
				if e.Eax > 1 {
					e.Eax = 1
				}
				e.Ebx &^= (uint32(1) << 6) | (uint32(1) << 13)
				e.Ecx &^= uint32(1) << 7
				e.Edx &^= uint32(1) << 20
			case 2:
				e.Eax, e.Ebx, e.Ecx, e.Edx = 0, 0, 0, 0
			}
		case 0xd:
			switch e.Index {
			case 0:
				e.Eax &^= (uint32(1) << 11) | (uint32(1) << 12)
			case 11, 12:
				e.Eax, e.Ebx, e.Ecx, e.Edx = 0, 0, 0, 0
			}
		case 1:
			e.Ebx = (e.Ebx &^ (uint32(0xffff) << 16)) | (logicalCount << 16) | (uint32(id&0xff) << 24)
			if cpus > 1 {
				e.Edx |= 1 << 28
			}
		case 0:
			if cpus > 1 && e.Eax < 0xb {
				e.Eax = 0xb
			}
		case 4:
			sharing := uint32(0)
			cacheLevel := (e.Eax >> 5) & 0x7
			if cacheLevel >= 3 {
				sharing = logicalCount - 1
			}
			e.Eax = (e.Eax &^ ((uint32(0xfff) << 14) | (uint32(0x3f) << 26))) |
				(sharing << 14) |
				((logicalCount - 1) << 26)
		case 0xb:
			configureExtendedTopology(e)
		case 0x1f:
			e.Eax, e.Ebx, e.Ecx, e.Edx = 0, 0, e.Index, 0
		}
	}
}

func setCPUIDBrandString(cpuid *kvmCPUID2, brand string) {
	if cpuid == nil {
		return
	}
	brand = strings.TrimSpace(brand)
	if brand == "" {
		return
	}

	maxExtended := ensureCPUIDEntry(cpuid, 0x80000000, 0)
	if maxExtended == nil {
		return
	}
	maxExtended.Flags &^= kvmCPUIDFlagSignificantIndex
	if maxExtended.Eax < 0x80000004 {
		maxExtended.Eax = 0x80000004
	}

	var encoded [48]byte
	copy(encoded[:], []byte(brand))
	for leaf := uint32(0); leaf < 3; leaf++ {
		entry := ensureCPUIDEntry(cpuid, 0x80000002+leaf, 0)
		if entry == nil {
			return
		}
		entry.Flags &^= kvmCPUIDFlagSignificantIndex
		off := leaf * 16
		entry.Eax = le32(encoded[off:])
		entry.Ebx = le32(encoded[off+4:])
		entry.Ecx = le32(encoded[off+8:])
		entry.Edx = le32(encoded[off+12:])
	}
}

func setCPUIDKVMHypervisor(cpuid *kvmCPUID2) {
	if cpuid == nil {
		return
	}
	feature := ensureCPUIDEntry(cpuid, 1, 0)
	if feature != nil {
		feature.Ecx |= 1 << 31
	}
	base := ensureCPUIDEntry(cpuid, 0x40000000, 0)
	if base == nil {
		return
	}
	base.Flags &^= kvmCPUIDFlagSignificantIndex
	if base.Eax < 0x40000001 {
		base.Eax = 0x40000001
	}
	copyCPUIDString12(base, "KVMKVMKVM\x00\x00\x00")
}

func setCPUIDKVMHypervisorFrequency(cpuid *kvmCPUID2, tscKHz uint32) {
	if cpuid == nil || tscKHz == 0 {
		return
	}
	setCPUIDKVMHypervisor(cpuid)
	base := ensureCPUIDEntry(cpuid, 0x40000000, 0)
	if base != nil && base.Eax < 0x40000010 {
		base.Eax = 0x40000010
	}

	frequency := ensureCPUIDEntry(cpuid, 0x40000010, 0)
	if frequency == nil {
		return
	}
	frequency.Flags &^= kvmCPUIDFlagSignificantIndex
	frequency.Eax = tscKHz
	frequency.Ebx = 0
	frequency.Ecx = 0
	frequency.Edx = 0
}

func copyCPUIDString12(entry *kvmCPUIDEntry2, value string) {
	var encoded [12]byte
	copy(encoded[:], value)
	entry.Ebx = le32(encoded[0:])
	entry.Ecx = le32(encoded[4:])
	entry.Edx = le32(encoded[8:])
}

func hostCPUBrandString() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "model name" {
			continue
		}
		return strings.TrimSpace(value)
	}
	return ""
}

func cpuidEntries(cpuid *kvmCPUID2) []kvmCPUIDEntry2 {
	if cpuid == nil {
		return nil
	}
	return unsafe.Slice(
		(*kvmCPUIDEntry2)(unsafe.Pointer(uintptr(unsafe.Pointer(cpuid))+unsafe.Sizeof(*cpuid))),
		cpuid.Nr,
	)
}

func cpuidStorage(cpuid *kvmCPUID2) []kvmCPUIDEntry2 {
	return unsafe.Slice(
		(*kvmCPUIDEntry2)(unsafe.Pointer(uintptr(unsafe.Pointer(cpuid))+unsafe.Sizeof(*cpuid))),
		cpuidMaxEntries,
	)
}

func ensureCPUIDEntry(cpuid *kvmCPUID2, function, index uint32) *kvmCPUIDEntry2 {
	entries := cpuidEntries(cpuid)
	for i := range entries {
		entry := &entries[i]
		if entry.Function == function && entry.Index == index {
			return entry
		}
	}
	if cpuid.Nr >= cpuidMaxEntries {
		return nil
	}
	storage := cpuidStorage(cpuid)
	entry := &storage[cpuid.Nr]
	*entry = kvmCPUIDEntry2{
		Function: function,
		Index:    index,
		Flags:    kvmCPUIDFlagSignificantIndex,
	}
	cpuid.Nr++
	return entry
}

func le32(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
}

func getRegs(vcpuFd int) (kvmRegs, error) {
	var regs kvmRegs
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetRegs), uintptr(unsafe.Pointer(&regs))); err != nil {
		return kvmRegs{}, err
	}
	return regs, nil
}

func setRegs(vcpuFd int, regs *kvmRegs) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetRegs), uintptr(unsafe.Pointer(regs)))
	return err
}

func getSRegs(vcpuFd int) (kvmSRegs, error) {
	var regs kvmSRegs
	if _, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmGetSregs), uintptr(unsafe.Pointer(&regs))); err != nil {
		return kvmSRegs{}, err
	}
	return regs, nil
}

func setSRegs(vcpuFd int, regs *kvmSRegs) error {
	_, err := ioctlWithRetry(uintptr(vcpuFd), uint64(kvmSetSregs), uintptr(unsafe.Pointer(regs)))
	return err
}
