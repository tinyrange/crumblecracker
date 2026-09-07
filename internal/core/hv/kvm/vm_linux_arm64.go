//go:build linux && arm64

package kvm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

type ExitReason uint32

const (
	ExitUnknown     ExitReason = 0
	ExitException   ExitReason = 1
	ExitMMIO        ExitReason = 6
	ExitShutdown    ExitReason = 8
	ExitSystemEvent ExitReason = 24
	ExitArmNISV     ExitReason = 28
)

type Exit struct {
	Reason      ExitReason
	PhysicalIPA uint64
	Syndrome    uint64
	MMIO        MMIOExit
	ArmNISV     ArmNISVExit
	SystemEvent uint32
}

type ArmNISVExit struct {
	ESRISS   uint64
	FaultIPA uint64
}

type MMIOExit struct {
	Addr  uint64
	Data  [8]byte
	Len   uint32
	Write bool
}

type VM struct {
	lifecycleMu   sync.RWMutex
	kvm           *Bootstrap
	vmfd          int
	vcpufd        int
	vgicfd        int
	vgicType      uint32
	run           []byte
	mem           []byte
	extraMem      [][]byte
	sharedMem     []arm64SharedMapping
	memRegion     kvmUserspaceMemoryRegion
	tid           atomic.Int32
	memoryDetach  []func()
	memoryClosing bool
}

type arm64SharedMapping struct {
	guestPhysAddr uint64
	mem           []byte
}

func NewVM() (*VM, error) {
	k, err := Open()
	if err != nil {
		return nil, err
	}
	vmfd, err := k.CreateVM()
	if err != nil {
		_ = k.Close()
		return nil, fmt.Errorf("create vm: %w", err)
	}
	vgicfd, vgicType, err := initVGIC(vmfd)
	if err != nil {
		_ = k.CloseVM(vmfd)
		_ = k.Close()
		return nil, fmt.Errorf("configure vgic: %w", err)
	}
	vcpufd, err := k.CreateVCPU(vmfd, 0)
	if err != nil {
		_ = unix.Close(vgicfd)
		_ = k.CloseVM(vmfd)
		_ = k.Close()
		return nil, fmt.Errorf("create vcpu: %w", err)
	}
	if err := finalizeVGIC(vgicfd); err != nil {
		_ = k.CloseVCPU(vcpufd)
		_ = unix.Close(vgicfd)
		_ = k.CloseVM(vmfd)
		_ = k.Close()
		return nil, fmt.Errorf("finalize vgic: %w", err)
	}
	if err := k.InitVCPU(vmfd, vcpufd); err != nil {
		_ = k.CloseVCPU(vcpufd)
		_ = unix.Close(vgicfd)
		_ = k.CloseVM(vmfd)
		_ = k.Close()
		return nil, fmt.Errorf("init vcpu: %w", err)
	}
	mmapSize, err := k.VcpuMmapSize()
	if err != nil {
		_ = k.CloseVCPU(vcpufd)
		_ = unix.Close(vgicfd)
		_ = k.CloseVM(vmfd)
		_ = k.Close()
		return nil, fmt.Errorf("get kvm_run mmap size: %w", err)
	}
	run, err := unix.Mmap(vcpufd, 0, mmapSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = k.CloseVCPU(vcpufd)
		_ = unix.Close(vgicfd)
		_ = k.CloseVM(vmfd)
		_ = k.Close()
		return nil, fmt.Errorf("mmap kvm_run: %w", err)
	}
	return &VM{kvm: k, vmfd: vmfd, vcpufd: vcpufd, vgicfd: vgicfd, vgicType: vgicType, run: run}, nil
}

func (v *VM) Close() error {
	if v == nil {
		return nil
	}
	v.lifecycleMu.Lock()
	if v.memoryClosing {
		v.lifecycleMu.Unlock()
		return nil
	}
	v.memoryClosing = true
	detaches := v.memoryDetach
	v.memoryDetach = nil
	v.lifecycleMu.Unlock()
	for _, detach := range detaches {
		if detach != nil {
			detach()
		}
	}
	v.lifecycleMu.Lock()
	defer v.lifecycleMu.Unlock()
	if len(v.run) != 0 {
		_ = unix.Munmap(v.run)
		v.run = nil
	}
	if len(v.mem) != 0 {
		_ = unix.Munmap(v.mem)
		v.mem = nil
	}
	for _, mem := range v.extraMem {
		if len(mem) != 0 {
			_ = unix.Munmap(mem)
		}
	}
	v.extraMem = nil
	v.sharedMem = nil
	if v.kvm != nil {
		_ = v.kvm.CloseVCPU(v.vcpufd)
		if v.vgicfd >= 0 {
			_ = unix.Close(v.vgicfd)
			v.vgicfd = -1
		}
		_ = v.kvm.CloseVM(v.vmfd)
		_ = v.kvm.Close()
		v.vcpufd = -1
		v.vmfd = -1
		v.kvm = nil
	}
	return nil
}

// RegisterGuestMemoryDetach registers work that must finish before guest
// memory is unmapped. Devices with asynchronous host-side workers use this to
// stop dereferencing cached virtqueue slices during VM teardown.
func (v *VM) RegisterGuestMemoryDetach(detach func()) {
	if v == nil || detach == nil {
		return
	}
	v.lifecycleMu.Lock()
	if v.kvm == nil || v.memoryClosing {
		v.lifecycleMu.Unlock()
		detach()
		return
	}
	v.memoryDetach = append(v.memoryDetach, detach)
	v.lifecycleMu.Unlock()
}

func (v *VM) MapAnonymousMemory(size uint64, guestPhysAddr uint64) ([]byte, error) {
	mem, err := unix.Mmap(-1, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap guest memory: %w", err)
	}
	region := kvmUserspaceMemoryRegion{
		Slot:          0,
		GuestPhysAddr: guestPhysAddr,
		MemorySize:    size,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(v.vmfd, &region); err != nil {
		_ = unix.Munmap(mem)
		return nil, fmt.Errorf("set user memory region: %w", err)
	}
	v.mem = mem
	v.memRegion = region
	return mem, nil
}

func (v *VM) MapMemory(mem []byte, guestPhysAddr uint64) error {
	if len(mem) == 0 {
		return fmt.Errorf("guest memory is empty")
	}
	region := kvmUserspaceMemoryRegion{
		Slot: 0, GuestPhysAddr: guestPhysAddr, MemorySize: uint64(len(mem)),
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(v.vmfd, &region); err != nil {
		return fmt.Errorf("set user memory region: %w", err)
	}
	v.mem = mem
	v.memRegion = region
	return nil
}

func (v *VM) MapAnonymousMemorySlot(slot uint32, size uint64, guestPhysAddr uint64) ([]byte, error) {
	mem, err := unix.Mmap(-1, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap guest memory slot %d: %w", slot, err)
	}
	region := kvmUserspaceMemoryRegion{
		Slot:          slot,
		GuestPhysAddr: guestPhysAddr,
		MemorySize:    size,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(v.vmfd, &region); err != nil {
		_ = unix.Munmap(mem)
		return nil, fmt.Errorf("set user memory region slot %d: %w", slot, err)
	}
	v.extraMem = append(v.extraMem, mem)
	return mem, nil
}

func (v *VM) MapMemoryAlias(slot uint32, guestPhysAddr uint64, mem []byte) error {
	if len(mem) == 0 {
		return fmt.Errorf("alias memory is empty")
	}
	region := kvmUserspaceMemoryRegion{
		Slot:          slot,
		GuestPhysAddr: guestPhysAddr,
		MemorySize:    uint64(len(mem)),
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(v.vmfd, &region); err != nil {
		return fmt.Errorf("set user memory alias: %w", err)
	}
	return nil
}

func (v *VM) MapSharedMemory(mem []byte, guestPhysAddr uint64) error {
	if len(mem) == 0 || guestPhysAddr%4096 != 0 || uint64(len(mem))%4096 != 0 {
		return fmt.Errorf("shared memory mapping must be non-empty and page-aligned")
	}
	size := uint64(len(mem))
	if guestPhysAddr > ^uint64(0)-(size-1) {
		return fmt.Errorf("shared memory mapping overflows guest address space")
	}
	if guestPhysAddr < 0x22000000 && 0x08000000 < guestPhysAddr+size {
		return fmt.Errorf("shared memory mapping overlaps the platform device aperture")
	}
	v.lifecycleMu.Lock()
	defer v.lifecycleMu.Unlock()
	if v.kvm == nil || v.memoryClosing {
		return fmt.Errorf("VM is closing")
	}
	if guestPhysAddr < v.memRegion.GuestPhysAddr+v.memRegion.MemorySize &&
		v.memRegion.GuestPhysAddr < guestPhysAddr+size {
		return fmt.Errorf("shared memory mapping overlaps guest RAM")
	}
	for _, existing := range v.sharedMem {
		if guestPhysAddr < existing.guestPhysAddr+uint64(len(existing.mem)) &&
			existing.guestPhysAddr < guestPhysAddr+size {
			return fmt.Errorf("shared memory mapping overlaps an existing guest mapping")
		}
	}
	slot := uint32(1 + len(v.extraMem) + len(v.sharedMem))
	region := kvmUserspaceMemoryRegion{
		Slot:          slot,
		GuestPhysAddr: guestPhysAddr,
		MemorySize:    size,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(v.vmfd, &region); err != nil {
		return fmt.Errorf("map shared memory slot %d: %w", slot, err)
	}
	v.sharedMem = append(v.sharedMem, arm64SharedMapping{guestPhysAddr: guestPhysAddr, mem: mem})
	return nil
}

func (v *VM) SetX(index int, value uint64) error {
	if index < 0 || index > 30 {
		return fmt.Errorf("invalid x register %d", index)
	}
	return setOneReg(v.vcpufd, regX(index), unsafe.Pointer(&value))
}

func (v *VM) SetPC(value uint64) error {
	return setOneReg(v.vcpufd, regPC, unsafe.Pointer(&value))
}

func (v *VM) SetPState(value uint64) error {
	return setOneReg(v.vcpufd, regPState, unsafe.Pointer(&value))
}

func (v *VM) SetSP(value uint64) error {
	return setOneReg(v.vcpufd, regSP, unsafe.Pointer(&value))
}

func (v *VM) SetSpEl1(value uint64) error {
	err := setOneReg(v.vcpufd, regSpEl1, unsafe.Pointer(&value))
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOENT) {
		return v.SetSP(value)
	}
	return err
}

func (v *VM) GetPC() (uint64, error) {
	var value uint64
	if err := getOneReg(v.vcpufd, regPC, unsafe.Pointer(&value)); err != nil {
		return 0, err
	}
	return value, nil
}

func (v *VM) GetArm64SysReg(op0, op1, crn, crm, op2 uint64) (uint64, error) {
	var value uint64
	if err := getOneReg(v.vcpufd, arm64SysReg(op0, op1, crn, crm, op2), unsafe.Pointer(&value)); err != nil {
		return 0, err
	}
	return value, nil
}

func (v *VM) Run(exit *Exit) error {
	return v.execute(exit, false)
}

func (v *VM) RunInterruptible(exit *Exit) error {
	return v.execute(exit, true)
}

func (v *VM) execute(exit *Exit, interruptible bool) error {
	if exit == nil {
		return fmt.Errorf("exit is nil")
	}
	run := (*kvmRunData)(unsafe.Pointer(&v.run[0]))
	run.immediateExit = 0
	var err error
	if interruptible {
		_, err = ioctlRunVCPUInterruptible(uintptr(v.vcpufd))
	} else {
		_, err = ioctlWithRetry(uintptr(v.vcpufd), uint64(kvmRun), 0)
	}
	if err != nil {
		return fmt.Errorf("run vcpu: %w", err)
	}
	reason := ExitReason(run.exitReason)
	*exit = Exit{Reason: reason}
	switch reason {
	case ExitMMIO:
		mmio := (*kvmExitMMIOData)(unsafe.Pointer(&run.anon0[0]))
		exit.MMIO = MMIOExit{
			Addr:  mmio.physAddr,
			Data:  mmio.data,
			Len:   mmio.len,
			Write: mmio.isWrite != 0,
		}
	case ExitSystemEvent:
		system := (*kvmSystemEvent)(unsafe.Pointer(&run.anon0[0]))
		exit.SystemEvent = system.typ
	case ExitArmNISV:
		nisv := (*kvmExitArmNISVData)(unsafe.Pointer(&run.anon0[0]))
		exit.ArmNISV = ArmNISVExit{ESRISS: nisv.esrISS, FaultIPA: nisv.faultIPA}
	case ExitShutdown:
	case ExitException:
		// KVM arm64 MMIO should normally be surfaced as KVM_EXIT_MMIO.
	default:
		if reason == 17 {
			ie := (*internalError)(unsafe.Pointer(&run.anon0[0]))
			return fmt.Errorf("internal error: %d", ie.Suberror)
		}
	}
	return nil
}

func (v *VM) RequestImmediateExit() {
	if v == nil {
		return
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	if len(v.run) == 0 {
		return
	}
	run := (*kvmRunData)(unsafe.Pointer(&v.run[0]))
	run.immediateExit = 1
	if tid := v.tid.Load(); tid > 0 {
		_ = unix.Tgkill(unix.Getpid(), int(tid), unix.SIGURG)
	}
}

func (v *VM) SetVCPUTID(tid int) {
	if v == nil {
		return
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	v.tid.Store(int32(tid))
}

func (v *VM) CancelRun() error {
	if v == nil {
		return nil
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	if len(v.run) == 0 {
		return nil
	}
	run := (*kvmRunData)(unsafe.Pointer(&v.run[0]))
	run.immediateExit = 1
	return nil
}

func (v *VM) CompleteMMIORead(value uint64, size uint32) {
	run := (*kvmRunData)(unsafe.Pointer(&v.run[0]))
	mmio := (*kvmExitMMIOData)(unsafe.Pointer(&run.anon0[0]))
	for i := range mmio.data {
		mmio.data[i] = 0
	}
	switch size {
	case 1:
		mmio.data[0] = byte(value)
	case 2:
		binary.LittleEndian.PutUint16(mmio.data[:2], uint16(value))
	case 4:
		binary.LittleEndian.PutUint32(mmio.data[:4], uint32(value))
	default:
		binary.LittleEndian.PutUint64(mmio.data[:8], value)
	}
}

func (v *VM) SetIRQ(line uint32, level bool) error {
	if v == nil {
		return fmt.Errorf("vm is nil")
	}
	if line >= 1020 {
		return fmt.Errorf("irq %d out of range", line)
	}
	kvmIRQ := (kvmArmIRQTypeSPI << 24) | ((line + 32) & 0xffff)
	if err := irqLevel(v.vmfd, kvmIRQ, level); err != nil {
		return fmt.Errorf("set irq %d level=%v: %w", line, level, err)
	}
	return nil
}

func (v *VM) ReadIPA(addr uint64, size int) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("vm is nil")
	}
	if size < 0 {
		return nil, fmt.Errorf("invalid read size %d", size)
	}
	if size == 0 {
		return []byte{}, nil
	}
	out := make([]byte, size)
	if err := v.ReadIPAInto(addr, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *VM) ReadIPAInto(addr uint64, dst []byte) error {
	if v == nil {
		return fmt.Errorf("vm is nil")
	}
	if len(dst) == 0 {
		return nil
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	start := v.memRegion.GuestPhysAddr
	end := start + v.memRegion.MemorySize
	if addr < start || addr+uint64(len(dst)) > end {
		return fmt.Errorf("read guest memory %#x size %d: unmapped", addr, len(dst))
	}
	off := addr - start
	copy(dst, v.mem[off:off+uint64(len(dst))])
	return nil
}

func (v *VM) SliceIPA(addr uint64, size int) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("vm is nil")
	}
	if size < 0 {
		return nil, fmt.Errorf("invalid slice size %d", size)
	}
	if size == 0 {
		return []byte{}, nil
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	start := v.memRegion.GuestPhysAddr
	end := start + v.memRegion.MemorySize
	if addr < start || addr+uint64(size) > end {
		return nil, fmt.Errorf("slice guest memory %#x size %d: unmapped", addr, size)
	}
	off := addr - start
	return v.mem[off : off+uint64(size)], nil
}

func (v *VM) WriteIPA(addr uint64, data []byte) error {
	if v == nil {
		return fmt.Errorf("vm is nil")
	}
	if len(data) == 0 {
		return nil
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	start := v.memRegion.GuestPhysAddr
	end := start + v.memRegion.MemorySize
	if addr < start || addr+uint64(len(data)) > end {
		return fmt.Errorf("write guest memory %#x size %d: unmapped", addr, len(data))
	}
	off := addr - start
	copy(v.mem[off:off+uint64(len(data))], data)
	return nil
}
