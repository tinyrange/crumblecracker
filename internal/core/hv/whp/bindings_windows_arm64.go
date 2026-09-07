//go:build windows && arm64

package whp

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winhvplatform = syscall.NewLazyDLL("winhvplatform.dll")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")

	procWHvGetCapability                = winhvplatform.NewProc("WHvGetCapability")
	procWHvCreatePartition              = winhvplatform.NewProc("WHvCreatePartition")
	procWHvSetupPartition               = winhvplatform.NewProc("WHvSetupPartition")
	procWHvDeletePartition              = winhvplatform.NewProc("WHvDeletePartition")
	procWHvSetPartitionProperty         = winhvplatform.NewProc("WHvSetPartitionProperty")
	procWHvMapGpaRange                  = winhvplatform.NewProc("WHvMapGpaRange")
	procWHvUnmapGpaRange                = winhvplatform.NewProc("WHvUnmapGpaRange")
	procWHvCreateVirtualProcessor       = winhvplatform.NewProc("WHvCreateVirtualProcessor")
	procWHvDeleteVirtualProcessor       = winhvplatform.NewProc("WHvDeleteVirtualProcessor")
	procWHvRunVirtualProcessor          = winhvplatform.NewProc("WHvRunVirtualProcessor")
	procWHvGetVirtualProcessorRegisters = winhvplatform.NewProc("WHvGetVirtualProcessorRegisters")
	procWHvSetVirtualProcessorRegisters = winhvplatform.NewProc("WHvSetVirtualProcessorRegisters")
	procWHvGetVirtualProcessorState     = winhvplatform.NewProc("WHvGetVirtualProcessorState")
	procWHvSetVirtualProcessorState     = winhvplatform.NewProc("WHvSetVirtualProcessorState")
	procWHvCancelRunVirtualProcessor    = winhvplatform.NewProc("WHvCancelRunVirtualProcessor")
	procWHvRequestInterrupt             = winhvplatform.NewProc("WHvRequestInterrupt")
	procWHvAdviseGpaRange               = winhvplatform.NewProc("WHvAdviseGpaRange")
	procVirtualAlloc                    = kernel32.NewProc("VirtualAlloc")
	procVirtualFree                     = kernel32.NewProc("VirtualFree")
)

type hresult int32

func (hr hresult) Err() error {
	if hr >= 0 {
		return nil
	}
	return fmt.Errorf("HRESULT 0x%08x: %s", uint32(hr), syscall.Errno(hr))
}

func callHRESULT(proc *syscall.LazyProc, args ...uintptr) error {
	r1, _, callErr := proc.Call(args...)
	if callErr != syscall.Errno(0) && r1 == 0 {
		return callErr
	}
	return hresult(int32(r1)).Err()
}

type partitionHandle syscall.Handle
type guestPhysicalAddress uint64

type capabilityCode uint32

const (
	capabilityCodeHypervisorPresent capabilityCode = 0
	capabilityCodeFeatures          capabilityCode = 0x00000001
	capabilityCodeGicLpiIntIDBits   capabilityCode = 0x00002011
)

type capabilityFeatures struct {
	AsUint64 uint64
}

func (f capabilityFeatures) arm64Support() bool {
	return f.AsUint64&(1<<11) != 0
}

type partitionPropertyCode uint32

const (
	partitionPropertyCodeArm64ICParameters partitionPropertyCode = 0x00001012
	partitionPropertyCodeProcessorCount    partitionPropertyCode = 0x00001fff
)

type arm64ICEmulationMode uint32

const arm64ICEmulationModeGICV3 arm64ICEmulationMode = 1

type arm64ICGICV3Parameters struct {
	GICDBaseAddress           guestPhysicalAddress
	GITSTranslaterBaseAddress guestPhysicalAddress
	Reserved                  uint32
	GICLPIIntIDBits           uint32
	GICPPIOverflowFromCNTV    uint32
	GICPPIPerformanceMonitors uint32
	Reserved1                 [6]uint32
}

type arm64ICParameters struct {
	EmulationMode   arm64ICEmulationMode
	Reserved        uint32
	GICV3Parameters arm64ICGICV3Parameters
}

type mapGPARangeFlags uint32

const (
	mapGPARangeFlagRead    mapGPARangeFlags = 0x1
	mapGPARangeFlagWrite   mapGPARangeFlags = 0x2
	mapGPARangeFlagExecute mapGPARangeFlags = 0x4
)

type memoryRangeEntry struct {
	GuestAddress guestPhysicalAddress
	SizeInBytes  uint64
}

type adviseGPARangeCode uint32

const (
	adviseGPARangeCodePopulate adviseGPARangeCode = 0
)

type memoryAccessType uint32

const (
	memoryAccessRead    memoryAccessType = 0
	memoryAccessWrite   memoryAccessType = 1
	memoryAccessExecute memoryAccessType = 2
)

type adviseGPARangePopulateFlags uint32

const (
	adviseGPARangePopulateFlagPrefetch        adviseGPARangePopulateFlags = 0x1
	adviseGPARangePopulateFlagAvoidHardFaults adviseGPARangePopulateFlags = 0x2
)

type adviseGPARangePopulateOptions struct {
	Flags      adviseGPARangePopulateFlags
	AccessType memoryAccessType
}

type registerName uint32
type virtualProcessorStateType uint32

const (
	virtualProcessorStateTypeArm64InterruptControllerState virtualProcessorStateType = 0x80000000
	virtualProcessorStateTypeArm64GlobalInterruptState     virtualProcessorStateType = 0xc0000006
)

const (
	registerX0          registerName = 0x00020000
	registerX1          registerName = 0x00020001
	registerX2          registerName = 0x00020002
	registerX3          registerName = 0x00020003
	registerX28         registerName = 0x0002001c
	registerFP          registerName = 0x0002001d
	registerLR          registerName = 0x0002001e
	registerSP          registerName = 0x0002001f
	registerSPEL0       registerName = 0x00020020
	registerSPEL1       registerName = 0x00020021
	registerPC          registerName = 0x00020022
	registerPSTATE      registerName = 0x00020023
	registerCurrentEL   registerName = 0x00021003
	registerDAIF        registerName = 0x00021004
	registerDIT         registerName = 0x00021005
	registerNZCV        registerName = 0x00021006
	registerPAN         registerName = 0x00021007
	registerSPSEL       registerName = 0x00021008
	registerSSBS        registerName = 0x00021009
	registerTCO         registerName = 0x0002100a
	registerUAO         registerName = 0x0002100b
	registerQ0          registerName = 0x00030000
	registerELREL1      registerName = 0x00040015
	registerFPCR        registerName = 0x00040012
	registerFPSR        registerName = 0x00040013
	registerSPSREL1     registerName = 0x00040014
	registerACTLREL1    registerName = 0x00040003
	registerCPACREL1    registerName = 0x00040004
	registerCSSELREL1   registerName = 0x00040035
	registerESREL1      registerName = 0x00040008
	registerFAREL1      registerName = 0x00040009
	registerMAIREL1     registerName = 0x0004000b
	registerPAREL1      registerName = 0x0004000a
	registerSCTLREL1    registerName = 0x00040002
	registerTCREL1      registerName = 0x00040007
	registerTPIDREL0    registerName = 0x00040011
	registerTPIDREL1    registerName = 0x0004000e
	registerTPIDRROEL0  registerName = 0x00040010
	registerTTBR0EL1    registerName = 0x00040005
	registerTTBR1EL1    registerName = 0x00040006
	registerVBAREL1     registerName = 0x0004000c
	registerCNTKCTLEL1  registerName = 0x00058008
	registerCNTVCTLEL0  registerName = 0x0005800e
	registerCNTVCVALEL0 registerName = 0x0005800f
	registerCNTVCTEL0   registerName = 0x00058011
	registerGICR        registerName = 0x00063000

	registerInternalActivityState      registerName = 0x00000004
	registerPendingEvent0              registerName = 0x00010004
	registerPendingEvent1              registerName = 0x00010005
	registerDeliverabilityNotification registerName = 0x00010006
	registerPendingEvent2              registerName = 0x00010008
	registerPendingEvent3              registerName = 0x00010009
)

func registerX(index int) registerName {
	if index >= 0 && index <= 28 {
		return registerName(uint32(registerX0) + uint32(index))
	}
	if index == 29 {
		return registerFP
	}
	return registerLR
}

type uint128 struct {
	Low64  uint64
	High64 uint64
}

type registerValue struct {
	raw uint128
}

func uint64RegisterValue(v uint64) registerValue {
	var out registerValue
	*(*uint64)(unsafe.Pointer(&out)) = v
	return out
}

func (v *registerValue) uint64() uint64 {
	return *(*uint64)(unsafe.Pointer(v))
}

type runVPExitReason uint32

const (
	runVPExitReasonUnmappedGPA            runVPExitReason = 0x80000000
	runVPExitReasonGPAIntercept           runVPExitReason = 0x80000001
	runVPExitReasonUnrecoverableException runVPExitReason = 0x80000021
	runVPExitReasonInvalidVPRegisterValue runVPExitReason = 0x80000020
	runVPExitReasonUnsupportedFeature     runVPExitReason = 0x80000022
	runVPExitReasonRegisterIntercept      runVPExitReason = 0x80010006
	runVPExitReasonArm64Reset             runVPExitReason = 0x8001000c
	runVPExitReasonCanceled               runVPExitReason = 0xffffffff
)

func (r runVPExitReason) String() string {
	switch r {
	case runVPExitReasonUnmappedGPA:
		return "UnmappedGpa"
	case runVPExitReasonGPAIntercept:
		return "GpaIntercept"
	case runVPExitReasonUnrecoverableException:
		return "UnrecoverableException"
	case runVPExitReasonInvalidVPRegisterValue:
		return "InvalidVpRegisterValue"
	case runVPExitReasonUnsupportedFeature:
		return "UnsupportedFeature"
	case runVPExitReasonRegisterIntercept:
		return "RegisterIntercept"
	case runVPExitReasonArm64Reset:
		return "Arm64Reset"
	case runVPExitReasonCanceled:
		return "Canceled"
	default:
		return fmt.Sprintf("Unknown(%#x)", uint32(r))
	}
}

type vpExecutionState struct {
	AsUint16 uint16
}

type interceptMessageHeader struct {
	VPIndex             uint32
	InstructionLength   uint8
	InterceptAccessType uint8
	ExecutionState      vpExecutionState
	PC                  uint64
	CPSR                uint64
}

type memoryAccessInfo struct {
	AsUint8 uint8
}

type memoryAccessContext struct {
	Header               interceptMessageHeader
	Reserved0            uint32
	InstructionByteCount uint8
	AccessInfo           memoryAccessInfo
	Reserved1            uint16
	InstructionBytes     [4]uint8
	Reserved2            uint32
	GVA                  uint64
	GPA                  uint64
	Syndrome             uint64
}

func (m memoryAccessContext) isWrite() bool {
	return m.Syndrome&(1<<6) != 0
}

func (m memoryAccessContext) accessSize() uint32 {
	if m.Syndrome&(1<<24) == 0 {
		return 0
	}
	return 1 << ((m.Syndrome >> 22) & 0x3)
}

func (m memoryAccessContext) registerIndex() int {
	return int((m.Syndrome >> 16) & 0x1f)
}

func (m memoryAccessContext) signExtend() bool {
	return m.Syndrome&(1<<21) != 0
}

func (m memoryAccessContext) sf() bool {
	return m.Syndrome&(1<<15) != 0
}

type runVPExitContext struct {
	ExitReason runVPExitReason
	Reserved   uint32
	Reserved1  uint64
	Payload    [256]byte
}

func (c *runVPExitContext) memoryAccess() *memoryAccessContext {
	return (*memoryAccessContext)(unsafe.Pointer(&c.Payload[0]))
}

type allocation struct {
	addr    uintptr
	size    uintptr
	mapping windows.Handle
}

func virtualAlloc(size uintptr) (*allocation, error) {
	const (
		memCommit            = 0x1000
		memReserve           = 0x2000
		pageExecuteReadWrite = 0x40
	)
	ptr, _, err := procVirtualAlloc.Call(0, size, memCommit|memReserve, pageExecuteReadWrite)
	if ptr == 0 {
		if err == syscall.Errno(0) {
			err = syscall.GetLastError()
		}
		return nil, err
	}
	return &allocation{addr: ptr, size: size}, nil
}

func virtualMapFile(path string, size uintptr) (*allocation, error) {
	if size == 0 {
		return nil, fmt.Errorf("memory mapping size must be non-zero")
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	file, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_EXECUTE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(file)

	mapping, err := windows.CreateFileMapping(file, nil, windows.PAGE_EXECUTE_WRITECOPY, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_COPY|windows.FILE_MAP_EXECUTE, 0, 0, size)
	if err != nil {
		_ = windows.CloseHandle(mapping)
		return nil, err
	}
	return &allocation{addr: addr, size: size, mapping: mapping}, nil
}

func (a *allocation) bytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(a.addr)), int(a.size))
}

func (a *allocation) free() error {
	if a == nil || a.addr == 0 {
		return nil
	}
	if a.mapping != 0 {
		var first error
		if err := windows.UnmapViewOfFile(a.addr); err != nil && first == nil {
			first = err
		}
		if err := windows.CloseHandle(a.mapping); err != nil && first == nil {
			first = err
		}
		a.addr = 0
		a.size = 0
		a.mapping = 0
		return first
	}
	const memRelease = 0x8000
	r1, _, err := procVirtualFree.Call(a.addr, 0, memRelease)
	if r1 == 0 {
		if err == syscall.Errno(0) {
			err = syscall.GetLastError()
		}
		return err
	}
	a.addr = 0
	a.size = 0
	return nil
}

func isHypervisorPresent() (bool, error) {
	var present uint32
	var written uint32
	err := callHRESULT(procWHvGetCapability, uintptr(capabilityCodeHypervisorPresent), uintptr(unsafe.Pointer(&present)), unsafe.Sizeof(present), uintptr(unsafe.Pointer(&written)))
	if err != nil {
		return false, err
	}
	return present != 0, nil
}

func getCapability[T any](code capabilityCode) (T, error) {
	var value T
	var written uint32
	err := callHRESULT(procWHvGetCapability, uintptr(code), uintptr(unsafe.Pointer(&value)), unsafe.Sizeof(value), uintptr(unsafe.Pointer(&written)))
	return value, err
}

func createPartition() (partitionHandle, error) {
	var part partitionHandle
	err := callHRESULT(procWHvCreatePartition, uintptr(unsafe.Pointer(&part)))
	return part, err
}

func setPartitionProperty[T any](part partitionHandle, code partitionPropertyCode, value T) error {
	return callHRESULT(procWHvSetPartitionProperty, uintptr(part), uintptr(code), uintptr(unsafe.Pointer(&value)), unsafe.Sizeof(value))
}

func setupPartition(part partitionHandle) error {
	return callHRESULT(procWHvSetupPartition, uintptr(part))
}

func deletePartition(part partitionHandle) error {
	return callHRESULT(procWHvDeletePartition, uintptr(part))
}

func mapGPARange(part partitionHandle, source unsafe.Pointer, guestAddress uint64, size uint64, flags mapGPARangeFlags) error {
	return callHRESULT(procWHvMapGpaRange, uintptr(part), uintptr(source), uintptr(guestPhysicalAddress(guestAddress)), uintptr(size), uintptr(flags))
}

func unmapGPARange(part partitionHandle, guestAddress uint64, size uint64) error {
	return callHRESULT(procWHvUnmapGpaRange, uintptr(part), uintptr(guestPhysicalAddress(guestAddress)), uintptr(size))
}

func adviseGPARangePopulate(part partitionHandle, guestAddress uint64, size uint64, access memoryAccessType, flags adviseGPARangePopulateFlags) error {
	ranges := []memoryRangeEntry{{
		GuestAddress: guestPhysicalAddress(guestAddress),
		SizeInBytes:  size,
	}}
	populate := adviseGPARangePopulateOptions{
		Flags:      flags,
		AccessType: access,
	}
	return callHRESULT(
		procWHvAdviseGpaRange,
		uintptr(part),
		uintptr(unsafe.Pointer(&ranges[0])),
		uintptr(len(ranges)),
		uintptr(adviseGPARangeCodePopulate),
		uintptr(unsafe.Pointer(&populate)),
		unsafe.Sizeof(populate),
	)
}

func createVirtualProcessor(part partitionHandle, vpIndex uint32) error {
	return callHRESULT(procWHvCreateVirtualProcessor, uintptr(part), uintptr(vpIndex), 0)
}

func deleteVirtualProcessor(part partitionHandle, vpIndex uint32) error {
	return callHRESULT(procWHvDeleteVirtualProcessor, uintptr(part), uintptr(vpIndex))
}

func runVirtualProcessor(part partitionHandle, vpIndex uint32, exit *runVPExitContext) error {
	return callHRESULT(procWHvRunVirtualProcessor, uintptr(part), uintptr(vpIndex), uintptr(unsafe.Pointer(exit)), unsafe.Sizeof(*exit))
}

func getVirtualProcessorRegisters(part partitionHandle, vpIndex uint32, names []registerName, values []registerValue) error {
	if len(values) < len(names) {
		return fmt.Errorf("register values slice too small")
	}
	if len(names) == 0 {
		return nil
	}
	aligned, backing := alignedRegisterValues(len(names))
	err := callHRESULT(procWHvGetVirtualProcessorRegisters, uintptr(part), uintptr(vpIndex), uintptr(unsafe.Pointer(&names[0])), uintptr(len(names)), uintptr(unsafe.Pointer(&aligned[0])))
	if err == nil {
		copy(values, aligned)
	}
	runtime.KeepAlive(names)
	runtime.KeepAlive(values)
	runtime.KeepAlive(backing)
	return err
}

func setVirtualProcessorRegisters(part partitionHandle, vpIndex uint32, names []registerName, values []registerValue) error {
	if len(values) < len(names) {
		return fmt.Errorf("register values slice too small")
	}
	if len(names) == 0 {
		return nil
	}
	aligned, backing := alignedRegisterValues(len(names))
	copy(aligned, values[:len(names)])
	err := callHRESULT(procWHvSetVirtualProcessorRegisters, uintptr(part), uintptr(vpIndex), uintptr(unsafe.Pointer(&names[0])), uintptr(len(names)), uintptr(unsafe.Pointer(&aligned[0])))
	runtime.KeepAlive(names)
	runtime.KeepAlive(values)
	runtime.KeepAlive(backing)
	return err
}

func getVirtualProcessorState(part partitionHandle, vpIndex uint32, stateType virtualProcessorStateType) ([]byte, error) {
	var written uint32
	probe := []byte{0}
	err := callGetVirtualProcessorState(part, vpIndex, stateType, probe, &written)
	if err == nil {
		return probe[:written], nil
	}
	if written == 0 {
		return nil, err
	}
	buf := make([]byte, written)
	if err := callGetVirtualProcessorState(part, vpIndex, stateType, buf, &written); err != nil {
		return nil, err
	}
	return buf[:written], nil
}

func callGetVirtualProcessorState(part partitionHandle, vpIndex uint32, stateType virtualProcessorStateType, buf []byte, written *uint32) error {
	if len(buf) == 0 {
		return fmt.Errorf("virtual processor state buffer is empty")
	}
	r1, _, callErr := procWHvGetVirtualProcessorState.Call(
		uintptr(part),
		uintptr(vpIndex),
		uintptr(stateType),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(written)),
	)
	if callErr != syscall.Errno(0) && r1 == 0 {
		return callErr
	}
	runtime.KeepAlive(buf)
	runtime.KeepAlive(written)
	return hresult(int32(r1)).Err()
}

func setVirtualProcessorState(part partitionHandle, vpIndex uint32, stateType virtualProcessorStateType, buf []byte) error {
	if len(buf) == 0 {
		return fmt.Errorf("virtual processor state buffer is empty")
	}
	err := callHRESULT(
		procWHvSetVirtualProcessorState,
		uintptr(part),
		uintptr(vpIndex),
		uintptr(stateType),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	runtime.KeepAlive(buf)
	return err
}

func alignedRegisterValues(count int) ([]registerValue, []byte) {
	const registerValueAlign = uintptr(16)
	if count == 0 {
		return nil, nil
	}
	size := unsafe.Sizeof(registerValue{})
	backing := make([]byte, uintptr(count)*size+registerValueAlign-1)
	addr := uintptr(unsafe.Pointer(&backing[0]))
	aligned := (addr + registerValueAlign - 1) &^ (registerValueAlign - 1)
	return unsafe.Slice((*registerValue)(unsafe.Pointer(aligned)), count), backing
}

func cancelRunVirtualProcessor(part partitionHandle, vpIndex uint32) error {
	return callHRESULT(procWHvCancelRunVirtualProcessor, uintptr(part), uintptr(vpIndex), 0)
}

type interruptControl2 struct {
	AsUint64 uint64
}

type interruptControl struct {
	TargetPartition    uint64
	InterruptControl   interruptControl2
	DestinationAddress uint64
	RequestedVector    uint32
	TargetVTL          uint8
	ReservedZ0         uint8
	ReservedZ1         uint16
}

func requestInterrupt(part partitionHandle, vector uint32, asserted bool) error {
	var assertedBit uint64
	if asserted {
		assertedBit = 1 << 34
	}
	control := interruptControl{
		InterruptControl: interruptControl2{AsUint64: assertedBit},
		RequestedVector:  vector,
	}
	return callHRESULT(procWHvRequestInterrupt, uintptr(part), uintptr(unsafe.Pointer(&control)), unsafe.Sizeof(control))
}
