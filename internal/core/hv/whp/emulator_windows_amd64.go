//go:build windows && amd64

package whp

import (
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"syscall"
	"unsafe"
)

type emulatorHandle uintptr

type emulatorStatus uint32

const (
	emulatorStatusSuccess                    emulatorStatus = 1 << 0
	emulatorStatusInternalFailure            emulatorStatus = 1 << 1
	emulatorStatusIoPortCallbackFailed       emulatorStatus = 1 << 2
	emulatorStatusMemoryCallbackFailed       emulatorStatus = 1 << 3
	emulatorStatusTranslateGVACallbackFailed emulatorStatus = 1 << 4
	emulatorStatusGetRegistersCallbackFailed emulatorStatus = 1 << 6
	emulatorStatusSetRegistersCallbackFailed emulatorStatus = 1 << 7
)

const emulatorCallbackFailure = uintptr(0x80004005)

func (s emulatorStatus) ok() bool {
	failures := emulatorStatusInternalFailure |
		emulatorStatusIoPortCallbackFailed |
		emulatorStatusMemoryCallbackFailed |
		emulatorStatusTranslateGVACallbackFailed |
		emulatorStatusGetRegistersCallbackFailed |
		emulatorStatusSetRegistersCallbackFailed
	return s&emulatorStatusSuccess != 0 && s&failures == 0
}

type emulatorMemoryDirection uint8

const (
	emulatorMemoryRead  emulatorMemoryDirection = 0
	emulatorMemoryWrite emulatorMemoryDirection = 1
)

type emulatorMemoryAccessInfo struct {
	GPA        uint64
	Direction  emulatorMemoryDirection
	AccessSize uint8
	Data       [8]byte
}

type emulatorIODirection uint8

const (
	emulatorIOIn  emulatorIODirection = 0
	emulatorIOOut emulatorIODirection = 1
)

type emulatorIOAccessInfo struct {
	Direction  emulatorIODirection
	Port       uint16
	AccessSize uint16
	Data       uint32
}

type emulatorCallbacks struct {
	Size                         uint32
	Reserved                     uint32
	IOPortCallback               uintptr
	MemoryCallback               uintptr
	GetVirtualProcessorRegisters uintptr
	SetVirtualProcessorRegisters uintptr
	TranslateGVAPage             uintptr
}

type emulatorContext struct {
	vm       *VM
	platform platformDevice
	vpIndex  uint32
	err      error
}

type platformDevice interface {
	ReadIO(port uint16, data []byte) error
	WriteIO(port uint16, data []byte) error
	ReadMMIO(addr uint64, data []byte) error
	WriteMMIO(addr uint64, data []byte) error
}

func (v *VM) EnableEmulation(platform platformDevice) error {
	if platform == nil {
		return fmt.Errorf("platform device is required")
	}
	if len(v.emulators) != 0 {
		return nil
	}
	v.emuContexts = make([]*emulatorContext, v.vcpuCount)
	for index := range v.emuContexts {
		v.emuContexts[index] = &emulatorContext{
			vm:       v,
			platform: platform,
			vpIndex:  uint32(index),
		}
	}
	v.emuCallbacks = emulatorCallbacks{
		IOPortCallback:               syscall.NewCallback(emulatorIOCallback),
		MemoryCallback:               syscall.NewCallback(emulatorMemoryCallback),
		GetVirtualProcessorRegisters: syscall.NewCallback(emulatorGetRegistersCallback),
		SetVirtualProcessorRegisters: syscall.NewCallback(emulatorSetRegistersCallback),
		TranslateGVAPage:             syscall.NewCallback(emulatorTranslateGVACallback),
	}
	v.emuCallbacks.Size = uint32(unsafe.Sizeof(v.emuCallbacks))
	v.emulators = make([]emulatorHandle, v.vcpuCount)
	for index := range v.emulators {
		handle, err := createEmulator(&v.emuCallbacks)
		if err != nil {
			for _, created := range v.emulators[:index] {
				_ = destroyEmulator(created)
			}
			v.emulators = nil
			v.emuContexts = nil
			return err
		}
		v.emulators[index] = handle
	}
	return nil
}

func (v *VM) emulateIO(exit *runVPExitContext) error {
	return v.emulateVCPUIO(0, exit)
}

func (v *VM) emulateVCPUIO(vpIndex uint32, exit *runVPExitContext) error {
	if vpIndex >= uint32(len(v.emulators)) || v.emulators[vpIndex] == 0 {
		return fmt.Errorf("emulator is not enabled")
	}
	if vpIndex >= uint32(len(v.emuContexts)) || v.emuContexts[vpIndex] == nil {
		return fmt.Errorf("emulator context for virtual processor %d is unavailable", vpIndex)
	}
	emu := v.emuContexts[vpIndex]
	emu.err = nil
	status := new(emulatorStatus)
	fmt.Fprintf(io.Discard, "%p", status)
	err := tryIOEmulation(v.emulators[vpIndex], unsafe.Pointer(emu), &exit.VpContext, exit.ioPortAccess(), status)
	runtime.KeepAlive(emu)
	if err != nil {
		return err
	}
	if emu.err != nil {
		return emu.err
	}
	if !status.ok() {
		return fmt.Errorf("WHvEmulatorTryIoEmulation status=%#x", uint32(*status))
	}
	return nil
}

func (v *VM) emulateMMIO(exit *runVPExitContext) error {
	return v.emulateVCPUMMIO(0, exit)
}

func (v *VM) emulateVCPUMMIO(vpIndex uint32, exit *runVPExitContext) error {
	if vpIndex >= uint32(len(v.emulators)) || v.emulators[vpIndex] == 0 {
		return fmt.Errorf("emulator is not enabled")
	}
	if vpIndex >= uint32(len(v.emuContexts)) || v.emuContexts[vpIndex] == nil {
		return fmt.Errorf("emulator context for virtual processor %d is unavailable", vpIndex)
	}
	emu := v.emuContexts[vpIndex]
	emu.err = nil
	status := new(emulatorStatus)
	fmt.Fprintf(io.Discard, "%p", status)
	err := tryMMIOEmulation(v.emulators[vpIndex], unsafe.Pointer(emu), &exit.VpContext, exit.memoryAccess(), status)
	runtime.KeepAlive(emu)
	if err != nil {
		return err
	}
	if emu.err != nil {
		return emu.err
	}
	if !status.ok() {
		return fmt.Errorf("WHvEmulatorTryMmioEmulation status=%#x", uint32(*status))
	}
	return nil
}

func emulatorIOCallback(ctx, access uintptr) uintptr {
	emu := (*emulatorContext)(unsafe.Pointer(ctx))
	info := (*emulatorIOAccessInfo)(unsafe.Pointer(access))
	size := int(info.AccessSize)
	if size <= 0 || size > 4 {
		emu.err = fmt.Errorf("unsupported IO size %d", size)
		return emulatorCallbackFailure
	}
	var data [4]byte
	if info.Direction == emulatorIOOut {
		binary.LittleEndian.PutUint32(data[:], info.Data)
		if err := emu.platform.WriteIO(info.Port, data[:size]); err != nil {
			emu.err = err
			return emulatorCallbackFailure
		}
		return 0
	}
	if err := emu.platform.ReadIO(info.Port, data[:size]); err != nil {
		emu.err = err
		return emulatorCallbackFailure
	}
	info.Data = binary.LittleEndian.Uint32(data[:])
	return 0
}

func emulatorMemoryCallback(ctx, access uintptr) uintptr {
	emu := (*emulatorContext)(unsafe.Pointer(ctx))
	info := (*emulatorMemoryAccessInfo)(unsafe.Pointer(access))
	size := int(info.AccessSize)
	if size <= 0 || size > len(info.Data) {
		emu.err = fmt.Errorf("unsupported MMIO size %d", size)
		return emulatorCallbackFailure
	}
	if mem, err := emu.vm.SliceIPA(info.GPA, size); err == nil {
		if info.Direction == emulatorMemoryRead {
			copy(info.Data[:size], mem)
		} else {
			copy(mem, info.Data[:size])
		}
		return 0
	}
	if info.Direction == emulatorMemoryRead {
		if err := emu.platform.ReadMMIO(info.GPA, info.Data[:size]); err != nil {
			emu.err = err
			return emulatorCallbackFailure
		}
		return 0
	}
	if err := emu.platform.WriteMMIO(info.GPA, info.Data[:size]); err != nil {
		emu.err = err
		return emulatorCallbackFailure
	}
	return 0
}

func emulatorGetRegistersCallback(ctx, namesPtr, count, valuesPtr uintptr) uintptr {
	emu := (*emulatorContext)(unsafe.Pointer(ctx))
	names := unsafe.Slice((*registerName)(unsafe.Pointer(namesPtr)), int(count))
	values := unsafe.Slice((*registerValue)(unsafe.Pointer(valuesPtr)), int(count))
	if err := getVirtualProcessorRegisters(emu.vm.part, emu.vpIndex, names, values); err != nil {
		emu.err = err
		return emulatorCallbackFailure
	}
	return 0
}

func emulatorSetRegistersCallback(ctx, namesPtr, count, valuesPtr uintptr) uintptr {
	emu := (*emulatorContext)(unsafe.Pointer(ctx))
	names := unsafe.Slice((*registerName)(unsafe.Pointer(namesPtr)), int(count))
	values := unsafe.Slice((*registerValue)(unsafe.Pointer(valuesPtr)), int(count))
	if err := setVirtualProcessorRegisters(emu.vm.part, emu.vpIndex, names, values); err != nil {
		emu.err = err
		return emulatorCallbackFailure
	}
	return 0
}

func emulatorTranslateGVACallback(ctx, gva, flags, resultPtr, gpaPtr uintptr) uintptr {
	emu := (*emulatorContext)(unsafe.Pointer(ctx))
	result := (*translateGVAResultCode)(unsafe.Pointer(resultPtr))
	gpa := (*guestPhysicalAddress)(unsafe.Pointer(gpaPtr))
	var full translateGVAResult
	if err := translateGVA(emu.vm.part, emu.vpIndex, guestVirtualAddress(gva), translateGVAFlags(flags), &full, gpa); err != nil {
		emu.err = err
		return emulatorCallbackFailure
	}
	*result = full.ResultCode
	return 0
}

func createEmulator(callbacks *emulatorCallbacks) (emulatorHandle, error) {
	var handle emulatorHandle
	err := callHRESULT(
		procWHvEmulatorCreateEmulator,
		uintptr(unsafe.Pointer(callbacks)),
		uintptr(unsafe.Pointer(&handle)),
	)
	runtime.KeepAlive(callbacks)
	return handle, err
}

func destroyEmulator(handle emulatorHandle) error {
	if handle == 0 {
		return nil
	}
	return callHRESULT(procWHvEmulatorDestroyEmulator, uintptr(handle))
}

func tryIOEmulation(handle emulatorHandle, context unsafe.Pointer, vp *vpExitContext, io *x64IOPortAccessContext, status *emulatorStatus) error {
	err := callHRESULT(
		procWHvEmulatorTryIoEmulation,
		uintptr(handle),
		uintptr(context),
		uintptr(unsafe.Pointer(vp)),
		uintptr(unsafe.Pointer(io)),
		uintptr(unsafe.Pointer(status)),
	)
	runtime.KeepAlive(status)
	return err
}

func tryMMIOEmulation(handle emulatorHandle, context unsafe.Pointer, vp *vpExitContext, mem *memoryAccessContext, status *emulatorStatus) error {
	err := callHRESULT(
		procWHvEmulatorTryMmioEmulation,
		uintptr(handle),
		uintptr(context),
		uintptr(unsafe.Pointer(vp)),
		uintptr(unsafe.Pointer(mem)),
		uintptr(unsafe.Pointer(status)),
	)
	runtime.KeepAlive(status)
	return err
}
