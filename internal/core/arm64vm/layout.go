package arm64vm

import (
	bootarm64 "github.com/tinyrange/crumblecracker/internal/core/linux/boot/arm64"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

const (
	GICVersionDefault = bootarm64.GICVersionDefault
	GICVersionV2      = bootarm64.GICVersionV2
	GICVersionV3      = bootarm64.GICVersionV3

	MemoryBase        = 0xa0000000
	DefaultMemorySize = 512 << 20

	DefaultPStateBits   = bootarm64.DefaultPStateBits
	DefaultUARTBase     = bootarm64.DefaultUARTBase
	DefaultUARTSize     = bootarm64.DefaultUARTSize
	DefaultUARTRegShift = bootarm64.DefaultUARTRegShift

	GICDistributorMin  = 0x08000000
	GICDistributorSize = 0x00010000
	GICDistributorMax  = GICDistributorMin + GICDistributorSize

	GICRedistributorMin  = 0x080a0000
	GICRedistributorSize = 0x00020000
	GICRedistributorMax  = GICRedistributorMin + GICRedistributorSize

	ConsoleBase = 0x0a100000
	ConsoleSize = 0x1000
	ConsoleIRQ  = 40

	RootFSBase = 0x0a101000
	RootFSSize = 0x1000
	RootFSIRQ  = 41

	VsockBase = 0x0a102000
	VsockSize = 0x1000
	VsockIRQ  = 42

	RNGBase = 0x0a103000
	RNGSize = 0x1000
	RNGIRQ  = 43

	ShareFSBase = 0x0a104000
	ShareFSIRQ  = 44
	FSStride    = 0x1000

	NetBase = 0x0a105000
	NetSize = 0x1000
	NetIRQ  = 45

	SnapshotBase = 0x0a106000
	SnapshotSize = 0x1000

	RTCBase = 0x0a107000
	RTCSize = 0x1000

	BalloonBase = 0x0a108000
	BalloonSize = 0x1000
	BalloonIRQ  = 47

	GPUBase = 0x0a109000
	GPUSize = 0x1000
	GPUIRQ  = 48

	KeyboardBase = 0x0a10a000
	KeyboardSize = 0x1000
	KeyboardIRQ  = 49

	PointerBase = 0x0a10b000
	PointerSize = 0x1000
	PointerIRQ  = 50

	PCIConfigBase = 0x20000000
	PCIConfigSize = 0x01000000
	PCIMMIOBase   = 0x21000000
	PCIMMIOSize   = 0x01000000
	NVMeBase      = PCIMMIOBase
	NVMeIRQ       = 46

	UARTSPI = 33

	RootFSTag   = vmruntime.RootFSTag
	EmulatorTag = vmruntime.EmulatorTag
	GuestCID    = vmruntime.GuestCID
	ControlPort = vmruntime.ControlPort
)
