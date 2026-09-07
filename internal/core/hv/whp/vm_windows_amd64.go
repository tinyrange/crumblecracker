//go:build windows && amd64

package whp

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
)

type VM struct {
	part         partitionHandle
	mem          *allocation
	memSize      uint64
	memRegions   []guestMemoryRegion
	vcpuCount    uint32
	emulators    []emulatorHandle
	emuCallbacks emulatorCallbacks
	emuContexts  []*emulatorContext
	running      []atomic.Bool
}

type guestMemoryRegion struct {
	guestPhysAddr uint64
	memoryOffset  uint64
	size          uint64
}

type Exit struct {
	Reason runVPExitReason
	RIP    uint64
	RFLAGS uint64
}

func Supports() error {
	present, err := isHypervisorPresent()
	if err != nil {
		if probeErr := probePartitionSupport(); probeErr == nil {
			return nil
		} else {
			return fmt.Errorf("whp unavailable: query hypervisor presence: %w; partition probe: %w", err, probeErr)
		}
	}
	if !present {
		return fmt.Errorf("whp unavailable: hypervisor not present")
	}
	return nil
}

func probePartitionSupport() error {
	part, err := createPartition()
	if err != nil {
		return fmt.Errorf("create partition: %w", err)
	}
	if err := deletePartition(part); err != nil {
		return fmt.Errorf("delete partition: %w", err)
	}
	return nil
}

func NewVM(memorySize uint64) (*VM, error) {
	return newVM(memorySize, false)
}

func NewVMWithCPUs(memorySize uint64, cpus int) (*VM, error) {
	return newVMWithAllocation(memorySize, false, false, cpus, nil)
}

func newBootVM(memorySize uint64, cpus int) (*VM, error) {
	return newVMWithAllocation(memorySize, true, true, cpus, nil)
}

func newVM(memorySize uint64, localAPIC bool) (*VM, error) {
	return newVMWithAllocation(memorySize, localAPIC, false, 1, nil)
}

func newVMWithAllocation(memorySize uint64, localAPIC bool, splitAMD64Memory bool, cpus int, mem *allocation) (*VM, error) {
	if memorySize == 0 {
		return nil, fmt.Errorf("memory size must be non-zero")
	}
	if cpus <= 0 {
		cpus = 1
	}
	if cpus > 255 {
		return nil, fmt.Errorf("processor count %d exceeds the amd64 guest limit of 255", cpus)
	}
	cleanupMem := func() {
		if mem != nil {
			_ = mem.free()
			mem = nil
		}
	}
	part, err := createPartition()
	if err != nil {
		cleanupMem()
		return nil, fmt.Errorf("create partition: %w", err)
	}
	vm := &VM{part: part, running: make([]atomic.Bool, cpus)}
	if err := setPartitionProperty(part, partitionPropertyCodeProcessorCount, uint32(cpus)); err != nil {
		cleanupMem()
		_ = vm.Close()
		return nil, fmt.Errorf("set processor count: %w", err)
	}
	if err := setPartitionProperty(part, partitionPropertyCodeExtendedVMExits, uint64(1<<1)); err != nil {
		cleanupMem()
		_ = vm.Close()
		return nil, fmt.Errorf("set extended VM exits: %w", err)
	}
	if localAPIC {
		if err := setPartitionProperty(part, partitionPropertyCodeLocalAPICEmulationMode, localAPICEmulationModeXAPIC); err != nil {
			cleanupMem()
			_ = vm.Close()
			return nil, fmt.Errorf("set local APIC emulation mode: %w", err)
		}
	}
	if err := setupPartition(part); err != nil {
		cleanupMem()
		_ = vm.Close()
		return nil, fmt.Errorf("setup partition: %w", err)
	}
	if mem == nil {
		var err error
		mem, err = virtualAlloc(uintptr(memorySize))
		if err != nil {
			_ = vm.Close()
			return nil, fmt.Errorf("allocate guest memory: %w", err)
		}
	} else if uint64(mem.size) < memorySize {
		_ = mem.free()
		_ = vm.Close()
		return nil, fmt.Errorf("guest memory allocation size %d is smaller than VM memory %d", mem.size, memorySize)
	}
	vm.mem = mem
	vm.memSize = memorySize
	regions := []guestMemoryRegion{{size: memorySize}}
	if splitAMD64Memory {
		regions = amd64GuestMemoryRegions(memorySize)
	}
	for _, region := range regions {
		hostAddr := unsafe.Pointer(mem.addr + uintptr(region.memoryOffset))
		if err := mapGPARange(part, hostAddr, region.guestPhysAddr, region.size, mapGPARangeFlagRead|mapGPARangeFlagWrite|mapGPARangeFlagExecute); err != nil {
			_ = vm.Close()
			return nil, fmt.Errorf("map guest memory at gpa %#x: %w", region.guestPhysAddr, err)
		}
		vm.memRegions = append(vm.memRegions, region)
	}
	for index := 0; index < cpus; index++ {
		if err := createVirtualProcessor(part, uint32(index)); err != nil {
			_ = vm.Close()
			return nil, fmt.Errorf("create virtual processor %d: %w", index, err)
		}
		vm.vcpuCount++
	}
	if localAPIC {
		const startupSuspend = uint64(1)
		for index := uint32(1); index < vm.vcpuCount; index++ {
			if err := vm.SetVCPURegisters(index, map[registerName]uint64{
				registerInternalActivityState: startupSuspend,
			}); err != nil {
				_ = vm.Close()
				return nil, fmt.Errorf("put virtual processor %d in startup suspend: %w", index, err)
			}
		}
	}
	return vm, nil
}

func amd64GuestMemoryRegions(memorySize uint64) []guestMemoryRegion {
	lowSize := min(memorySize, uint64(amd64vm.LowMemorySize))
	regions := []guestMemoryRegion{{
		guestPhysAddr: amd64vm.MemoryBase,
		size:          lowSize,
	}}
	if highSize := memorySize - lowSize; highSize > 0 {
		regions = append(regions, guestMemoryRegion{
			guestPhysAddr: amd64vm.HighMemoryBase,
			memoryOffset:  lowSize,
			size:          highSize,
		})
	}
	return regions
}

func (v *VM) Close() error {
	if v == nil {
		return nil
	}
	var first error
	for _, emulator := range v.emulators {
		if emulator != 0 {
			if err := destroyEmulator(emulator); err != nil && first == nil {
				first = err
			}
		}
	}
	v.emulators = nil
	v.emuContexts = nil
	if v.part != 0 {
		for index := uint32(0); index < v.vcpuCount; index++ {
			_ = cancelRunVirtualProcessor(v.part, index)
		}
		for index := v.vcpuCount; index > 0; index-- {
			if err := deleteVirtualProcessor(v.part, index-1); err != nil && first == nil {
				first = err
			}
		}
		v.vcpuCount = 0
		v.running = nil
	}
	if v.part != 0 {
		for _, region := range v.memRegions {
			if err := unmapGPARange(v.part, region.guestPhysAddr, region.size); err != nil && first == nil {
				first = err
			}
		}
		v.memRegions = nil
	}
	if v.mem != nil {
		if err := v.mem.free(); err != nil && first == nil {
			first = err
		}
		v.mem = nil
	}
	if v.part != 0 {
		if err := deletePartition(v.part); err != nil && first == nil {
			first = err
		}
		v.part = 0
	}
	return first
}

func (v *VM) Memory() []byte {
	if v == nil || v.mem == nil {
		return nil
	}
	return v.mem.bytes()
}

func (v *VM) ReadIPA(addr uint64, size int) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid read size %d", size)
	}
	out := make([]byte, size)
	if err := v.ReadIPAInto(addr, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *VM) ReadIPAInto(addr uint64, dst []byte) error {
	mem, err := v.SliceIPA(addr, len(dst))
	if err != nil {
		return fmt.Errorf("read ipa %#x size %d: %w", addr, len(dst), err)
	}
	copy(dst, mem)
	return nil
}

func (v *VM) SliceIPA(addr uint64, size int) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid slice size %d", size)
	}
	if v == nil || v.mem == nil {
		return nil, fmt.Errorf("guest memory is not mapped")
	}
	for _, region := range v.memRegions {
		if addr < region.guestPhysAddr {
			continue
		}
		regionOffset := addr - region.guestPhysAddr
		if regionOffset > region.size || uint64(size) > region.size-regionOffset {
			continue
		}
		memoryOffset := region.memoryOffset + regionOffset
		return v.mem.bytes()[memoryOffset : memoryOffset+uint64(size)], nil
	}
	return nil, fmt.Errorf("guest physical range is not mapped")
}

func (v *VM) WriteIPA(addr uint64, data []byte) error {
	mem, err := v.SliceIPA(addr, len(data))
	if err != nil {
		return fmt.Errorf("write ipa %#x size %d: %w", addr, len(data), err)
	}
	copy(mem, data)
	return nil
}

func (v *VM) SetFlatProtectedMode(entry uint64) error {
	code := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x8, Attributes: segmentAttributes(11, 1, 0, 1, 0, 0, 1, 1)}
	data := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x10, Attributes: segmentAttributes(3, 1, 0, 1, 0, 0, 1, 1)}
	names := []registerName{
		registerCr0,
		registerCr3,
		registerCr4,
		registerEfer,
		registerCs,
		registerDs,
		registerEs,
		registerFs,
		registerGs,
		registerSs,
		registerRip,
		registerRsp,
		registerRflags,
	}
	values := []registerValue{
		uint64RegisterValue(1), // CR0.PE
		uint64RegisterValue(0),
		uint64RegisterValue(0),
		uint64RegisterValue(0),
		segmentRegisterValue(code),
		segmentRegisterValue(data),
		segmentRegisterValue(data),
		segmentRegisterValue(data),
		segmentRegisterValue(data),
		segmentRegisterValue(data),
		uint64RegisterValue(entry),
		uint64RegisterValue(v.memSize - 0x10),
		uint64RegisterValue(0x2),
	}
	if err := setVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return fmt.Errorf("set flat protected-mode registers: %w", err)
	}
	return nil
}

func (v *VM) SetProtectedMode32(entry, stack uint64) error {
	const (
		cr0PE = 1 << 0
		cr0MP = 1 << 1
		cr0ET = 1 << 4
		cr0NE = 1 << 5
		cr0PG = 1 << 31
	)
	code := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x10, Attributes: segmentAttributes(11, 1, 0, 1, 0, 0, 1, 1)}
	data := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x18, Attributes: segmentAttributes(3, 1, 0, 1, 0, 0, 1, 1)}
	names := []registerName{
		registerCr0,
		registerCr3,
		registerCr4,
		registerEfer,
		registerCs,
		registerDs,
		registerEs,
		registerFs,
		registerGs,
		registerSs,
		registerRip,
		registerRsp,
		registerRflags,
	}
	values := make([]registerValue, len(names))
	if err := getVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return fmt.Errorf("get protected-mode registers: %w", err)
	}
	values[0] = uint64RegisterValue((values[0].uint64() | cr0PE | cr0MP | cr0ET | cr0NE) &^ uint64(cr0PG))
	values[1] = uint64RegisterValue(0)
	values[2] = uint64RegisterValue(0)
	values[3] = uint64RegisterValue(0)
	values[4] = segmentRegisterValue(code)
	values[5] = segmentRegisterValue(data)
	values[6] = segmentRegisterValue(data)
	values[7] = segmentRegisterValue(data)
	values[8] = segmentRegisterValue(data)
	values[9] = segmentRegisterValue(data)
	values[10] = uint64RegisterValue(entry)
	values[11] = uint64RegisterValue(stack)
	values[12] = uint64RegisterValue(0x2)
	if err := setVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return fmt.Errorf("set protected-mode registers: %w", err)
	}
	return nil
}

func (v *VM) SetLongMode(entry, zeroPage, stack, pagingBase uint64) error {
	if err := v.setupPageTables(pagingBase, 4); err != nil {
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
	code := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x10, Attributes: segmentAttributes(11, 1, 0, 1, 0, 1, 0, 1)}
	data := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x18, Attributes: segmentAttributes(3, 1, 0, 1, 0, 0, 1, 1)}
	names := []registerName{
		registerCr3,
		registerCr4,
		registerCr0,
		registerEfer,
		registerCs,
		registerDs,
		registerEs,
		registerFs,
		registerGs,
		registerSs,
		registerRip,
		registerRsi,
		registerRsp,
		registerRflags,
	}
	values := make([]registerValue, len(names))
	if err := getVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return fmt.Errorf("get long-mode registers: %w", err)
	}
	values[0] = uint64RegisterValue(pagingBase)
	values[1] = uint64RegisterValue(values[1].uint64() | cr4PAE)
	values[2] = uint64RegisterValue(values[2].uint64() | cr0PE | cr0MP | cr0ET | cr0NE | cr0WP | cr0AM | cr0PG)
	values[3] = uint64RegisterValue(values[3].uint64() | eferLME | eferLMA)
	values[4] = segmentRegisterValue(code)
	values[5] = segmentRegisterValue(data)
	values[6] = segmentRegisterValue(data)
	values[7] = segmentRegisterValue(data)
	values[8] = segmentRegisterValue(data)
	values[9] = segmentRegisterValue(data)
	values[10] = uint64RegisterValue(entry)
	values[11] = uint64RegisterValue(zeroPage)
	values[12] = uint64RegisterValue(stack)
	values[13] = uint64RegisterValue(0x2)
	if err := setVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return fmt.Errorf("set long-mode registers: %w", err)
	}
	return nil
}

func (v *VM) SetFreeBSDLongMode(entry, stack, pagingBase uint64) error {
	if err := v.setupFreeBSDPageTables(pagingBase, 4); err != nil {
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
	code := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x8, Attributes: segmentAttributes(11, 1, 0, 1, 0, 1, 0, 1)}
	data := x64SegmentRegister{Base: 0, Limit: 0xffffffff, Selector: 0x10, Attributes: segmentAttributes(3, 1, 0, 1, 0, 0, 1, 1)}
	names := []registerName{
		registerCr3,
		registerCr4,
		registerCr0,
		registerEfer,
		registerCs,
		registerDs,
		registerEs,
		registerFs,
		registerGs,
		registerSs,
		registerRip,
		registerRsp,
		registerRflags,
	}
	values := make([]registerValue, len(names))
	if err := getVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return fmt.Errorf("get FreeBSD long-mode registers: %w", err)
	}
	values[0] = uint64RegisterValue(pagingBase)
	values[1] = uint64RegisterValue(values[1].uint64() | cr4PAE)
	values[2] = uint64RegisterValue(values[2].uint64() | cr0PE | cr0MP | cr0ET | cr0NE | cr0WP | cr0AM | cr0PG)
	values[3] = uint64RegisterValue(values[3].uint64() | eferLME | eferLMA)
	values[4] = segmentRegisterValue(code)
	values[5] = segmentRegisterValue(data)
	values[6] = segmentRegisterValue(data)
	values[7] = segmentRegisterValue(data)
	values[8] = segmentRegisterValue(data)
	values[9] = segmentRegisterValue(data)
	values[10] = uint64RegisterValue(entry)
	values[11] = uint64RegisterValue(stack)
	values[12] = uint64RegisterValue(0x2)
	if err := setVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return fmt.Errorf("set FreeBSD long-mode registers: %w", err)
	}
	return nil
}

func (v *VM) setupPageTables(pagingBase uint64, giB int) error {
	needed := pagingBase + uint64(0x3000+giB*0x1000)
	if needed > v.memSize {
		return fmt.Errorf("paging structures require %#x bytes, memory size %#x", needed, v.memSize)
	}
	mem := v.Memory()
	put64 := func(addr, value uint64) {
		binary.LittleEndian.PutUint64(mem[addr:addr+8], value)
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
	for off := pagingBase; off < needed; off += 8 {
		put64(off, 0)
	}
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

func (v *VM) setupFreeBSDPageTables(pagingBase uint64, giB int) error {
	_ = giB
	needed := pagingBase + 0x3000
	if needed > v.memSize {
		return fmt.Errorf("paging structures require %#x bytes, memory size %#x", needed, v.memSize)
	}
	mem := v.Memory()
	put64 := func(addr, value uint64) {
		binary.LittleEndian.PutUint64(mem[addr:addr+8], value)
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

func (v *VM) Run() (Exit, error) {
	var ctx runVPExitContext
	return v.runVCPUWithContext(0, &ctx)
}

func (v *VM) runWithContext(ctx *runVPExitContext) (Exit, error) {
	return v.runVCPUWithContext(0, ctx)
}

func (v *VM) runVCPUWithContext(index uint32, ctx *runVPExitContext) (Exit, error) {
	if v == nil || index >= v.vcpuCount {
		return Exit{}, fmt.Errorf("virtual processor %d out of range", index)
	}
	v.running[index].Store(true)
	err := runVirtualProcessor(v.part, index, ctx)
	v.running[index].Store(false)
	if err != nil {
		return Exit{}, fmt.Errorf("run virtual processor %d: %w", index, err)
	}
	return Exit{Reason: ctx.ExitReason, RIP: ctx.VpContext.Rip, RFLAGS: ctx.VpContext.Rflags}, nil
}

func (v *VM) runWithCancel(ctx context.Context, raw *runVPExitContext) (Exit, error) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cancelRunVirtualProcessor(v.part, 0)
		case <-done:
		}
	}()
	exit, err := v.runWithContext(raw)
	close(done)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Exit{}, ctxErr
		}
		return Exit{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil && exit.Reason == runVPExitReasonCanceled {
		return exit, ctxErr
	}
	return exit, nil
}

func (v *VM) GetRIP() (uint64, error) {
	names := []registerName{registerRip}
	values := make([]registerValue, 1)
	if err := getVirtualProcessorRegisters(v.part, 0, names, values); err != nil {
		return 0, err
	}
	return values[0].uint64(), nil
}

func (v *VM) SetRIP(rip uint64) error {
	return v.SetVCPURIP(0, rip)
}

func (v *VM) SetVCPURIP(index uint32, rip uint64) error {
	names := []registerName{registerRip}
	values := []registerValue{uint64RegisterValue(rip)}
	return setVirtualProcessorRegisters(v.part, index, names, values)
}

func (v *VM) SetRegisters(values map[registerName]uint64) error {
	return v.SetVCPURegisters(0, values)
}

func (v *VM) SetVCPURegisters(index uint32, values map[registerName]uint64) error {
	names := make([]registerName, 0, len(values))
	regs := make([]registerValue, 0, len(values))
	for name, value := range values {
		names = append(names, name)
		regs = append(regs, uint64RegisterValue(value))
	}
	return setVirtualProcessorRegisters(v.part, index, names, regs)
}

func (v *VM) RequestInterrupt(vector uint32) error {
	return v.RequestInterruptWithTrigger(vector, interruptTriggerEdge)
}

func (v *VM) RequestInterruptWithTrigger(vector uint32, trigger interruptTriggerMode) error {
	return requestInterrupt(v.part, vector, trigger, interruptTypeFixed, interruptDestinationPhysical, 0)
}

func (v *VM) RequestInterruptRoute(vector uint32, trigger interruptTriggerMode, typ interruptType, destinationMode interruptDestinationMode, destination uint32) error {
	return requestInterrupt(v.part, vector, trigger, typ, destinationMode, destination)
}

func (v *VM) NotifyInterruptWindow() error {
	return v.NotifyVCPUInterruptWindow(0)
}

func (v *VM) NotifyVCPUInterruptWindow(index uint32) error {
	if v == nil || v.part == 0 {
		return nil
	}
	const value = uint64(1 << 1)
	names := []registerName{registerDeliverabilityNotifications}
	values := []registerValue{uint64RegisterValue(value)}
	return setVirtualProcessorRegisters(v.part, index, names, values)
}

func (v *VM) SetPendingInterruption(vector uint8) error {
	return v.SetVCPUPendingInterruption(0, vector)
}

func (v *VM) SetVCPUPendingInterruption(index uint32, vector uint8) error {
	if v == nil || v.part == 0 {
		return nil
	}
	const interruptionPending = uint64(1)
	value := interruptionPending | uint64(vector)<<16
	names := []registerName{registerPendingInterruption}
	values := []registerValue{uint64RegisterValue(value)}
	return setVirtualProcessorRegisters(v.part, index, names, values)
}

func (v *VM) canSetPendingInterruption(vector uint8) (bool, error) {
	return v.vcpuCanSetPendingInterruption(0, vector)
}

func (v *VM) vcpuCanSetPendingInterruption(index uint32, vector uint8) (bool, error) {
	if v == nil || v.part == 0 {
		return false, nil
	}
	names := []registerName{
		registerPendingInterruption,
		registerCr8,
	}
	values := make([]registerValue, len(names))
	if err := getVirtualProcessorRegisters(v.part, index, names, values); err != nil {
		return false, err
	}
	if values[0].uint64() != 0 {
		return false, nil
	}
	priority := vector >> 4
	return priority == 0 || priority > uint8(values[1].uint64()), nil
}

func (v *VM) haltedAndInterruptible(vector uint8) (bool, error) {
	return v.vcpuHaltedAndInterruptible(0, vector)
}

func (v *VM) vcpuHaltedAndInterruptible(index uint32, vector uint8) (bool, error) {
	if v == nil || v.part == 0 {
		return false, nil
	}
	names := []registerName{
		registerRflags,
		registerPendingInterruption,
		registerInternalActivityState,
	}
	values := make([]registerValue, len(names))
	if err := getVirtualProcessorRegisters(v.part, index, names, values); err != nil {
		return false, err
	}
	const (
		rflagsInterruptEnable = uint64(1 << 9)
		haltSuspend           = uint64(1 << 1)
	)
	if values[0].uint64()&rflagsInterruptEnable == 0 {
		return false, nil
	}
	if values[1].uint64() != 0 {
		return false, nil
	}
	if values[2].uint64()&haltSuspend == 0 {
		return false, nil
	}
	priority := vector >> 4
	return priority != 0, nil
}

func (v *VM) kickOutOfHLT() error {
	return v.kickVCPUOutOfHLT(0)
}

func (v *VM) kickVCPUOutOfHLT(index uint32) error {
	if v == nil || v.part == 0 {
		return nil
	}
	names := []registerName{registerInternalActivityState}
	values := make([]registerValue, 1)
	if err := getVirtualProcessorRegisters(v.part, index, names, values); err != nil {
		return err
	}
	const haltSuspend = uint64(1 << 1)
	raw := values[0].uint64()
	if raw&haltSuspend == 0 {
		return nil
	}
	values[0] = uint64RegisterValue(raw &^ haltSuspend)
	return setVirtualProcessorRegisters(v.part, index, names, values)
}

func (v *VM) kickIfRunning() {
	v.kickVCPUIfRunning(0)
}

func (v *VM) kickVCPUIfRunning(index uint32) {
	if v == nil || index >= uint32(len(v.running)) || !v.running[index].Load() {
		return
	}
	_ = cancelRunVirtualProcessor(v.part, index)
}

func (v *VM) CancelRun() error {
	if v == nil || v.part == 0 {
		return nil
	}
	var first error
	for index := uint32(0); index < v.vcpuCount; index++ {
		if err := cancelRunVirtualProcessor(v.part, index); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func segmentAttributes(typ, s, dpl, present, avl, long, db, gran uint16) uint16 {
	return (typ & 0xf) |
		((s & 0x1) << 4) |
		((dpl & 0x3) << 5) |
		((present & 0x1) << 7) |
		((avl & 0x1) << 12) |
		((long & 0x1) << 13) |
		((db & 0x1) << 14) |
		((gran & 0x1) << 15)
}
