//go:build linux && amd64

package kvm

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	"golang.org/x/sys/unix"
)

type ExitReason uint32

const (
	ExitUnknown     ExitReason = 0
	ExitIO          ExitReason = 2
	ExitHLT         ExitReason = 5
	ExitMMIO        ExitReason = 6
	ExitShutdown    ExitReason = 8
	ExitInternal    ExitReason = 17
	ExitSystemEvent ExitReason = 24
)

type Exit struct {
	Reason      ExitReason
	IO          IOExit
	MMIO        MMIOExit
	SystemEvent uint32
}

type IOExit struct {
	Port  uint16
	Data  []byte
	Size  uint8
	Count uint32
	Write bool
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
	vcpus         []*VCPU
	mem           []byte
	lowMemLimit   uint64
	regions       []memoryMapping
	memoryDetach  []func()
	memoryClosing bool
}

type VCPU struct {
	id  int
	fd  int
	run []byte
	tid atomic.Int32
}

type memoryMapping struct {
	guestPhysAddr uint64
	mem           []byte
	ownsMapping   bool
}

func NewVM() (*VM, error) {
	return NewVMWithCPUs(1)
}

func NewVMWithCPUs(cpus int) (*VM, error) {
	if cpus <= 0 {
		cpus = 1
	}
	k, err := sharedBootstrap()
	if err != nil {
		return nil, err
	}
	vmfd, err := k.CreateVM()
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	if err := k.InitVM(vmfd); err != nil {
		_ = k.CloseVM(vmfd)
		return nil, fmt.Errorf("init vm: %w", err)
	}
	mmapSize, err := k.VcpuMmapSize()
	if err != nil {
		_ = k.CloseVM(vmfd)
		return nil, fmt.Errorf("get kvm_run mmap size: %w", err)
	}
	vm := &VM{kvm: k, vmfd: vmfd}
	for id := 0; id < cpus; id++ {
		vcpufd, err := k.CreateVCPU(vmfd, id)
		if err != nil {
			_ = vm.Close()
			return nil, fmt.Errorf("create vcpu %d: %w", id, err)
		}
		if err := k.InitVCPUWithTopology(vmfd, vcpufd, id, cpus); err != nil {
			_ = k.CloseVCPU(vcpufd)
			_ = vm.Close()
			return nil, fmt.Errorf("init vcpu %d: %w", id, err)
		}
		run, err := unix.Mmap(vcpufd, 0, mmapSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			_ = k.CloseVCPU(vcpufd)
			_ = vm.Close()
			return nil, fmt.Errorf("mmap kvm_run vcpu %d: %w", id, err)
		}
		vm.vcpus = append(vm.vcpus, &VCPU{id: id, fd: vcpufd, run: run})
		if id == 0 {
			vm.vcpufd = vcpufd
		}
	}
	if err := vm.SetMiscEnable(); err != nil {
		vm.Close()
		return nil, fmt.Errorf("set vcpu misc enable: %w", err)
	}
	if err := vm.SetMicrocodeSignature(0xffffffff); err != nil {
		vm.Close()
		return nil, fmt.Errorf("set vcpu microcode signature: %w", err)
	}
	return vm, nil
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
	for _, vcpu := range v.vcpus {
		if vcpu == nil {
			continue
		}
		if len(vcpu.run) != 0 {
			_ = unix.Munmap(vcpu.run)
			vcpu.run = nil
		}
		if vcpu.fd >= 0 && v.kvm != nil {
			_ = v.kvm.CloseVCPU(vcpu.fd)
			vcpu.fd = -1
		}
	}
	v.vcpus = nil
	if len(v.mem) != 0 {
		_ = unix.Munmap(v.mem)
		v.mem = nil
	}
	v.lowMemLimit = 0
	for _, region := range v.regions {
		if region.ownsMapping && len(region.mem) != 0 {
			_ = unix.Munmap(region.mem)
		}
	}
	v.regions = nil
	if v.kvm != nil {
		_ = v.kvm.CloseVM(v.vmfd)
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
	return v.MapAnonymousMemorySlot(0, size, guestPhysAddr)
}

func (v *VM) MapAnonymousMemorySlot(slot uint32, size uint64, guestPhysAddr uint64) ([]byte, error) {
	mem, err := unix.Mmap(-1, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap guest memory: %w", err)
	}
	region := kvmUserspaceMemoryRegion{
		Slot:          slot,
		GuestPhysAddr: guestPhysAddr,
		MemorySize:    size,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(v.vmfd, &region); err != nil {
		_ = unix.Munmap(mem)
		return nil, fmt.Errorf("set user memory region: %w", err)
	}
	if slot == 0 {
		v.mem = mem
		v.lowMemLimit = uint64(len(mem))
	} else {
		v.regions = append(v.regions, memoryMapping{
			guestPhysAddr: guestPhysAddr,
			mem:           mem,
			ownsMapping:   true,
		})
	}
	return mem, nil
}

func (v *VM) MapSharedMemory(mem []byte, guestPhysAddr uint64) error {
	if len(mem) == 0 || guestPhysAddr%4096 != 0 || uint64(len(mem))%4096 != 0 {
		return fmt.Errorf("shared memory mapping must be non-empty and page-aligned")
	}
	size := uint64(len(mem))
	if guestPhysAddr > ^uint64(0)-(size-1) {
		return fmt.Errorf("shared memory mapping overflows guest address space")
	}
	// The current amd64 platform devices occupy the 0xd0000000 aperture.
	if guestPhysAddr < 0xd1000000 && 0xd0000000 < guestPhysAddr+size {
		return fmt.Errorf("shared memory mapping overlaps the platform device aperture")
	}
	v.lifecycleMu.Lock()
	defer v.lifecycleMu.Unlock()
	if v.kvm == nil || v.memoryClosing {
		return fmt.Errorf("VM is closing")
	}
	if rangesOverlapGuestMemory(guestPhysAddr, size, 0, v.lowMemLimit) {
		return fmt.Errorf("shared memory mapping overlaps guest RAM")
	}
	for _, existing := range v.regions {
		if rangesOverlapGuestMemory(guestPhysAddr, size, existing.guestPhysAddr, uint64(len(existing.mem))) {
			return fmt.Errorf("shared memory mapping overlaps an existing guest mapping")
		}
	}
	slot := uint32(1 + len(v.regions))
	region := kvmUserspaceMemoryRegion{
		Slot:          slot,
		GuestPhysAddr: guestPhysAddr,
		MemorySize:    size,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(v.vmfd, &region); err != nil {
		return fmt.Errorf("map shared memory slot %d: %w", slot, err)
	}
	v.regions = append(v.regions, memoryMapping{guestPhysAddr: guestPhysAddr, mem: mem})
	return nil
}

func rangesOverlapGuestMemory(a, aSize, b, bSize uint64) bool {
	return a < b+bSize && b < a+aSize
}

func mapAMD64GuestMemory(vm *VM, memoryMB uint64) ([]byte, error) {
	totalSize := amd64vm.MemorySizeBytes(memoryMB)
	mem, err := unix.Mmap(-1, 0, int(totalSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap guest memory: %w", err)
	}
	if err := mapAMD64GuestMemoryBytes(vm, memoryMB, mem); err != nil {
		_ = unix.Munmap(mem)
		return nil, err
	}
	return mem, nil
}

func mapAMD64GuestMemoryBytes(vm *VM, memoryMB uint64, mem []byte) error {
	lowSize := amd64vm.LowMemorySizeBytes(memoryMB)
	highSize := amd64vm.HighMemorySizeBytes(memoryMB)
	totalSize := lowSize + highSize
	if uint64(len(mem)) != totalSize {
		return fmt.Errorf("guest memory size %d does not match requested VM memory %d", len(mem), totalSize)
	}
	lowRegion := kvmUserspaceMemoryRegion{
		Slot:          0,
		GuestPhysAddr: amd64vm.MemoryBase,
		MemorySize:    lowSize,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := setUserMemoryRegion(vm.vmfd, &lowRegion); err != nil {
		return fmt.Errorf("set user memory region: %w", err)
	}
	vm.mem = mem
	vm.lowMemLimit = lowSize
	if highSize > 0 {
		highRegion := kvmUserspaceMemoryRegion{
			Slot:          1,
			GuestPhysAddr: amd64vm.HighMemoryBase,
			MemorySize:    highSize,
			UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[lowSize]))),
		}
		if err := setUserMemoryRegion(vm.vmfd, &highRegion); err != nil {
			vm.mem = nil
			vm.lowMemLimit = 0
			return fmt.Errorf("set user memory region: %w", err)
		}
		vm.regions = append(vm.regions, memoryMapping{
			guestPhysAddr: amd64vm.HighMemoryBase,
			mem:           mem[lowSize : lowSize+highSize],
		})
	}
	return nil
}

func (v *VM) Run(exit *Exit) error {
	return v.RunVCPU(0, exit)
}

func (v *VM) RunVCPU(index int, exit *Exit) error {
	if v == nil || index < 0 || index >= len(v.vcpus) {
		return fmt.Errorf("vcpu %d out of range", index)
	}
	return v.vcpus[index].Run(exit)
}

func (v *VM) RunVCPUInterruptible(index int, exit *Exit) error {
	if v == nil || index < 0 || index >= len(v.vcpus) {
		return fmt.Errorf("vcpu %d out of range", index)
	}
	return v.vcpus[index].RunInterruptible(exit)
}

func (c *VCPU) Run(exit *Exit) error {
	return c.execute(exit, false)
}

func (c *VCPU) RunInterruptible(exit *Exit) error {
	return c.execute(exit, true)
}

func (c *VCPU) execute(exit *Exit, interruptible bool) error {
	if exit == nil {
		return fmt.Errorf("exit is nil")
	}
	run := (*kvmRunData)(unsafe.Pointer(&c.run[0]))
	run.immediateExit = 0
	var err error
	if interruptible {
		_, err = ioctlRunVCPUInterruptible(uintptr(c.fd))
	} else {
		_, err = ioctlRunVCPU(uintptr(c.fd))
	}
	if err != nil {
		return fmt.Errorf("run vcpu: %w", err)
	}
	reason := ExitReason(run.exitReason)
	*exit = Exit{Reason: reason}
	switch reason {
	case ExitIO:
		ioData := (*kvmExitIoData)(unsafe.Pointer(&run.anon0[0]))
		dataLen := uint64(ioData.size) * uint64(ioData.count)
		data := c.run[ioData.dataOffset : ioData.dataOffset+dataLen]
		exit.IO = IOExit{
			Port:  ioData.port,
			Data:  data,
			Size:  ioData.size,
			Count: ioData.count,
			Write: ioData.direction != 0,
		}
	case ExitMMIO:
		mmio := (*kvmExitMMIOData)(unsafe.Pointer(&run.anon0[0]))
		exit.MMIO = MMIOExit{Addr: mmio.physAddr, Data: mmio.data, Len: mmio.len, Write: mmio.isWrite != 0}
	case ExitInternal:
		ie := (*internalError)(unsafe.Pointer(&run.anon0[0]))
		return fmt.Errorf("internal error: %d", ie.Suberror)
	case ExitSystemEvent:
		system := (*kvmSystemEvent)(unsafe.Pointer(&run.anon0[0]))
		exit.SystemEvent = system.typ
	}
	return nil
}

func (v *VM) RequestImmediateExit() {
	if v == nil {
		return
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	pid := unix.Getpid()
	for _, vcpu := range v.vcpus {
		if vcpu == nil || len(vcpu.run) == 0 {
			continue
		}
		run := (*kvmRunData)(unsafe.Pointer(&vcpu.run[0]))
		run.immediateExit = 1
		if tid := vcpu.tid.Load(); tid > 0 {
			_ = unix.Tgkill(pid, int(tid), unix.SIGURG)
		}
	}
}

func (v *VM) SetVCPUTID(index int, tid int) {
	if v == nil {
		return
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	if index < 0 || index >= len(v.vcpus) || v.vcpus[index] == nil {
		return
	}
	v.vcpus[index].tid.Store(int32(tid))
}

func (v *VM) CancelRun() error {
	if v == nil {
		return nil
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	for _, vcpu := range v.vcpus {
		if vcpu == nil || len(vcpu.run) == 0 {
			continue
		}
		run := (*kvmRunData)(unsafe.Pointer(&vcpu.run[0]))
		run.immediateExit = 1
	}
	return nil
}

func (v *VM) SetTSC(value uint64) error {
	if v == nil {
		return nil
	}
	for index, vcpu := range v.vcpus {
		if vcpu == nil || vcpu.fd < 0 {
			continue
		}
		if err := setVCPUMSR(vcpu.fd, ia32TSCMSR, value); err != nil {
			return fmt.Errorf("set tsc vcpu %d: %w", index, err)
		}
	}
	return nil
}

func (v *VM) SetTSCAux() error {
	if v == nil {
		return nil
	}
	for index, vcpu := range v.vcpus {
		if vcpu == nil || vcpu.fd < 0 {
			continue
		}
		if err := setVCPUMSR(vcpu.fd, ia32TSCAuxMSR, uint64(index)); err != nil {
			return fmt.Errorf("set tsc aux vcpu %d: %w", index, err)
		}
	}
	return nil
}

func (v *VM) SetMicrocodeSignature(signature uint32) error {
	for index, vcpu := range v.vcpus {
		if vcpu == nil {
			continue
		}
		if err := setVCPUMSR(vcpu.fd, ia32BIOSSignIDMSR, uint64(signature)<<32); err != nil {
			return fmt.Errorf("set vcpu %d microcode signature: %w", index, err)
		}
	}
	return nil
}

func (v *VM) SetMiscEnable() error {
	if v == nil {
		return nil
	}
	for index, vcpu := range v.vcpus {
		if vcpu == nil || vcpu.fd < 0 {
			continue
		}
		if err := setVCPUMSR(vcpu.fd, ia32MiscEnableMSR, ia32MiscEnableDefault); err != nil {
			return fmt.Errorf("set misc enable vcpu %d: %w", index, err)
		}
	}
	return nil
}

func (v *VM) GetPC() (uint64, error) {
	return v.GetVCPUPC(0)
}

func (v *VM) GetFaultAddress() (uint64, error) {
	if v == nil || len(v.vcpus) == 0 {
		return 0, fmt.Errorf("vcpu 0 out of range")
	}
	sregs, err := getSRegs(v.vcpus[0].fd)
	if err != nil {
		return 0, err
	}
	return sregs.Cr2, nil
}

func (v *VM) GetVCPUPC(index int) (uint64, error) {
	if v == nil || index < 0 || index >= len(v.vcpus) {
		return 0, fmt.Errorf("vcpu %d out of range", index)
	}
	regs, err := getRegs(v.vcpus[index].fd)
	if err != nil {
		return 0, err
	}
	return regs.Rip, nil
}

func (v *VM) VCPURegisters(index int) map[string]any {
	snapshot := map[string]any{"vcpu": index}
	if v == nil || index < 0 || index >= len(v.vcpus) {
		snapshot["error"] = fmt.Sprintf("vcpu %d out of range", index)
		return snapshot
	}
	vcpu := v.vcpus[index]
	if vcpu == nil || vcpu.fd < 0 {
		snapshot["error"] = "vcpu is closed"
		return snapshot
	}
	regs, err := getRegs(vcpu.fd)
	if err != nil {
		snapshot["error"] = err.Error()
		return snapshot
	}
	snapshot["rip"] = regs.Rip
	snapshot["rsp"] = regs.Rsp
	snapshot["rbp"] = regs.Rbp
	snapshot["rax"] = regs.Rax
	snapshot["rbx"] = regs.Rbx
	snapshot["rcx"] = regs.Rcx
	snapshot["rdx"] = regs.Rdx
	snapshot["rsi"] = regs.Rsi
	snapshot["rdi"] = regs.Rdi
	snapshot["rflags"] = regs.Rflags
	return snapshot
}

func (v *VM) CompleteMMIORead(value uint64, size uint32) {
	v.CompleteVCPUMMIORead(0, value, size)
}

func (v *VM) CompleteVCPUMMIORead(index int, value uint64, size uint32) {
	if v == nil || index < 0 || index >= len(v.vcpus) {
		return
	}
	v.vcpus[index].CompleteMMIORead(value, size)
}

func (c *VCPU) CompleteMMIORead(value uint64, size uint32) {
	run := (*kvmRunData)(unsafe.Pointer(&c.run[0]))
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
	if err := v.copyFromGuest(addr, dst); err != nil {
		return fmt.Errorf("read guest memory %#x size %d: unmapped", addr, len(dst))
	}
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
	region, off, ok := v.findMemoryRegion(addr)
	if !ok || uint64(size) > uint64(len(region))-off {
		return nil, fmt.Errorf("slice guest memory %#x size %d: unmapped", addr, size)
	}
	return region[off : off+uint64(size)], nil
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
	if err := v.copyToGuest(addr, data); err != nil {
		return fmt.Errorf("write guest memory %#x size %d: unmapped", addr, len(data))
	}
	return nil
}

func (v *VM) ReclaimGuestPage(ipa uint64) error {
	return v.ReclaimGuestPages(ipa, 4096)
}

func (v *VM) ReuseGuestPage(ipa uint64) error {
	return nil
}

func (v *VM) ReclaimGuestPages(ipa, size uint64) error {
	return v.madviseGuestRange(ipa, size, unix.MADV_DONTNEED)
}

func (v *VM) ReuseGuestPages(ipa, size uint64) error {
	return nil
}

func (v *VM) ReclaimGuestPageRanges(ranges [][2]uint64) error {
	if v == nil {
		return fmt.Errorf("vm is nil")
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	iovecs, err := v.guestRangeIOVectors(ranges)
	if err != nil {
		return err
	}
	if err := processMadviseSelf(iovecs, unix.MADV_DONTNEED); err == nil {
		return nil
	}
	for _, iovec := range iovecs {
		region := unsafe.Slice(iovec.Base, int(iovec.Len))
		if err := unix.Madvise(region, unix.MADV_DONTNEED); err != nil {
			return err
		}
	}
	return nil
}

func (v *VM) ReuseGuestPageRanges(ranges [][2]uint64) error {
	return nil
}

func (v *VM) guestRangeIOVectors(ranges [][2]uint64) ([]unix.Iovec, error) {
	iovecs := make([]unix.Iovec, 0, len(ranges))
	for _, pageRange := range ranges {
		ipa, size := pageRange[0], pageRange[1]
		if ipa%4096 != 0 || size%4096 != 0 {
			return nil, fmt.Errorf("guest memory range %#x+%#x is not page aligned", ipa, size)
		}
		for size != 0 {
			region, off, ok := v.findMemoryRegion(ipa)
			if !ok {
				return nil, fmt.Errorf("guest memory range at %#x: unmapped", ipa)
			}
			chunk := min(size, uint64(len(region))-off)
			if chunk == 0 {
				return nil, fmt.Errorf("guest memory range at %#x has invalid size %#x", ipa, chunk)
			}
			iovec := unix.Iovec{Base: &region[off]}
			iovec.SetLen(int(chunk))
			iovecs = append(iovecs, iovec)
			ipa += chunk
			size -= chunk
		}
	}
	return iovecs, nil
}

var selfProcessMadvise = struct {
	sync.Once
	fd  int
	err error
}{fd: -1}

func processMadviseSelf(iovecs []unix.Iovec, advice int) error {
	if len(iovecs) == 0 {
		return nil
	}
	selfProcessMadvise.Do(func() {
		selfProcessMadvise.fd, selfProcessMadvise.err = unix.PidfdOpen(os.Getpid(), 0)
	})
	if selfProcessMadvise.err != nil {
		return selfProcessMadvise.err
	}
	want := uint64(0)
	for i := range iovecs {
		want += iovecs[i].Len
	}
	advised, _, errno := unix.Syscall6(unix.SYS_PROCESS_MADVISE,
		uintptr(selfProcessMadvise.fd), uintptr(unsafe.Pointer(&iovecs[0])), uintptr(len(iovecs)), uintptr(advice), 0, 0)
	runtime.KeepAlive(iovecs)
	if errno != 0 {
		return errno
	}
	if uint64(advised) != want {
		return fmt.Errorf("process_madvise advised %d bytes, want %d", advised, want)
	}
	return nil
}

func (v *VM) madviseGuestRange(ipa, size uint64, advice int) error {
	if v == nil {
		return fmt.Errorf("vm is nil")
	}
	const pageSize = 4096
	if ipa%pageSize != 0 || size%pageSize != 0 {
		return fmt.Errorf("guest memory range %#x+%#x is not page aligned", ipa, size)
	}
	v.lifecycleMu.RLock()
	defer v.lifecycleMu.RUnlock()
	for size != 0 {
		region, off, ok := v.findMemoryRegion(ipa)
		if !ok {
			return fmt.Errorf("guest memory range at %#x: unmapped", ipa)
		}
		chunk := min(size, uint64(len(region))-off)
		if chunk == 0 {
			return fmt.Errorf("guest memory range at %#x has invalid size %#x", ipa, chunk)
		}
		if err := unix.Madvise(region[off:off+chunk], advice); err != nil {
			return err
		}
		ipa += chunk
		size -= chunk
	}
	return nil
}

func (v *VM) copyFromGuest(addr uint64, out []byte) error {
	for len(out) > 0 {
		region, off, ok := v.findMemoryRegion(addr)
		if !ok {
			return fmt.Errorf("unmapped")
		}
		n := copy(out, region[off:])
		out = out[n:]
		addr += uint64(n)
	}
	return nil
}

func (v *VM) copyToGuest(addr uint64, data []byte) error {
	for len(data) > 0 {
		region, off, ok := v.findMemoryRegion(addr)
		if !ok {
			return fmt.Errorf("unmapped")
		}
		n := copy(region[off:], data)
		data = data[n:]
		addr += uint64(n)
	}
	return nil
}

func (v *VM) findMemoryRegion(addr uint64) ([]byte, uint64, bool) {
	if addr < v.lowMemLimit {
		return v.mem, addr, true
	}
	for _, region := range v.regions {
		start := region.guestPhysAddr
		end := start + uint64(len(region.mem))
		if addr >= start && addr < end {
			return region.mem, addr - start, true
		}
	}
	return nil, 0, false
}

func (v *VM) SetLongMode(entry, zeroPage, stack, pagingBase uint64) error {
	if v == nil || len(v.vcpus) == 0 {
		return fmt.Errorf("missing bootstrap vcpu")
	}
	if err := v.setupPageTables(pagingBase, 4); err != nil {
		return err
	}
	bsp := v.vcpus[0]
	sregs, err := getSRegs(bsp.fd)
	if err != nil {
		return err
	}
	const (
		cr0PE   = 1 << 0
		cr0MP   = 1 << 1
		cr0ET   = 1 << 4
		cr0NE   = 1 << 5
		cr0WP   = 1 << 16
		cr0AM   = 1 << 18
		cr0PG   = 1 << 31
		cr4PAE  = 1 << 5
		eferLME = 1 << 8
		eferLMA = 1 << 10
	)
	sregs.Cr3 = pagingBase
	sregs.Cr4 |= cr4PAE
	sregs.Cr0 |= cr0PE | cr0MP | cr0ET | cr0NE | cr0WP | cr0AM | cr0PG
	sregs.Efer = eferLME | eferLMA

	code := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x10, Type: 11, Present: 1, S: 1, L: 1, G: 1}
	data := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x18, Type: 3, Present: 1, S: 1, Db: 1, G: 1}
	sregs.Cs = code
	sregs.Ds = data
	sregs.Es = data
	sregs.Fs = data
	sregs.Gs = data
	sregs.Ss = data
	if err := setSRegs(bsp.fd, &sregs); err != nil {
		return err
	}
	return setRegs(bsp.fd, &kvmRegs{
		Rip:    entry,
		Rsi:    zeroPage,
		Rsp:    stack,
		Rflags: 0x2,
	})
}

// SetFreestandingLongMode enters a direct-boot amd64 kernel with identity
// mappings for low memory and a matching alias at 0xffffffff80000000. RDI
// contains the guest-physical address of the boot information structure.
func (v *VM) SetFreestandingLongMode(entry, bootInfo, stack, pagingBase uint64) error {
	if v == nil || len(v.vcpus) == 0 {
		return fmt.Errorf("missing bootstrap vcpu")
	}
	if err := v.setupFreestandingPageTables(pagingBase); err != nil {
		return err
	}
	bsp := v.vcpus[0]
	sregs, err := getSRegs(bsp.fd)
	if err != nil {
		return err
	}
	const (
		cr0PE   = 1 << 0
		cr0MP   = 1 << 1
		cr0ET   = 1 << 4
		cr0NE   = 1 << 5
		cr0WP   = 1 << 16
		cr0AM   = 1 << 18
		cr0PG   = 1 << 31
		cr4PAE  = 1 << 5
		eferLME = 1 << 8
		eferLMA = 1 << 10
	)
	sregs.Cr3 = pagingBase
	sregs.Cr4 |= cr4PAE
	sregs.Cr0 |= cr0PE | cr0MP | cr0ET | cr0NE | cr0WP | cr0AM | cr0PG
	sregs.Efer = eferLME | eferLMA

	code := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x10, Type: 11, Present: 1, S: 1, L: 1, G: 1}
	data := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x18, Type: 3, Present: 1, S: 1, Db: 1, G: 1}
	sregs.Cs = code
	sregs.Ds = data
	sregs.Es = data
	sregs.Fs = data
	sregs.Gs = data
	sregs.Ss = data
	if err := setSRegs(bsp.fd, &sregs); err != nil {
		return err
	}
	return setRegs(bsp.fd, &kvmRegs{
		Rip:    entry,
		Rdi:    bootInfo,
		Rsp:    stack,
		Rflags: 0x2,
	})
}

func (v *VM) SetFreeBSDLongMode(entry, stack, pagingBase uint64) error {
	if v == nil || len(v.vcpus) == 0 {
		return fmt.Errorf("missing bootstrap vcpu")
	}
	if err := v.setupFreeBSDPageTables(pagingBase, 4); err != nil {
		return err
	}
	bsp := v.vcpus[0]
	sregs, err := getSRegs(bsp.fd)
	if err != nil {
		return err
	}
	const (
		cr0PE   = 1 << 0
		cr0MP   = 1 << 1
		cr0ET   = 1 << 4
		cr0NE   = 1 << 5
		cr0WP   = 1 << 16
		cr0AM   = 1 << 18
		cr0PG   = 1 << 31
		cr4PAE  = 1 << 5
		eferLME = 1 << 8
		eferLMA = 1 << 10
	)
	sregs.Cr3 = pagingBase
	sregs.Cr4 |= cr4PAE
	sregs.Cr0 |= cr0PE | cr0MP | cr0ET | cr0NE | cr0WP | cr0AM | cr0PG
	sregs.Efer = eferLME | eferLMA

	code := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x8, Type: 11, Present: 1, S: 1, L: 1, G: 1}
	data := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x10, Type: 3, Present: 1, S: 1, Db: 1, G: 1}
	sregs.Cs = code
	sregs.Ds = data
	sregs.Es = data
	sregs.Fs = data
	sregs.Gs = data
	sregs.Ss = data
	if err := setSRegs(bsp.fd, &sregs); err != nil {
		return err
	}
	return setRegs(bsp.fd, &kvmRegs{
		Rip:    entry,
		Rsp:    stack,
		Rflags: 0x2,
	})
}

func (v *VM) SetProtectedMode32(entry, stack uint64) error {
	if v == nil || len(v.vcpus) == 0 {
		return fmt.Errorf("missing bootstrap vcpu")
	}
	bsp := v.vcpus[0]
	sregs, err := getSRegs(bsp.fd)
	if err != nil {
		return err
	}
	const (
		cr0PE = 1 << 0
		cr0MP = 1 << 1
		cr0ET = 1 << 4
		cr0NE = 1 << 5
	)
	sregs.Cr0 = (sregs.Cr0 | cr0PE | cr0MP | cr0ET | cr0NE) &^ uint64(1<<31)
	sregs.Cr3 = 0
	sregs.Cr4 = 0
	sregs.Efer = 0
	code := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x10, Type: 11, Present: 1, S: 1, Db: 1, G: 1}
	data := kvmSegment{Base: 0, Limit: 0xffffffff, Selector: 0x18, Type: 3, Present: 1, S: 1, Db: 1, G: 1}
	sregs.Cs = code
	sregs.Ds = data
	sregs.Es = data
	sregs.Fs = data
	sregs.Gs = data
	sregs.Ss = data
	if err := setSRegs(bsp.fd, &sregs); err != nil {
		return err
	}
	return setRegs(bsp.fd, &kvmRegs{
		Rip:    entry,
		Rsp:    stack,
		Rflags: 0x2,
	})
}

func (v *VM) setupPageTables(pagingBase uint64, giB int) error {
	if pagingBase+uint64(0x3000+giB*0x1000) > uint64(len(v.mem)) {
		return fmt.Errorf("paging structures do not fit")
	}
	put64 := func(addr, value uint64) {
		binary.LittleEndian.PutUint64(v.mem[addr:addr+8], value)
	}
	pml4 := pagingBase
	pdpt := pagingBase + 0x1000
	pdBase := pagingBase + 0x2000
	const (
		p  = 1 << 0
		rw = 1 << 1
		us = 1 << 2
		ps = 1 << 7
	)
	put64(pml4, pdpt|p|rw|us)
	for g := 0; g < giB; g++ {
		pd := pdBase + uint64(g)*0x1000
		put64(pdpt+uint64(g)*8, pd|p|rw|us)
		for i := 0; i < 512; i++ {
			phys := (uint64(g) << 30) | (uint64(i) << 21)
			put64(pd+uint64(i)*8, phys|p|rw|us|ps)
		}
	}
	return nil
}

func (v *VM) setupFreestandingPageTables(pagingBase uint64) error {
	const tableBytes = uint64(5 * 0x1000)
	if pagingBase > v.lowMemLimit || tableBytes > v.lowMemLimit-pagingBase {
		return fmt.Errorf("freestanding paging structures do not fit in low memory")
	}
	clear(v.mem[pagingBase : pagingBase+tableBytes])
	put64 := func(addr, value uint64) {
		binary.LittleEndian.PutUint64(v.mem[addr:addr+8], value)
	}
	pml4 := pagingBase
	pdpt := pagingBase + 0x1000
	pd := pagingBase + 0x2000
	pt := pagingBase + 0x3000
	const (
		present = 1 << 0
		write   = 1 << 1
		user    = 1 << 2
		large   = 1 << 7
	)
	// Identity-map the first GiB for bootstrap data and alias that same range
	// at 0xffffffff80000000 for the kernel's higher-half image.
	put64(pml4+0*8, pdpt|present|write|user)
	put64(pml4+511*8, pdpt|present|write)
	put64(pdpt+0*8, pd|present|write|user)
	put64(pdpt+510*8, pd|present|write)
	for index := uint64(0); index < 512; index++ {
		put64(pd+index*8, index*0x200000|present|write|large)
	}
	// Reserve one lower-half 2 MiB window for the first userspace task. Leaves
	// begin supervisor-only; the kernel memory service grants individual pages.
	put64(pd+2*8, pt|present|write|user)
	for index := uint64(0); index < 512; index++ {
		put64(pt+index*8, 0x400000+index*0x1000|present|write)
	}
	return nil
}

func (v *VM) setupFreeBSDPageTables(pagingBase uint64, giB int) error {
	if pagingBase+0x3000 > uint64(len(v.mem)) {
		return fmt.Errorf("paging structures do not fit")
	}
	put64 := func(addr, value uint64) {
		binary.LittleEndian.PutUint64(v.mem[addr:addr+8], value)
	}
	pml4 := pagingBase
	pdpt := pagingBase + 0x1000
	pd := pagingBase + 0x2000
	const (
		p  = 1 << 0
		rw = 1 << 1
		us = 1 << 2
		ps = 1 << 7
	)
	for i := 0; i < 512; i++ {
		put64(pml4+uint64(i)*8, pdpt|p|rw|us)
		put64(pdpt+uint64(i)*8, pd|p|rw|us)
		phys := uint64(i) << 21
		put64(pd+uint64(i)*8, phys|p|rw|us|ps)
	}
	return nil
}

func (v *VM) SetIRQ(line uint32, level bool) error {
	if v == nil {
		return fmt.Errorf("vm is nil")
	}
	if line >= kvmNrInterrupts {
		return fmt.Errorf("irq %d out of range", line)
	}
	if err := irqLevel(v.vmfd, line, level); err != nil {
		return fmt.Errorf("set irq %d level=%v: %w", line, level, err)
	}
	return nil
}
