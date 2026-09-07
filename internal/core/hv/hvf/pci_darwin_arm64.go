//go:build darwin && arm64

package hvf

import (
	"encoding/binary"
	"fmt"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/fdt"
	"github.com/tinyrange/crumblecracker/internal/core/nvme"
)

const (
	hvfPCIVendorRedHat = 0x1b36
	hvfPCINVMeDeviceID = 0x0010
)

type hvfPCIHost struct {
	configBase uint64
	configSize uint64
	mmioBase   uint64
	mmioSize   uint64
	devices    []*hvfPCIDevice
}

type hvfPCIDevice struct {
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
	mmio        interface {
		ReadMMIO(offset uint64, size int) (uint64, error)
		WriteMMIO(offset uint64, size int, value uint64) error
	}
	barProbeValue bool
}

func newHVFPCIHost(devices ...*hvfPCIDevice) *hvfPCIHost {
	return &hvfPCIHost{
		configBase: arm64vm.PCIConfigBase,
		configSize: arm64vm.PCIConfigSize,
		mmioBase:   arm64vm.PCIMMIOBase,
		mmioSize:   arm64vm.PCIMMIOSize,
		devices:    devices,
	}
}

func newHVFNVMePCIDevice(dev uint8, mmioBase uint64, irq uint8, ctrl *nvme.Controller) *hvfPCIDevice {
	if ctrl != nil {
		ctrl.Base = mmioBase
		ctrl.Size = nvme.MMIOSize
		ctrl.IRQ = uint32(irq)
	}
	return &hvfPCIDevice{
		Device:      dev,
		VendorID:    hvfPCIVendorRedHat,
		DeviceID:    hvfPCINVMeDeviceID,
		SubsystemID: hvfPCINVMeDeviceID,
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

func (h *hvfPCIHost) DeviceTreeNode() fdt.Node {
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
		if dev == nil {
			continue
		}
		node.Children = append(node.Children, dev.deviceTreeNode())
	}
	return node
}

func (h *hvfPCIHost) Contains(addr uint64, size int) bool {
	if size <= 0 {
		return false
	}
	end := addr + uint64(size)
	return (addr >= h.configBase && end <= h.configBase+h.configSize) ||
		(addr >= h.mmioBase && end <= h.mmioBase+h.mmioSize)
}

func (h *hvfPCIHost) Read(addr uint64, size int) (uint64, error) {
	if addr >= h.configBase && addr+uint64(size) <= h.configBase+h.configSize {
		return h.readConfig(addr-h.configBase, size), nil
	}
	for _, dev := range h.devices {
		if dev == nil || dev.mmio == nil || dev.MMIOSize == 0 {
			continue
		}
		if addr >= dev.MMIOBAR && addr+uint64(size) <= dev.MMIOBAR+dev.MMIOSize {
			return dev.mmio.ReadMMIO(addr-dev.MMIOBAR, size)
		}
	}
	return 0, fmt.Errorf("unhandled PCI read addr=%#x size=%d", addr, size)
}

func (h *hvfPCIHost) Write(addr uint64, size int, value uint64) error {
	if addr >= h.configBase && addr+uint64(size) <= h.configBase+h.configSize {
		h.writeConfig(addr-h.configBase, size, value)
		return nil
	}
	for _, dev := range h.devices {
		if dev == nil || dev.mmio == nil || dev.MMIOSize == 0 {
			continue
		}
		if addr >= dev.MMIOBAR && addr+uint64(size) <= dev.MMIOBAR+dev.MMIOSize {
			return dev.mmio.WriteMMIO(addr-dev.MMIOBAR, size, value)
		}
	}
	return fmt.Errorf("unhandled PCI write addr=%#x size=%d value=%#x", addr, size, value)
}

func (h *hvfPCIHost) readConfig(offset uint64, size int) uint64 {
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
	return readLittleConfigValue(cfg[reg:], size)
}

func (h *hvfPCIHost) writeConfig(offset uint64, size int, value uint64) {
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

func (h *hvfPCIHost) deviceAt(bus, device, function uint8) *hvfPCIDevice {
	for _, dev := range h.devices {
		if dev != nil && dev.Bus == bus && dev.Device == device && dev.Function == function {
			return dev
		}
	}
	return nil
}

func (h *hvfPCIHost) interruptMap() []uint32 {
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

func (d *hvfPCIDevice) buildConfig(cfg []byte) {
	binary.LittleEndian.PutUint16(cfg[0x00:0x02], d.VendorID)
	binary.LittleEndian.PutUint16(cfg[0x02:0x04], d.DeviceID)
	binary.LittleEndian.PutUint16(cfg[0x04:0x06], d.Command)
	cfg[0x08] = d.Revision
	cfg[0x09] = d.ProgIF
	cfg[0x0a] = d.Subclass
	cfg[0x0b] = d.Class
	cfg[0x0e] = 0
	if d.barProbeValue {
		binary.LittleEndian.PutUint32(cfg[0x10:0x14], uint32(^(d.MMIOSize-1)&0xfffffff0))
	} else {
		binary.LittleEndian.PutUint32(cfg[0x10:0x14], uint32(d.MMIOBAR&0xfffffff0))
	}
	binary.LittleEndian.PutUint16(cfg[0x2c:0x2e], d.VendorID)
	binary.LittleEndian.PutUint16(cfg[0x2e:0x30], d.SubsystemID)
	cfg[0x3c] = d.IRQLine
	cfg[0x3d] = d.IRQPin
}

func (d *hvfPCIDevice) deviceTreeNode() fdt.Node {
	addr := uint32(d.Bus)<<16 | uint32(d.Device)<<11 | uint32(d.Function)<<8
	return fdt.Node{
		Name: fmt.Sprintf("nvme@%x,0", d.Device),
		Properties: map[string]fdt.Property{
			"reg":        {U32: []uint32{addr, 0, 0, 0, 0}},
			"interrupts": {U32: []uint32{uint32(d.IRQPin)}},
		},
	}
}

func (d *hvfPCIDevice) writeConfigByte(offset uint16, value byte) {
	switch offset {
	case 0x04:
		d.Command = (d.Command & 0xff00) | uint16(value)
	case 0x05:
		d.Command = (d.Command & 0x00ff) | uint16(value)<<8
	case 0x10, 0x11, 0x12, 0x13:
		if value == 0xff {
			d.barProbeValue = true
			return
		}
		d.barProbeValue = false
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

func pciRanges(cpuAddr, pciAddr, size uint64) []uint32 {
	return []uint32{
		0x02000000,
		uint32(pciAddr >> 32),
		uint32(pciAddr),
		uint32(cpuAddr >> 32),
		uint32(cpuAddr),
		uint32(size >> 32),
		uint32(size),
	}
}

func readLittleConfigValue(data []byte, size int) uint64 {
	if size <= 0 {
		return 0
	}
	var value uint64
	for i := 0; i < size && i < len(data) && i < 8; i++ {
		value |= uint64(data[i]) << (8 * i)
	}
	return value
}
