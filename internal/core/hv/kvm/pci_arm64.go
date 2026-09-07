//go:build linux && arm64

package kvm

import (
	"encoding/binary"
	"fmt"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/fdt"
	"github.com/tinyrange/crumblecracker/internal/core/nvme"
)

const (
	pciVendorRedHat = 0x1b36
	pciNVMeDeviceID = 0x0010
)

type pciMMIOHandler interface {
	ReadMMIO(offset uint64, size int) (uint64, error)
	WriteMMIO(offset uint64, size int, value uint64) error
}

type Arm64PCIHost struct {
	configBase uint64
	configSize uint64
	mmioBase   uint64
	mmioSize   uint64
	devices    []*Arm64PCIDevice
}

type Arm64PCIDevice struct {
	Bus         uint8
	Device      uint8
	Function    uint8
	VendorID    uint16
	DeviceID    uint16
	SubsystemID uint16
	Class       uint8
	Subclass    uint8
	ProgIF      uint8
	Revision    uint8
	IRQLine     uint8
	IRQPin      uint8
	MMIOBAR     uint64
	MMIOSize    uint64
	Command     uint16
	mmio        pciMMIOHandler
	barProbe    bool
}

func NewArm64PCIHost(devices ...*Arm64PCIDevice) *Arm64PCIHost {
	return &Arm64PCIHost{
		configBase: arm64vm.PCIConfigBase,
		configSize: arm64vm.PCIConfigSize,
		mmioBase:   arm64vm.PCIMMIOBase,
		mmioSize:   arm64vm.PCIMMIOSize,
		devices:    devices,
	}
}

func NewArm64NVMePCIDevice(dev uint8, mmioBase uint64, irq uint8, ctrl *nvme.Controller) *Arm64PCIDevice {
	if ctrl != nil {
		ctrl.Base = mmioBase
		ctrl.Size = nvme.MMIOSize
		ctrl.IRQ = uint32(irq)
	}
	return &Arm64PCIDevice{
		Device:      dev,
		VendorID:    pciVendorRedHat,
		DeviceID:    pciNVMeDeviceID,
		SubsystemID: pciNVMeDeviceID,
		Class:       0x01,
		Subclass:    0x08,
		ProgIF:      0x02,
		IRQLine:     irq,
		IRQPin:      1,
		MMIOBAR:     mmioBase,
		MMIOSize:    nvme.MMIOSize,
		mmio:        ctrl,
	}
}

func (h *Arm64PCIHost) DeviceTreeNode() fdt.Node {
	node := fdt.Node{
		Name: fmt.Sprintf("pcie@%x", h.configBase),
		Properties: map[string]fdt.Property{
			"compatible":         {Strings: []string{"pci-host-ecam-generic"}},
			"device_type":        {Strings: []string{"pci"}},
			"#address-cells":     {U32: []uint32{3}},
			"#size-cells":        {U32: []uint32{2}},
			"#interrupt-cells":   {U32: []uint32{1}},
			"bus-range":          {U32: []uint32{0, 0}},
			"dma-coherent":       {Flag: true},
			"linux,pci-domain":   {U32: []uint32{0}},
			"reg":                {U64: []uint64{h.configBase, h.configSize}},
			"ranges":             {U32: pciRanges(h.mmioBase, h.mmioBase, h.mmioSize)},
			"interrupt-map-mask": {U32: []uint32{0x0000f800, 0, 0, 7}},
			"interrupt-map":      {U32: h.interruptMap()},
			"interrupt-parent":   {U32: []uint32{1}},
			"status":             {Strings: []string{"okay"}},
		},
	}
	for _, dev := range h.devices {
		if dev != nil {
			node.Children = append(node.Children, dev.deviceTreeNode())
		}
	}
	return node
}

func (h *Arm64PCIHost) Contains(addr uint64, size int) bool {
	if h == nil || size <= 0 {
		return false
	}
	end := addr + uint64(size)
	return (addr >= h.configBase && end <= h.configBase+h.configSize) ||
		(addr >= h.mmioBase && end <= h.mmioBase+h.mmioSize)
}

func (h *Arm64PCIHost) HandleMMIO(vm *VM, mmio MMIOExit) (bool, error) {
	if h == nil || !h.Contains(mmio.Addr, int(mmio.Len)) {
		return false, nil
	}
	if mmio.Addr >= h.configBase && mmio.Addr+uint64(mmio.Len) <= h.configBase+h.configSize {
		offset := mmio.Addr - h.configBase
		if mmio.Write {
			h.writeConfig(offset, int(mmio.Len), mmioValue(mmio))
			return true, nil
		}
		vm.CompleteMMIORead(h.readConfig(offset, int(mmio.Len)), mmio.Len)
		return true, nil
	}
	for _, dev := range h.devices {
		if dev == nil || dev.mmio == nil || dev.MMIOSize == 0 {
			continue
		}
		if mmio.Addr < dev.MMIOBAR || mmio.Addr+uint64(mmio.Len) > dev.MMIOBAR+dev.MMIOSize {
			continue
		}
		offset := mmio.Addr - dev.MMIOBAR
		if mmio.Write {
			return true, dev.mmio.WriteMMIO(offset, int(mmio.Len), mmioValue(mmio))
		}
		value, err := dev.mmio.ReadMMIO(offset, int(mmio.Len))
		if err != nil {
			return true, err
		}
		vm.CompleteMMIORead(value, mmio.Len)
		return true, nil
	}
	return true, fmt.Errorf("unhandled PCI MMIO addr=%#x len=%d write=%v", mmio.Addr, mmio.Len, mmio.Write)
}

func (h *Arm64PCIHost) readConfig(offset uint64, size int) uint64 {
	bus := uint8((offset >> 20) & 0xff)
	device := uint8((offset >> 15) & 0x1f)
	function := uint8((offset >> 12) & 0x07)
	reg := uint16(offset & 0xfff)
	dev := h.deviceAt(bus, device, function)
	cfg := make([]byte, 4096)
	for i := range cfg {
		cfg[i] = 0xff
	}
	if dev != nil {
		clear(cfg)
		dev.buildConfig(cfg)
	}
	return readLittleValue(cfg[reg : reg+uint16(size)])
}

func (h *Arm64PCIHost) writeConfig(offset uint64, size int, value uint64) {
	bus := uint8((offset >> 20) & 0xff)
	device := uint8((offset >> 15) & 0x1f)
	function := uint8((offset >> 12) & 0x07)
	reg := uint16(offset & 0xfff)
	dev := h.deviceAt(bus, device, function)
	if dev == nil {
		return
	}
	for i := 0; i < size; i++ {
		dev.writeConfigByte(uint16(int(reg)+i), byte(value>>(8*i)))
	}
}

func (h *Arm64PCIHost) deviceAt(bus, device, function uint8) *Arm64PCIDevice {
	for _, dev := range h.devices {
		if dev != nil && dev.Bus == bus && dev.Device == device && dev.Function == function {
			return dev
		}
	}
	return nil
}

func (h *Arm64PCIHost) interruptMap() []uint32 {
	var out []uint32
	for _, dev := range h.devices {
		if dev == nil || dev.IRQPin == 0 {
			continue
		}
		devAddr := uint32(dev.Bus)<<16 | uint32(dev.Device)<<11 | uint32(dev.Function)<<8
		out = append(out, devAddr, 0, 0, uint32(dev.IRQPin), 1, 0, 0, 0, uint32(dev.IRQLine), 4)
	}
	return out
}

func (d *Arm64PCIDevice) buildConfig(cfg []byte) {
	binary.LittleEndian.PutUint16(cfg[0x00:0x02], d.VendorID)
	binary.LittleEndian.PutUint16(cfg[0x02:0x04], d.DeviceID)
	binary.LittleEndian.PutUint16(cfg[0x04:0x06], d.Command)
	cfg[0x08] = d.Revision
	cfg[0x09] = d.ProgIF
	cfg[0x0a] = d.Subclass
	cfg[0x0b] = d.Class
	if d.barProbe {
		binary.LittleEndian.PutUint32(cfg[0x10:0x14], uint32(^(d.MMIOSize-1)&0xfffffff0))
	} else {
		binary.LittleEndian.PutUint32(cfg[0x10:0x14], uint32(d.MMIOBAR&0xfffffff0))
	}
	binary.LittleEndian.PutUint16(cfg[0x2c:0x2e], d.VendorID)
	binary.LittleEndian.PutUint16(cfg[0x2e:0x30], d.SubsystemID)
	cfg[0x3c] = d.IRQLine
	cfg[0x3d] = d.IRQPin
}

func (d *Arm64PCIDevice) deviceTreeNode() fdt.Node {
	addr := uint32(d.Bus)<<16 | uint32(d.Device)<<11 | uint32(d.Function)<<8
	return fdt.Node{
		Name: fmt.Sprintf("nvme@%x,0", d.Device),
		Properties: map[string]fdt.Property{
			"reg":        {U32: []uint32{addr, 0, 0, 0, 0}},
			"interrupts": {U32: []uint32{uint32(d.IRQPin)}},
		},
	}
}

func (d *Arm64PCIDevice) writeConfigByte(offset uint16, value byte) {
	switch offset {
	case 0x04:
		d.Command = (d.Command & 0xff00) | uint16(value)
	case 0x05:
		d.Command = (d.Command & 0x00ff) | uint16(value)<<8
	case 0x10, 0x11, 0x12, 0x13:
		if value == 0xff {
			d.barProbe = true
			return
		}
		d.barProbe = false
		switch offset {
		case 0x10:
			d.MMIOBAR = (d.MMIOBAR & 0xffffffffffffff00) | uint64(value&0xf0)
		case 0x11:
			d.MMIOBAR = (d.MMIOBAR & 0xffffffffffff00ff) | uint64(value)<<8
		case 0x12:
			d.MMIOBAR = (d.MMIOBAR & 0xffffffffff00ffff) | uint64(value)<<16
		case 0x13:
			d.MMIOBAR = (d.MMIOBAR & 0xffffffff00ffffff) | uint64(value)<<24
		}
	case 0x3c:
		d.IRQLine = value
	}
}

func pciRanges(child, parent, size uint64) []uint32 {
	return []uint32{
		0x02000000, uint32(child >> 32), uint32(child),
		uint32(parent >> 32), uint32(parent),
		uint32(size >> 32), uint32(size),
	}
}

func readLittleValue(data []byte) uint64 {
	var value uint64
	for i, b := range data {
		value |= uint64(b) << (8 * i)
	}
	return value
}
