package amd64

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const stackGuardBytes = 0x1000

type E820Entry struct {
	Addr uint64
	Size uint64
	Type uint32
}

type BootOptions struct {
	MemoryBase  uint64
	MemorySize  uint64
	NumCPUs     int
	Cmdline     string
	LoadAddr    uint64
	Initrd      []byte
	InitrdGPA   uint64
	ZeroPageGPA uint64
	CmdlineGPA  uint64
	StackTopGPA uint64
	PagingBase  uint64
	E820        []E820Entry
}

type BootPlan struct {
	LoadAddr    uint64
	EntryGPA    uint64
	ZeroPageGPA uint64
	StackTopGPA uint64
	PagingBase  uint64
}

func PrepareBoot(memory []byte, kernelFile []byte, opts BootOptions) (*BootPlan, error) {
	if len(memory) == 0 {
		return nil, errors.New("guest memory is empty")
	}
	if len(kernelFile) == 0 {
		return nil, errors.New("kernel file is empty")
	}
	if opts.MemorySize == 0 {
		opts.MemorySize = uint64(len(memory))
	}
	if opts.NumCPUs <= 0 {
		opts.NumCPUs = 1
	}
	if opts.ZeroPageGPA == 0 {
		opts.ZeroPageGPA = 0x00090000
	}
	if opts.CmdlineGPA == 0 {
		opts.CmdlineGPA = opts.ZeroPageGPA + zeroPageSize
	}
	if opts.PagingBase == 0 {
		opts.PagingBase = 0x00020000
	}

	img, err := LoadBzImage(bytesReaderAt(kernelFile), int64(len(kernelFile)))
	if err != nil {
		return nil, err
	}
	memStart := opts.MemoryBase
	memEnd := memStart + opts.MemorySize

	loadAddr := opts.LoadAddr
	if loadAddr == 0 {
		loadAddr = img.DefaultLoadAddress()
	}
	if img.Header.RelocatableKernel != 0 {
		align := uint64(img.Header.KernelAlignment)
		if align == 0 {
			align = 0x200000
		}
		loadAddr = alignUp(loadAddr, align)
	}

	payload := img.Payload()
	clearLen := len(payload)
	if init := int(img.Header.InitSize); init > clearLen {
		clearLen = init
	}
	if err := clearAndWrite(memory, memStart, loadAddr, clearLen, payload); err != nil {
		return nil, fmt.Errorf("load kernel: %w", err)
	}

	var initrdAddr uint64
	if len(opts.Initrd) > 0 {
		if opts.InitrdGPA != 0 {
			initrdAddr = opts.InitrdGPA
		} else {
			top := memEnd
			if max := uint64(img.Header.InitrdAddrMax); max != 0 && top > max+1 {
				top = max + 1
			}
			if top <= memStart+uint64(len(opts.Initrd)) {
				return nil, fmt.Errorf("not enough space to place initrd below %#x", top)
			}
			initrdAddr = alignDown(top-uint64(len(opts.Initrd)), 0x1000)
		}
		if err := writeAt(memory, memStart, initrdAddr, opts.Initrd); err != nil {
			return nil, fmt.Errorf("write initrd: %w", err)
		}
	}

	e820 := opts.E820
	if len(e820) == 0 {
		e820 = DefaultE820Map(memStart, memEnd)
	}
	acpiRSDP, err := installBootACPI(memory, memStart, opts.NumCPUs)
	if err != nil {
		return nil, err
	}
	if err := img.buildZeroPage(memory, memStart, loadAddr, opts.Cmdline, opts.CmdlineGPA, initrdAddr, uint32(len(opts.Initrd)), opts.ZeroPageGPA, acpiRSDP, e820); err != nil {
		return nil, err
	}
	if opts.NumCPUs > 1 {
		if err := writeMPTable(memory, memStart, opts.NumCPUs); err != nil {
			return nil, err
		}
	}

	stack := opts.StackTopGPA
	if stack == 0 {
		top := memEnd
		if initrdAddr != 0 {
			top = initrdAddr
		}
		if top <= memStart+2*stackGuardBytes {
			return nil, fmt.Errorf("not enough space to place stack")
		}
		stack = alignDown(top-stackGuardBytes, 0x10)
	}

	return &BootPlan{
		LoadAddr:    loadAddr,
		EntryGPA:    img.EntryPoint(loadAddr),
		ZeroPageGPA: opts.ZeroPageGPA,
		StackTopGPA: stack,
		PagingBase:  opts.PagingBase,
	}, nil
}

func writeMPTable(memory []byte, memStart uint64, numCPUs int) error {
	const (
		fpGPA       = 0x000f0000
		tableGPA    = 0x000f0100
		lapicAddr   = 0xfee00000
		ioapicAddr  = 0xfec00000
		entryCPU    = 0
		entryBus    = 1
		entryIOAPIC = 2
		entryIOInt  = 3
		entryLInt   = 4
	)
	if numCPUs < 1 {
		numCPUs = 1
	}
	if numCPUs > 255 {
		numCPUs = 255
	}
	entryCount := numCPUs + 1 + 1 + 16 + 2
	tableLen := 44 + numCPUs*20 + 8 + 8 + 16*8 + 2*8
	table := make([]byte, tableLen)
	copy(table[0:4], "PCMP")
	binary.LittleEndian.PutUint16(table[4:6], uint16(tableLen))
	table[6] = 4
	copy(table[8:16], []byte("CC      "))
	copy(table[16:28], []byte("CC SMP      "))
	binary.LittleEndian.PutUint16(table[34:36], uint16(entryCount))
	binary.LittleEndian.PutUint32(table[36:40], lapicAddr)
	off := 44
	cpuFeatureFlags := uint32(0x201)
	if numCPUs > 1 {
		cpuFeatureFlags |= 1 << 28
	}
	for cpu := 0; cpu < numCPUs; cpu++ {
		table[off] = entryCPU
		table[off+1] = byte(cpu)
		table[off+2] = 0x14
		table[off+3] = 1
		if cpu == 0 {
			table[off+3] |= 2
		}
		binary.LittleEndian.PutUint32(table[off+4:off+8], 0x600)
		binary.LittleEndian.PutUint32(table[off+8:off+12], cpuFeatureFlags)
		off += 20
	}
	table[off] = entryBus
	table[off+1] = 0
	copy(table[off+2:off+8], []byte("ISA   "))
	off += 8
	table[off] = entryIOAPIC
	table[off+1] = byte(numCPUs)
	table[off+2] = 0x11
	table[off+3] = 1
	binary.LittleEndian.PutUint32(table[off+4:off+8], ioapicAddr)
	off += 8
	for irq := 0; irq < 16; irq++ {
		table[off] = entryIOInt
		table[off+1] = 0
		binary.LittleEndian.PutUint16(table[off+2:off+4], 0)
		table[off+4] = 0
		table[off+5] = byte(irq)
		table[off+6] = byte(numCPUs)
		table[off+7] = byte(irq)
		off += 8
	}
	for lint := 0; lint < 2; lint++ {
		table[off] = entryLInt
		table[off+1] = byte(lint)
		binary.LittleEndian.PutUint16(table[off+2:off+4], 0)
		table[off+4] = 0
		table[off+5] = 0
		table[off+6] = 0xff
		table[off+7] = byte(lint)
		off += 8
	}
	table[7] = checksum(table)
	if err := writeAt(memory, memStart, tableGPA, table); err != nil {
		return fmt.Errorf("write MP config table: %w", err)
	}

	fp := make([]byte, 16)
	copy(fp[0:4], "_MP_")
	binary.LittleEndian.PutUint32(fp[4:8], tableGPA)
	fp[8] = 1
	fp[9] = 4
	fp[10] = checksum(fp)
	if err := writeAt(memory, memStart, fpGPA, fp); err != nil {
		return fmt.Errorf("write MP floating pointer: %w", err)
	}
	return nil
}

func checksum(buf []byte) byte {
	var sum byte
	for _, b := range buf {
		sum += b
	}
	return -sum
}

func (k *KernelImage) buildZeroPage(memory []byte, memStart, loadAddr uint64, cmdline string, cmdlineGPA, initrdGPA uint64, initrdSize uint32, zeroPageGPA, acpiRSDP uint64, e820 []E820Entry) error {
	zp := make([]byte, zeroPageSize)
	if len(k.HeaderBytes) > zeroPageSize-setupHeaderOffset {
		return errors.New("setup header larger than zero page space")
	}
	copy(zp[setupHeaderOffset:], k.HeaderBytes)
	binary.LittleEndian.PutUint16(zp[setupHeaderBootFlagOffset:], 0xaa55)
	copy(zp[setupHeaderHeaderOffset:], []byte(headerMagic))
	binary.LittleEndian.PutUint16(zp[protocolVersionOffset:], k.Header.ProtocolVersion)
	zp[typeOfLoaderOffset] = 0xff
	zp[loadFlagsOffset] = k.Header.LoadFlags | 0x80
	binary.LittleEndian.PutUint16(zp[heapEndPtrOffset:], 0xe000-0x200)
	binary.LittleEndian.PutUint32(zp[code32StartOffset:], uint32(loadAddr))
	binary.LittleEndian.PutUint32(zp[cmdLinePtrOffset:], uint32(cmdlineGPA))
	binary.LittleEndian.PutUint32(zp[zeroPageExtCmdLinePtr:], uint32(cmdlineGPA>>32))
	binary.LittleEndian.PutUint64(zp[zeroPageACPIRSDPAddr:], acpiRSDP)
	binary.LittleEndian.PutUint32(zp[initrdAddrMaxOffset:], k.Header.InitrdAddrMax)
	binary.LittleEndian.PutUint32(zp[kernelAlignmentOffset:], k.Header.KernelAlignment)
	zp[relocatableKernelOffset] = k.Header.RelocatableKernel
	zp[minAlignmentOffset] = k.Header.MinAlignment
	binary.LittleEndian.PutUint16(zp[xloadflagsOffset:], k.Header.XLoadFlags)
	binary.LittleEndian.PutUint32(zp[cmdlineSizeOffset:], k.Header.CmdlineSize)
	binary.LittleEndian.PutUint64(zp[prefAddressOffset:], k.Header.PrefAddress)
	binary.LittleEndian.PutUint32(zp[initSizeOffset:], k.Header.InitSize)
	if initrdSize > 0 {
		binary.LittleEndian.PutUint32(zp[ramdiskImageOffset:], uint32(initrdGPA))
		binary.LittleEndian.PutUint32(zp[ramdiskSizeOffset:], initrdSize)
		binary.LittleEndian.PutUint32(zp[zeroPageExtRamDiskImage:], uint32(initrdGPA>>32))
	}
	if k.Header.CmdlineSize != 0 && len(cmdline) > int(k.Header.CmdlineSize) {
		return fmt.Errorf("command line length %d exceeds kernel limit %d", len(cmdline), k.Header.CmdlineSize)
	}
	if err := writeAt(memory, memStart, cmdlineGPA, append([]byte(strings.TrimRight(cmdline, "\x00")), 0)); err != nil {
		return fmt.Errorf("write command line: %w", err)
	}
	if len(e820) == 0 {
		return errors.New("e820 map must contain at least one entry")
	}
	if len(e820) > 128 {
		return fmt.Errorf("too many e820 entries: %d", len(e820))
	}
	zp[zeroPageE820Entries] = byte(len(e820))
	for i, ent := range e820 {
		base := zeroPageE820Table + i*20
		binary.LittleEndian.PutUint64(zp[base:], ent.Addr)
		binary.LittleEndian.PutUint64(zp[base+8:], ent.Size)
		binary.LittleEndian.PutUint32(zp[base+16:], ent.Type)
	}
	return writeAt(memory, memStart, zeroPageGPA, zp)
}

func DefaultE820Map(memStart, memEnd uint64) []E820Entry {
	if memEnd <= memStart {
		return nil
	}
	entries := []E820Entry{}
	lowEnd := min(memEnd, 0x0009f000)
	if lowEnd > memStart {
		entries = append(entries, E820Entry{Addr: memStart, Size: lowEnd - memStart, Type: 1})
	}
	if memEnd > 0x0009f000 {
		resEnd := min(memEnd, 0x00100000)
		if resEnd > 0x0009f000 {
			entries = append(entries, E820Entry{Addr: 0x0009f000, Size: resEnd - 0x0009f000, Type: 2})
		}
	}
	if memEnd > 0x00100000 {
		entries = append(entries, E820Entry{Addr: 0x00100000, Size: memEnd - 0x00100000, Type: 1})
	}
	return entries
}

func clearAndWrite(memory []byte, memStart, gpa uint64, clearLen int, data []byte) error {
	if clearLen > 0 {
		if err := writeAt(memory, memStart, gpa, make([]byte, clearLen)); err != nil {
			return err
		}
	}
	return writeAt(memory, memStart, gpa, data)
}

func writeAt(memory []byte, memStart, gpa uint64, data []byte) error {
	if gpa < memStart {
		return fmt.Errorf("GPA %#x below memory base %#x", gpa, memStart)
	}
	off := gpa - memStart
	if off+uint64(len(data)) > uint64(len(memory)) {
		return fmt.Errorf("range [%#x,%#x) outside guest memory", gpa, gpa+uint64(len(data)))
	}
	copy(memory[off:off+uint64(len(data))], data)
	return nil
}

func alignUp(value, align uint64) uint64 {
	if align == 0 {
		return value
	}
	return (value + align - 1) &^ (align - 1)
}

func alignDown(value, align uint64) uint64 {
	if align == 0 {
		return value
	}
	return value &^ (align - 1)
}

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= int64(len(b)) {
		return 0, errors.New("offset beyond buffer")
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, errors.New("short read")
	}
	return n, nil
}
