package amd64

const (
	zeroPageSize = 4096

	setupHeaderOffset = 497

	zeroPageExtRamDiskImage = 192
	zeroPageExtRamDiskSize  = 196
	zeroPageExtCmdLinePtr   = 200
	zeroPageACPIRSDPAddr    = 112
	zeroPageE820Entries     = 488
	zeroPageE820Table       = 720

	protocolVersionOffset     = setupHeaderOffset + 21
	typeOfLoaderOffset        = setupHeaderOffset + 31
	loadFlagsOffset           = setupHeaderOffset + 32
	heapEndPtrOffset          = setupHeaderOffset + 51
	setupHeaderBootFlagOffset = setupHeaderOffset + 13
	setupHeaderHeaderOffset   = setupHeaderOffset + 17
	code32StartOffset         = setupHeaderOffset + 35
	ramdiskImageOffset        = setupHeaderOffset + 39
	ramdiskSizeOffset         = setupHeaderOffset + 43
	cmdLinePtrOffset          = setupHeaderOffset + 55
	initrdAddrMaxOffset       = setupHeaderOffset + 59
	kernelAlignmentOffset     = setupHeaderOffset + 63
	relocatableKernelOffset   = setupHeaderOffset + 67
	minAlignmentOffset        = setupHeaderOffset + 68
	xloadflagsOffset          = setupHeaderOffset + 69
	cmdlineSizeOffset         = setupHeaderOffset + 71
	prefAddressOffset         = setupHeaderOffset + 103
	initSizeOffset            = setupHeaderOffset + 111
)
