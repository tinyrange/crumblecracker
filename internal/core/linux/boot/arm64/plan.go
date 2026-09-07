package arm64

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
)

const (
	dtbAlignment    = 0x8
	initrdAlignment = 0x1000
	stackGuardBytes = 0x2000

	pstateModeEL1h    = 0x5
	pstateDF          = 0x200
	pstateAF          = 0x100
	pstateIF          = 0x80
	pstateFF          = 0x40
	DefaultPStateBits = pstateModeEL1h | pstateDF | pstateAF | pstateIF | pstateFF

	DefaultUARTBase     = 0x09000000
	DefaultUARTSize     = 0x1000
	DefaultUARTClockHz  = 1843200
	DefaultUARTRegShift = 0
	DefaultUARTBaudRate = 115200

	defaultGICDistributorBase           = 0x08000000
	defaultGICDistributorSize           = 0x00010000
	defaultGICv2CPUInterfaceBase        = 0x08010000
	defaultGICv2CPUInterfaceSize        = 0x00002000
	defaultGICRedistributorBase         = 0x080a0000
	defaultGICRedistributorSize         = 0x00020000
	defaultGICMaintenanceIntNum         = 9
	gicDefaultPhandle            uint32 = 1
)

type GICVersion int

const (
	GICVersionDefault GICVersion = iota
	GICVersionV2
	GICVersionV3
)

type BootOptions struct {
	MemoryBase  uint64
	MemorySize  uint64
	Cmdline     string
	NumCPUs     int
	GICVersion  GICVersion
	UART        *UARTConfig
	DisableUART bool
	Console     bool
	HyperVTimer bool
	Initrd      []byte
	InitrdGPA   uint64
	ExtraNodes  []fdt.Node
	RecordTime  func(name string, duration time.Duration)
}

type UARTConfig struct {
	Base      uint64
	Size      uint64
	ClockHz   uint32
	RegShift  uint32
	BaudRate  uint32
	Interrupt InterruptSpec
}

type InterruptSpec struct {
	Type  uint32
	Num   uint32
	Flags uint32
}

type BootPlan struct {
	EntryGPA      uint64
	StackTopGPA   uint64
	DeviceTreeGPA uint64
	KernelGPA     uint64
	InitrdGPA     uint64
	InitrdSize    uint64
	KernelBytes   []byte
	DeviceTree    []byte
}

func PrepareBoot(memory []byte, kernelFile []byte, opts BootOptions) (*BootPlan, error) {
	record := func(name string, start time.Time) {
		if opts.RecordTime != nil {
			opts.RecordTime(name, time.Since(start))
		}
	}
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
	if opts.UART == nil && !opts.DisableUART {
		opts.UART = &UARTConfig{
			Base:      DefaultUARTBase,
			Size:      DefaultUARTSize,
			ClockHz:   DefaultUARTClockHz,
			RegShift:  DefaultUARTRegShift,
			BaudRate:  DefaultUARTBaudRate,
			Interrupt: InterruptSpec{Type: 0, Num: 33, Flags: 0x4},
		}
	}

	reader := bytes.NewReader(kernelFile)
	start := time.Now()
	probe, err := ProbeKernelImage(reader, int64(len(kernelFile)))
	record("probe_kernel", start)
	if err != nil {
		return nil, err
	}

	start = time.Now()
	image, err := probe.ExtractImage(reader, int64(len(kernelFile)))
	record("extract_kernel", start)
	if err != nil {
		return nil, err
	}

	base := alignUp(opts.MemoryBase, ImageLoadAlignment)
	loadAddr := base + probe.Header.TextOffset
	if loadAddr < opts.MemoryBase {
		return nil, fmt.Errorf("kernel load address %#x below RAM base %#x", loadAddr, opts.MemoryBase)
	}

	kernelReserveSize := uint64(len(image))
	if probe.Header.ImageSize > kernelReserveSize {
		kernelReserveSize = probe.Header.ImageSize
	}
	loadOff := loadAddr - opts.MemoryBase
	if loadOff+kernelReserveSize > uint64(len(memory)) {
		return nil, fmt.Errorf("kernel does not fit in guest RAM")
	}
	start = time.Now()
	copy(memory[loadOff:loadOff+uint64(len(image))], image)
	record("copy_kernel", start)

	initrdAddr := opts.InitrdGPA
	if len(opts.Initrd) > 0 {
		if initrdAddr == 0 {
			initrdAddr = alignUp(loadAddr+kernelReserveSize, initrdAlignment)
		}
		initrdOff := initrdAddr - opts.MemoryBase
		if initrdAddr < opts.MemoryBase || initrdOff+uint64(len(opts.Initrd)) > uint64(len(memory)) {
			return nil, fmt.Errorf("initrd does not fit in guest RAM")
		}
		start = time.Now()
		copy(memory[initrdOff:initrdOff+uint64(len(opts.Initrd))], opts.Initrd)
		record("copy_initrd", start)
	}

	start = time.Now()
	dtb, err := buildDeviceTree(deviceTreeConfig{
		MemoryBase:  opts.MemoryBase,
		MemorySize:  opts.MemorySize,
		NumCPUs:     opts.NumCPUs,
		Cmdline:     opts.Cmdline,
		GICVersion:  opts.GICVersion,
		UART:        opts.UART,
		Console:     opts.Console,
		HyperVTimer: opts.HyperVTimer,
		InitrdGPA:   initrdAddr,
		InitrdSize:  uint64(len(opts.Initrd)),
		ExtraNodes:  append([]fdt.Node(nil), opts.ExtraNodes...),
	})
	record("build_device_tree", start)
	if err != nil {
		return nil, err
	}

	dtbStart := loadAddr + kernelReserveSize
	if len(opts.Initrd) > 0 {
		dtbStart = initrdAddr + uint64(len(opts.Initrd))
	}
	dtbAddr := alignUp(dtbStart, dtbAlignment)
	dtbOff := dtbAddr - opts.MemoryBase
	if dtbOff+uint64(len(dtb)) > uint64(len(memory)) {
		return nil, fmt.Errorf("device tree does not fit in guest RAM")
	}
	start = time.Now()
	copy(memory[dtbOff:dtbOff+uint64(len(dtb))], dtb)
	record("copy_device_tree", start)

	stackTop := alignDown(opts.MemoryBase+opts.MemorySize, 16)
	if stackTop <= dtbAddr+uint64(len(dtb))+stackGuardBytes {
		return nil, fmt.Errorf("stack overlaps device tree")
	}

	start = time.Now()
	entry, err := probe.Header.EntryPoint(base)
	record("entry_point", start)
	if err != nil {
		return nil, err
	}

	return &BootPlan{
		EntryGPA:      entry,
		StackTopGPA:   stackTop,
		DeviceTreeGPA: dtbAddr,
		KernelGPA:     loadAddr,
		InitrdGPA:     initrdAddr,
		InitrdSize:    uint64(len(opts.Initrd)),
		KernelBytes:   image,
		DeviceTree:    dtb,
	}, nil
}

type deviceTreeConfig struct {
	MemoryBase  uint64
	MemorySize  uint64
	NumCPUs     int
	Cmdline     string
	GICVersion  GICVersion
	UART        *UARTConfig
	Console     bool
	HyperVTimer bool
	InitrdGPA   uint64
	InitrdSize  uint64
	ExtraNodes  []fdt.Node
}

func buildDeviceTree(cfg deviceTreeConfig) ([]byte, error) {
	root := fdt.Node{
		Name: "",
		Properties: map[string]fdt.Property{
			"#address-cells":   {U32: []uint32{2}},
			"#size-cells":      {U32: []uint32{2}},
			"compatible":       {Strings: []string{"ccx3,arm64"}},
			"model":            {Strings: []string{"ccx3-arm64"}},
			"interrupt-parent": {U32: []uint32{gicDefaultPhandle}},
		},
	}

	cpus := fdt.Node{
		Name: "cpus",
		Properties: map[string]fdt.Property{
			"#address-cells": {U32: []uint32{2}},
			"#size-cells":    {U32: []uint32{0}},
		},
	}
	for cpu := 0; cpu < cfg.NumCPUs; cpu++ {
		cpus.Children = append(cpus.Children, fdt.Node{
			Name: fmt.Sprintf("cpu@%d", cpu),
			Properties: map[string]fdt.Property{
				"device_type":   {Strings: []string{"cpu"}},
				"compatible":    {Strings: []string{"arm,armv8"}},
				"reg":           {U64: []uint64{uint64(cpu)}},
				"enable-method": {Strings: []string{"psci"}},
			},
		})
	}
	root.Children = append(root.Children, cpus)

	root.Children = append(root.Children, fdt.Node{
		Name: fmt.Sprintf("memory@%x", cfg.MemoryBase),
		Properties: map[string]fdt.Property{
			"device_type": {Strings: []string{"memory"}},
			"reg":         {U64: []uint64{cfg.MemoryBase, cfg.MemorySize}},
		},
	})

	if cfg.UART != nil {
		serialNode := fdt.Node{
			Name: fmt.Sprintf("serial@%x", cfg.UART.Base),
			Properties: map[string]fdt.Property{
				"compatible":   {Strings: []string{"ns16550a"}},
				"reg":          {U64: []uint64{cfg.UART.Base, cfg.UART.Size}},
				"reg-io-width": {U32: []uint32{1}},
				"status":       {Strings: []string{"okay"}},
			},
		}
		if cfg.UART.ClockHz != 0 {
			serialNode.Properties["clock-frequency"] = fdt.Property{U32: []uint32{cfg.UART.ClockHz}}
		}
		if cfg.UART.RegShift != 0 {
			serialNode.Properties["reg-shift"] = fdt.Property{U32: []uint32{cfg.UART.RegShift}}
		}
		if cfg.UART.Interrupt != (InterruptSpec{}) {
			serialNode.Properties["interrupts"] = fdt.Property{U32: []uint32{
				cfg.UART.Interrupt.Type, cfg.UART.Interrupt.Num, cfg.UART.Interrupt.Flags,
			}}
			serialNode.Properties["interrupt-parent"] = fdt.Property{U32: []uint32{gicDefaultPhandle}}
		}
		root.Children = append(root.Children, serialNode)
		root.Children = append(root.Children, fdt.Node{
			Name: "aliases",
			Properties: map[string]fdt.Property{
				"serial0": {Strings: []string{fmt.Sprintf("/%s", serialNode.Name)}},
			},
		})
	}

	chosen := fdt.Node{
		Name: "chosen",
		Properties: map[string]fdt.Property{
			"bootargs": {Strings: []string{cfg.Cmdline}},
		},
	}
	if cfg.Console && cfg.UART != nil {
		consolePath := fmt.Sprintf("serial0:%dn8", cfg.UART.BaudRate)
		chosen.Properties["stdout-path"] = fdt.Property{Strings: []string{consolePath}}
		chosen.Properties["linux,stdout-path"] = fdt.Property{Strings: []string{consolePath}}
	}
	root.Children = append(root.Children, chosen)

	if cfg.InitrdSize > 0 {
		chosenIndex := -1
		for i, child := range root.Children {
			if child.Name == "chosen" {
				chosenIndex = i
				break
			}
		}
		if chosenIndex == -1 {
			root.Children = append(root.Children, fdt.Node{
				Name:       "chosen",
				Properties: map[string]fdt.Property{},
			})
			chosenIndex = len(root.Children) - 1
		}
		root.Children[chosenIndex].Properties["linux,initrd-start"] = fdt.Property{U64: []uint64{cfg.InitrdGPA}}
		root.Children[chosenIndex].Properties["linux,initrd-end"] = fdt.Property{U64: []uint64{cfg.InitrdGPA + cfg.InitrdSize}}
	}

	root.Children = append(root.Children, fdt.Node{
		Name: "psci",
		Properties: map[string]fdt.Property{
			"compatible": {Strings: []string{"arm,psci-0.2", "arm,psci"}},
			"method":     {Strings: []string{"hvc"}},
		},
	})

	timerProperties := map[string]fdt.Property{
		"compatible": {Strings: []string{"arm,armv8-timer"}},
		"always-on":  {Flag: true},
		"interrupts": {U32: []uint32{1, 13, 4, 1, 14, 4, 1, 11, 4, 1, 10, 4}},
	}
	if cfg.HyperVTimer {
		timerProperties["interrupt-names"] = fdt.Property{Strings: []string{"virt"}}
		timerProperties["interrupts"] = fdt.Property{U32: []uint32{1, 4, 8}}
	}
	root.Children = append(root.Children, fdt.Node{
		Name:       "timer",
		Properties: timerProperties,
	})

	root.Children = append(root.Children, fdt.Node{
		Name: fmt.Sprintf("interrupt-controller@%x", defaultGICDistributorBase),
		Properties: map[string]fdt.Property{
			"#interrupt-cells":     {U32: []uint32{3}},
			"#address-cells":       {U32: []uint32{2}},
			"#size-cells":          {U32: []uint32{2}},
			"interrupt-controller": {Flag: true},
			"phandle":              {U32: []uint32{gicDefaultPhandle}},
			"linux,phandle":        {U32: []uint32{gicDefaultPhandle}},
			"interrupts":           {U32: []uint32{1, defaultGICMaintenanceIntNum, 0xF04}},
		},
	})
	gicProps := root.Children[len(root.Children)-1].Properties
	if cfg.GICVersion == GICVersionV2 {
		gicProps["compatible"] = fdt.Property{Strings: []string{"arm,gic-400"}}
		gicProps["reg"] = fdt.Property{U64: []uint64{
			defaultGICDistributorBase, defaultGICDistributorSize,
			defaultGICv2CPUInterfaceBase, defaultGICv2CPUInterfaceSize,
		}}
	} else {
		redistributorSize := uint64(defaultGICRedistributorSize) * uint64(cfg.NumCPUs)
		gicProps["compatible"] = fdt.Property{Strings: []string{"arm,gic-v3"}}
		gicProps["reg"] = fdt.Property{U64: []uint64{
			defaultGICDistributorBase, defaultGICDistributorSize,
			defaultGICRedistributorBase, redistributorSize,
		}}
	}
	root.Children = append(root.Children, cfg.ExtraNodes...)

	return fdt.Build(root)
}

func alignUp(value, align uint64) uint64 {
	if align == 0 {
		return value
	}
	mask := align - 1
	return (value + mask) &^ mask
}

func alignDown(value, align uint64) uint64 {
	if align == 0 {
		return value
	}
	return value &^ (align - 1)
}
