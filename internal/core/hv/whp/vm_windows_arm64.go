//go:build windows && arm64

package whp

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"time"
	"unsafe"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
)

type VM struct {
	part        partitionHandle
	mem         *allocation
	memGPA      uint64
	memSize     uint64
	vcpuCreated bool
}

const (
	defaultCNTVOverflowInterrupt = 27
	defaultPMUInterrupt          = 23
)

type VMOptions struct {
	CNTVOverflowInterrupt uint32
	GICLPIIntIDBits       uint32
}

type Exit struct {
	Reason runVPExitReason
	PC     uint64
	CPSR   uint64
	MMIO   MMIOExit
}

type MMIOExit struct {
	Addr       uint64
	Data       [8]byte
	Len        uint32
	Write      bool
	Reg        int
	SignExtend bool
	SF         bool
	PC         uint64
	NextPC     uint64
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
	features, err := getCapability[capabilityFeatures](capabilityCodeFeatures)
	if err != nil {
		return fmt.Errorf("whp unavailable: query features: %w", err)
	}
	if !features.arm64Support() {
		return fmt.Errorf("whp unavailable: arm64 support not reported")
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

func NewVM(memorySize uint64, memoryBase uint64) (*VM, error) {
	return NewVMWithOptions(memorySize, memoryBase, VMOptions{})
}

func NewVMWithOptions(memorySize uint64, memoryBase uint64, opts VMOptions) (*VM, error) {
	return newVMWithAllocation(memorySize, memoryBase, opts, nil)
}

func newVMWithAllocation(memorySize uint64, memoryBase uint64, opts VMOptions, mem *allocation) (*VM, error) {
	if memorySize == 0 {
		return nil, fmt.Errorf("memory size must be non-zero")
	}
	trace := os.Getenv("CC_WHP_ARM64_TIMING") != ""
	createStart := time.Now()
	traceStep := func(name string, start time.Time, err error) {
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 vm create +%s: %s took=%s err=%v\n", time.Since(createStart).Round(time.Millisecond), name, time.Since(start).Round(time.Millisecond), err)
		}
	}
	stepStart := time.Now()
	part, err := createPartition()
	traceStep("create partition", stepStart, err)
	if err != nil {
		return nil, fmt.Errorf("create partition: %w", err)
	}
	vm := &VM{part: part, memGPA: memoryBase}
	stepStart = time.Now()
	if err := setPartitionProperty(part, partitionPropertyCodeProcessorCount, uint32(1)); err != nil {
		traceStep("set processor count", stepStart, err)
		_ = vm.Close()
		return nil, fmt.Errorf("set processor count: %w", err)
	}
	traceStep("set processor count", stepStart, nil)
	stepStart = time.Now()
	opts, err = normalizeVMOptions(opts)
	traceStep("normalize options", stepStart, err)
	if err != nil {
		_ = vm.Close()
		return nil, err
	}
	ic := arm64ICParameters{
		EmulationMode: arm64ICEmulationModeGICV3,
		GICV3Parameters: arm64ICGICV3Parameters{
			GICDBaseAddress:           0x08000000,
			GITSTranslaterBaseAddress: 0x08080000,
			GICLPIIntIDBits:           opts.GICLPIIntIDBits,
			GICPPIOverflowFromCNTV:    opts.CNTVOverflowInterrupt,
			GICPPIPerformanceMonitors: defaultPMUInterrupt,
		},
	}
	stepStart = time.Now()
	if err := setPartitionProperty(part, partitionPropertyCodeArm64ICParameters, ic); err != nil {
		traceStep("set interrupt controller parameters", stepStart, err)
		_ = vm.Close()
		return nil, fmt.Errorf("set arm64 interrupt controller parameters: %w", err)
	}
	traceStep("set interrupt controller parameters", stepStart, nil)
	stepStart = time.Now()
	if err := setupPartition(part); err != nil {
		traceStep("setup partition", stepStart, err)
		_ = vm.Close()
		return nil, fmt.Errorf("setup partition: %w", err)
	}
	traceStep("setup partition", stepStart, nil)
	blankMemory := mem == nil
	if mem == nil {
		var err error
		stepStart = time.Now()
		mem, err = virtualAlloc(uintptr(memorySize))
		traceStep("allocate guest memory", stepStart, err)
		if err != nil {
			_ = vm.Close()
			return nil, fmt.Errorf("allocate guest memory: %w", err)
		}
	}
	vm.mem = mem
	vm.memSize = memorySize
	if blankMemory {
		stepStart = time.Now()
		vm.preTouchGuestMemory()
		traceStep("pre-touch guest memory", stepStart, nil)
	}
	stepStart = time.Now()
	if err := mapGPARange(part, unsafe.Pointer(mem.addr), memoryBase, memorySize, mapGPARangeFlagRead|mapGPARangeFlagWrite|mapGPARangeFlagExecute); err != nil {
		traceStep("map guest memory", stepStart, err)
		_ = vm.Close()
		return nil, fmt.Errorf("map guest memory: %w", err)
	}
	traceStep("map guest memory", stepStart, nil)
	stepStart = time.Now()
	vm.populateGuestMemory()
	traceStep("populate guest memory", stepStart, nil)
	stepStart = time.Now()
	if err := createVirtualProcessor(part, 0); err != nil {
		traceStep("create virtual processor", stepStart, err)
		_ = vm.Close()
		return nil, fmt.Errorf("create virtual processor: %w", err)
	}
	traceStep("create virtual processor", stepStart, nil)
	vm.vcpuCreated = true
	stepStart = time.Now()
	if err := vm.setRegister(registerGICR, arm64vm.GICRedistributorMin); err != nil {
		traceStep("set gic redistributor base", stepStart, err)
		_ = vm.Close()
		return nil, fmt.Errorf("set gic redistributor base: %w", err)
	}
	traceStep("set gic redistributor base", stepStart, nil)
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 vm create +%s: total\n", time.Since(createStart).Round(time.Millisecond))
	}
	return vm, nil
}

func (v *VM) preTouchGuestMemory() {
	if v.mem == nil {
		return
	}
	// WHP/arm64 can otherwise populate backing pages lazily on the guest's
	// first writes, making early kernel page clearing pathologically slow.
	mem := v.mem.bytes()
	const pageSize = 4096
	for off := 0; off < len(mem); off += pageSize {
		mem[off] = 0
	}
	if len(mem) > 0 {
		mem[len(mem)-1] = 0
	}
}

func (v *VM) populateGuestMemory() {
	flags := adviseGPARangePopulateFlagPrefetch | adviseGPARangePopulateFlagAvoidHardFaults
	_ = adviseGPARangePopulate(v.part, v.memGPA, v.memSize, memoryAccessRead, flags)
	_ = adviseGPARangePopulate(v.part, v.memGPA, v.memSize, memoryAccessWrite, flags)
	_ = adviseGPARangePopulate(v.part, v.memGPA, v.memSize, memoryAccessExecute, flags)
}

func normalizeVMOptions(opts VMOptions) (VMOptions, error) {
	if opts.CNTVOverflowInterrupt == 0 {
		opts.CNTVOverflowInterrupt = defaultCNTVOverflowInterrupt
	}
	if opts.GICLPIIntIDBits == 0 {
		lpiBits, err := getCapability[uint32](capabilityCodeGicLpiIntIDBits)
		if err != nil || lpiBits == 0 {
			lpiBits = 16
		}
		opts.GICLPIIntIDBits = lpiBits
	}
	return opts, nil
}

func (v *VM) Close() error {
	if v == nil {
		return nil
	}
	trace := os.Getenv("CC_WHP_ARM64_TIMING") != ""
	closeStart := time.Now()
	traceStep := func(name string, start time.Time, err error) {
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 vm close +%s: %s took=%s err=%v\n", time.Since(closeStart).Round(time.Millisecond), name, time.Since(start).Round(time.Millisecond), err)
		}
	}
	var first error
	if v.part != 0 && v.vcpuCreated {
		stepStart := time.Now()
		_ = cancelRunVirtualProcessor(v.part, 0)
		err := deleteVirtualProcessor(v.part, 0)
		traceStep("delete virtual processor", stepStart, err)
		if err != nil && first == nil {
			first = err
		}
		v.vcpuCreated = false
	}
	if v.part != 0 && v.mem != nil {
		stepStart := time.Now()
		err := unmapGPARange(v.part, v.memGPA, v.memSize)
		traceStep("unmap guest memory", stepStart, err)
		if err != nil && first == nil {
			first = err
		}
	}
	if v.mem != nil {
		stepStart := time.Now()
		err := v.mem.free()
		traceStep("free guest memory", stepStart, err)
		if err != nil && first == nil {
			first = err
		}
		v.mem = nil
	}
	if v.part != 0 {
		stepStart := time.Now()
		err := deletePartition(v.part)
		traceStep("delete partition", stepStart, err)
		if err != nil && first == nil {
			first = err
		}
		v.part = 0
	}
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 vm close +%s: total err=%v\n", time.Since(closeStart).Round(time.Millisecond), first)
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
	if v == nil || v.mem == nil {
		return fmt.Errorf("guest memory is not mapped")
	}
	if addr < v.memGPA || addr-v.memGPA > v.memSize || uint64(len(dst)) > v.memSize-(addr-v.memGPA) {
		return fmt.Errorf("read ipa %#x size %d out of range [%#x,%#x)", addr, len(dst), v.memGPA, v.memGPA+v.memSize)
	}
	off := addr - v.memGPA
	copy(dst, v.mem.bytes()[off:off+uint64(len(dst))])
	return nil
}

func (v *VM) SliceIPA(addr uint64, size int) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid slice size %d", size)
	}
	if v == nil || v.mem == nil {
		return nil, fmt.Errorf("guest memory is not mapped")
	}
	if addr < v.memGPA || addr-v.memGPA > v.memSize || uint64(size) > v.memSize-(addr-v.memGPA) {
		return nil, fmt.Errorf("slice ipa %#x size %d out of range [%#x,%#x)", addr, size, v.memGPA, v.memGPA+v.memSize)
	}
	off := addr - v.memGPA
	return v.mem.bytes()[off : off+uint64(size)], nil
}

func (v *VM) WriteIPA(addr uint64, data []byte) error {
	if v == nil || v.mem == nil {
		return fmt.Errorf("guest memory is not mapped")
	}
	if addr < v.memGPA || addr-v.memGPA > v.memSize || uint64(len(data)) > v.memSize-(addr-v.memGPA) {
		return fmt.Errorf("write ipa %#x size %d out of range [%#x,%#x)", addr, len(data), v.memGPA, v.memGPA+v.memSize)
	}
	off := addr - v.memGPA
	copy(v.mem.bytes()[off:off+uint64(len(data))], data)
	return nil
}

func (v *VM) SetX(index int, value uint64) error {
	if index < 0 || index > 30 {
		return fmt.Errorf("invalid x register %d", index)
	}
	return v.setRegister(registerX(index), value)
}

func (v *VM) SetPC(value uint64) error {
	return v.setRegister(registerPC, value)
}

func (v *VM) SetPState(value uint64) error {
	return v.setRegister(registerPSTATE, value)
}

func (v *VM) SetSpEl1(value uint64) error {
	return v.setRegister(registerSPEL1, value)
}

func (v *VM) GetPC() (uint64, error) {
	return v.getRegister(registerPC)
}

func (v *VM) setRegister(name registerName, value uint64) error {
	return setVirtualProcessorRegisters(v.part, 0, []registerName{name}, []registerValue{uint64RegisterValue(value)})
}

func (v *VM) getRegister(name registerName) (uint64, error) {
	values := make([]registerValue, 1)
	if err := getVirtualProcessorRegisters(v.part, 0, []registerName{name}, values); err != nil {
		return 0, err
	}
	return values[0].uint64(), nil
}

func (v *VM) Run(exit *Exit) error {
	var raw runVPExitContext
	if err := runVirtualProcessor(v.part, 0, &raw); err != nil {
		return fmt.Errorf("run virtual processor: %w", err)
	}
	*exit = Exit{Reason: raw.ExitReason}
	switch raw.ExitReason {
	case runVPExitReasonUnmappedGPA, runVPExitReasonGPAIntercept:
		mem := raw.memoryAccess()
		mmio, err := v.decodeMMIO(*mem)
		if err != nil {
			return err
		}
		exit.PC = mem.Header.PC
		exit.CPSR = mem.Header.CPSR
		exit.MMIO = mmio
	}
	return nil
}

func (v *VM) RunInterruptible(ctx context.Context, exit *Exit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stopCancel := context.AfterFunc(ctx, func() {
		_ = cancelRunVirtualProcessor(v.part, 0)
	})
	err := v.Run(exit)
	stopCancel()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil && exit.Reason == runVPExitReasonCanceled {
		return ctxErr
	}
	return nil
}

func (v *VM) CancelRun() error {
	if v == nil || v.part == 0 {
		return nil
	}
	return cancelRunVirtualProcessor(v.part, 0)
}

func (v *VM) SetIRQ(line uint32, level bool) error {
	if v == nil || v.part == 0 {
		return fmt.Errorf("vm is nil")
	}
	if err := requestInterrupt(v.part, line+32, level); err != nil {
		return err
	}
	if level {
		_ = cancelRunVirtualProcessor(v.part, 0)
	}
	return nil
}

func (v *VM) SetMSI(addr uint64, data uint32) error {
	if v == nil || v.part == 0 {
		return fmt.Errorf("vm is nil")
	}
	const lpiBase = 8192
	if err := requestInterrupt(v.part, lpiBase+data, true); err != nil {
		return err
	}
	_ = cancelRunVirtualProcessor(v.part, 0)
	return nil
}

func (v *VM) CompleteMMIORead(exit MMIOExit, value uint64) error {
	if exit.Reg == 31 {
		return v.SetPC(exit.NextPC)
	}
	value = loadResult(value, exit.Len, exit.SignExtend, exit.SF)
	if err := v.setRegister(registerX(exit.Reg), value); err != nil {
		return err
	}
	return v.SetPC(exit.NextPC)
}

func (v *VM) CompleteMMIOWrite(exit MMIOExit) error {
	return v.SetPC(exit.NextPC)
}

func (v *VM) decodeMMIO(mem memoryAccessContext) (MMIOExit, error) {
	size := mem.accessSize()
	if size == 0 || size > 8 {
		return MMIOExit{}, fmt.Errorf("unsupported arm64 mmio syndrome %#x", mem.Syndrome)
	}
	nextPC := mem.Header.PC + uint64(mem.Header.InstructionLength)
	if nextPC == mem.Header.PC {
		nextPC += 4
	}
	out := MMIOExit{
		Addr:       mem.GPA,
		Len:        size,
		Write:      mem.isWrite(),
		Reg:        mem.registerIndex(),
		SignExtend: mem.signExtend(),
		SF:         mem.sf(),
		PC:         mem.Header.PC,
		NextPC:     nextPC,
	}
	if out.Write && out.Reg != 31 {
		value, err := v.getRegister(registerX(out.Reg))
		if err != nil {
			return MMIOExit{}, err
		}
		switch size {
		case 1:
			out.Data[0] = byte(value)
		case 2:
			binary.LittleEndian.PutUint16(out.Data[:2], uint16(value))
		case 4:
			binary.LittleEndian.PutUint32(out.Data[:4], uint32(value))
		default:
			binary.LittleEndian.PutUint64(out.Data[:8], value)
		}
	}
	return out, nil
}

func loadResult(value uint64, size uint32, signExtend, sf bool) uint64 {
	switch size {
	case 1:
		if signExtend {
			if sf {
				return uint64(int64(int8(value)))
			}
			return uint64(uint32(int32(int8(value))))
		}
		return uint64(uint8(value))
	case 2:
		if signExtend {
			if sf {
				return uint64(int64(int16(value)))
			}
			return uint64(uint32(int32(int16(value))))
		}
		return uint64(uint16(value))
	case 4:
		if signExtend && sf {
			return uint64(int64(int32(value)))
		}
		return uint64(uint32(value))
	default:
		return value
	}
}
