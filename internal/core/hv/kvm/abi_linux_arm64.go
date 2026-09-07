//go:build linux && arm64

package kvm

type kvmUserspaceMemoryRegion struct {
	Slot          uint32
	Flags         uint32
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
}

type kvmCreateDeviceArgs struct {
	Type  uint32
	Fd    uint32
	Flags uint32
}

type kvmEnableCapData struct {
	Cap   uint32
	Flags uint32
	Args  [4]uint64
	Pad   [64]uint8
}

type kvmDeviceAttr struct {
	Flags uint32
	Group uint32
	Attr  uint64
	Addr  uint64
}

type kvmIRQLevel struct {
	IRQOrStatus uint32
	Level       uint32
}

type internalErrorSubReason uint32

const (
	internalErrorEmulation            internalErrorSubReason = 1
	internalErrorSimulEx              internalErrorSubReason = 2
	internalErrorDeliveryEv           internalErrorSubReason = 3
	internalErrorUnexpectedExitReason internalErrorSubReason = 4
)

type internalError struct {
	Suberror internalErrorSubReason
	Ndata    uint32
	Data     [16]uint64
}

const syncRegsSizeBytes = 2048

type kvmRunData struct {
	requestInterruptWindow     uint8
	immediateExit              uint8
	padding1                   [6]uint8
	exitReason                 uint32
	readyForInterruptInjection uint8
	ifFlag                     uint8
	flags                      uint16
	cr8                        uint64
	apicBase                   uint64
	anon0                      [256]byte
	kvmValidRegs               uint64
	kvmDirtyRegs               uint64
	s                          struct{ padding [syncRegsSizeBytes]byte }
}

type kvmExitMMIOData struct {
	physAddr uint64
	data     [8]byte
	len      uint32
	isWrite  uint8
}

type kvmExitArmNISVData struct {
	esrISS   uint64
	faultIPA uint64
}

type kvmSystemEvent struct {
	typ   uint32
	ndata uint32
	data  [16]uint64
}

const (
	kvmArmVcpuInitFeatureWords = 7
	kvmArmVcpuFeaturePsci02    = 2
	kvmArmIRQTypeSPI           = 1
)

type kvmVcpuInit struct {
	Target   uint32
	Features [kvmArmVcpuInitFeatureWords]uint32
}

type kvmOneReg struct {
	id   uint64
	addr uint64
}

type kvmRegList struct {
	n uint64
}
