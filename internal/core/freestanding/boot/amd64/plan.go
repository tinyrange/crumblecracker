package amd64

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	HigherHalfBase = uint64(0xffffffff80000000)

	defaultBootInfoGPA = 0x00010000
	defaultStackTopGPA = 0x00080000
	defaultPagingGPA   = 0x00090000
	pageSize           = 4096
	pageTableSize      = 5 * pageSize
	directMapSize      = 1 << 30

	BootInfoMagic   = uint64(0x3436544f4f424343) // "CCBOOT64"
	BootInfoVersion = uint64(1)
	KernelOSABI     = elf.OSABI(0x80)
	UserOSABI       = elf.OSABI(0x81)
)

// BootOptions controls the small host-to-kernel contract used for direct ELF
// boot. The defaults keep all bootstrap data below 1 MiB and kernel images are
// expected to load above it.
type BootOptions struct {
	MemorySize  uint64
	BootInfoGPA uint64
	StackTopGPA uint64
	PagingGPA   uint64
}

// BootPlan contains the CPU state and reserved memory required by the VMM.
type BootPlan struct {
	EntryGVA          uint64
	BootInfoGPA       uint64
	BootInfoSize      uint64
	StackTopGPA       uint64
	PagingGPA         uint64
	KernelPhysicalMin uint64
	KernelPhysicalEnd uint64
}

// BootInfo is passed to the kernel in RDI. All addresses are guest physical
// addresses unless explicitly named otherwise.
type BootInfo struct {
	Magic              uint64
	Version            uint64
	Size               uint64
	MemorySize         uint64
	HigherHalfBase     uint64
	KernelPhysicalMin  uint64
	KernelPhysicalEnd  uint64
	PageSize           uint64
	PagingPhysicalBase uint64
	PagingSize         uint64
	BootstrapStackTop  uint64
	BootstrapStackSize uint64
}

func PrepareBoot(memory []byte, kernel []byte, opts BootOptions) (*BootPlan, error) {
	if len(memory) == 0 {
		return nil, errors.New("guest memory is empty")
	}
	if len(kernel) == 0 {
		return nil, errors.New("kernel file is empty")
	}
	if opts.MemorySize == 0 {
		opts.MemorySize = uint64(len(memory))
	}
	if opts.MemorySize > uint64(len(memory)) {
		return nil, fmt.Errorf("declared memory %#x exceeds mapped memory %#x", opts.MemorySize, len(memory))
	}
	if opts.BootInfoGPA == 0 {
		opts.BootInfoGPA = defaultBootInfoGPA
	}
	if opts.StackTopGPA == 0 {
		opts.StackTopGPA = defaultStackTopGPA
	}
	if opts.PagingGPA == 0 {
		opts.PagingGPA = defaultPagingGPA
	}
	if opts.PagingGPA%pageSize != 0 {
		return nil, fmt.Errorf("paging GPA %#x is not page-aligned", opts.PagingGPA)
	}

	entry, physicalMin, physicalEnd, err := loadELF(memory, kernel, opts)
	if err != nil {
		return nil, err
	}
	info := BootInfo{
		Magic:              BootInfoMagic,
		Version:            BootInfoVersion,
		Size:               64,
		MemorySize:         opts.MemorySize,
		HigherHalfBase:     HigherHalfBase,
		KernelPhysicalMin:  physicalMin,
		KernelPhysicalEnd:  physicalEnd,
		PageSize:           pageSize,
		PagingPhysicalBase: opts.PagingGPA,
		PagingSize:         pageTableSize,
		BootstrapStackTop:  opts.StackTopGPA,
		BootstrapStackSize: pageSize,
	}
	info.Size = uint64(binary.Size(info))
	encoded := encodeBootInfo(info)
	if rangesOverlap(opts.BootInfoGPA, uint64(len(encoded)), physicalMin, physicalEnd-physicalMin) {
		return nil, fmt.Errorf("boot info at %#x overlaps kernel image", opts.BootInfoGPA)
	}
	if err := writeAt(memory, opts.BootInfoGPA, encoded); err != nil {
		return nil, fmt.Errorf("write boot info: %w", err)
	}

	return &BootPlan{
		EntryGVA:          entry,
		BootInfoGPA:       opts.BootInfoGPA,
		BootInfoSize:      uint64(len(encoded)),
		StackTopGPA:       opts.StackTopGPA,
		PagingGPA:         opts.PagingGPA,
		KernelPhysicalMin: physicalMin,
		KernelPhysicalEnd: physicalEnd,
	}, nil
}

func loadELF(memory []byte, kernel []byte, opts BootOptions) (uint64, uint64, uint64, error) {
	f, err := elf.NewFile(bytes.NewReader(kernel))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse freestanding kernel ELF: %w", err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != elf.EM_X86_64 {
		return 0, 0, 0, fmt.Errorf("unsupported kernel ELF class=%v data=%v machine=%v", f.Class, f.Data, f.Machine)
	}
	if f.Type != elf.ET_EXEC {
		return 0, 0, 0, fmt.Errorf("kernel ELF type is %v, want ET_EXEC", f.Type)
	}
	if f.OSABI != KernelOSABI || f.ABIVersion != 1 {
		return 0, 0, 0, fmt.Errorf("kernel ELF OSABI=%#x version=%d, want OSABI=%#x version=1", f.OSABI, f.ABIVersion, KernelOSABI)
	}

	physicalMin := ^uint64(0)
	var physicalEnd uint64
	entryExecutable := false
	for _, prog := range f.Progs {
		if prog.Type != elf.PT_LOAD || prog.Memsz == 0 {
			continue
		}
		if prog.Filesz > prog.Memsz {
			return 0, 0, 0, fmt.Errorf("kernel segment filesz %#x exceeds memsz %#x", prog.Filesz, prog.Memsz)
		}
		if prog.Vaddr < HigherHalfBase || prog.Vaddr-HigherHalfBase != prog.Paddr {
			return 0, 0, 0, fmt.Errorf("kernel segment vaddr=%#x paddr=%#x does not use higher-half direct mapping %#x", prog.Vaddr, prog.Paddr, HigherHalfBase)
		}
		if prog.Paddr > opts.MemorySize || prog.Memsz > opts.MemorySize-prog.Paddr {
			return 0, 0, 0, fmt.Errorf("kernel segment paddr=%#x memsz=%#x exceeds guest memory %#x", prog.Paddr, prog.Memsz, opts.MemorySize)
		}
		if prog.Paddr > directMapSize || prog.Memsz > directMapSize-prog.Paddr {
			return 0, 0, 0, fmt.Errorf("kernel segment paddr=%#x memsz=%#x exceeds the bootstrap direct map %#x", prog.Paddr, prog.Memsz, directMapSize)
		}
		if rangesOverlap(prog.Paddr, prog.Memsz, opts.PagingGPA, pageTableSize) {
			return 0, 0, 0, fmt.Errorf("kernel segment paddr=%#x memsz=%#x overlaps bootstrap page tables", prog.Paddr, prog.Memsz)
		}
		segment := memory[prog.Paddr : prog.Paddr+prog.Memsz]
		clear(segment)
		if prog.Filesz != 0 {
			if _, err := io.ReadFull(prog.Open(), segment[:prog.Filesz]); err != nil {
				return 0, 0, 0, fmt.Errorf("read kernel segment at %#x: %w", prog.Paddr, err)
			}
		}
		if prog.Paddr < physicalMin {
			physicalMin = prog.Paddr
		}
		if end := alignUp(prog.Paddr+prog.Memsz, pageSize); end > physicalEnd {
			physicalEnd = end
		}
		if prog.Flags&elf.PF_X != 0 && f.Entry >= prog.Vaddr && f.Entry < prog.Vaddr+prog.Memsz {
			entryExecutable = true
		}
	}
	if physicalEnd == 0 {
		return 0, 0, 0, errors.New("kernel ELF has no loadable segments")
	}
	if !entryExecutable {
		return 0, 0, 0, fmt.Errorf("kernel entry %#x is not in an executable load segment", f.Entry)
	}
	return f.Entry, physicalMin, physicalEnd, nil
}

func encodeBootInfo(info BootInfo) []byte {
	values := [...]uint64{
		info.Magic,
		info.Version,
		info.Size,
		info.MemorySize,
		info.HigherHalfBase,
		info.KernelPhysicalMin,
		info.KernelPhysicalEnd,
		info.PageSize,
		info.PagingPhysicalBase,
		info.PagingSize,
		info.BootstrapStackTop,
		info.BootstrapStackSize,
	}
	out := make([]byte, len(values)*8)
	for index, value := range values {
		binary.LittleEndian.PutUint64(out[index*8:], value)
	}
	return out
}

func writeAt(memory []byte, address uint64, data []byte) error {
	if address > uint64(len(memory)) || uint64(len(data)) > uint64(len(memory))-address {
		return fmt.Errorf("range %#x..%#x exceeds guest memory %#x", address, address+uint64(len(data)), len(memory))
	}
	copy(memory[address:], data)
	return nil
}

func rangesOverlap(a, aSize, b, bSize uint64) bool {
	return a < b+bSize && b < a+aSize
}

func alignUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}
