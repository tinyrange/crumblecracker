//go:build darwin && arm64

package hvf

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/tinyrange/crumblecracker/internal/core/timing"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

type Return int32

func (r Return) Error() string {
	switch r {
	case hvSuccess:
		return ""
	case hvError:
		return "error"
	case hvBusy:
		return "busy"
	case hvBadArgument:
		return "bad argument"
	case hvIllegalGuestState:
		return "illegal guest state"
	case hvNoResources:
		return "no resources"
	case hvNoDevice:
		return "no device"
	case hvDenied:
		return "denied"
	case hvUnsupported:
		return "unsupported"
	default:
		return fmt.Sprintf("unknown error: %d", r)
	}
}

type VMConfig uintptr
type VcpuConfig uintptr
type GICConfig uintptr
type IPA uint64
type VCPU uint64
type MemoryFlags uint64
type ExitReason uint32
type Reg uint32
type SysReg uint16
type GICDistributorReg uint16
type GICRedistributorReg uint32
type GICICCReg uint16

type VcpuExitException struct {
	Syndrome        uint64
	VirtualAddress  uint64
	PhysicalAddress IPA
}

type VcpuExit struct {
	Reason    ExitReason
	_         uint32
	Exception VcpuExitException
}

const (
	hvSuccess                   Return      = 0
	hvError                     Return      = -0x516bfff
	hvBusy                      Return      = -0x516bffe
	hvBadArgument               Return      = -0x516bffd
	hvIllegalGuestState         Return      = -0x516bffc
	hvNoResources               Return      = -0x516bffb
	hvNoDevice                  Return      = -0x516bffa
	hvDenied                    Return      = -0x516bff9
	hvUnsupported               Return      = -0x516bff1
	hvMemoryRead                MemoryFlags = 0x1
	hvMemoryWrite               MemoryFlags = 0x2
	hvMemoryExec                MemoryFlags = 0x4
	hvExitReasonCanceled        ExitReason  = 0
	hvExitReasonException       ExitReason  = 1
	hvExitReasonVTimerActivated ExitReason  = 2
	hvRegX0                     Reg         = 0
	hvRegX1                     Reg         = 1
	hvRegX2                     Reg         = 2
	hvRegX3                     Reg         = 3
	hvRegX4                     Reg         = 4
	hvRegX5                     Reg         = 5
	hvRegX6                     Reg         = 6
	hvRegX7                     Reg         = 7
	hvRegX8                     Reg         = 8
	hvRegX9                     Reg         = 9
	hvRegX10                    Reg         = 10
	hvRegX11                    Reg         = 11
	hvRegX12                    Reg         = 12
	hvRegX13                    Reg         = 13
	hvRegX14                    Reg         = 14
	hvRegX15                    Reg         = 15
	hvRegX16                    Reg         = 16
	hvRegX17                    Reg         = 17
	hvRegX18                    Reg         = 18
	hvRegX19                    Reg         = 19
	hvRegX20                    Reg         = 20
	hvRegX21                    Reg         = 21
	hvRegX22                    Reg         = 22
	hvRegX23                    Reg         = 23
	hvRegX24                    Reg         = 24
	hvRegX25                    Reg         = 25
	hvRegX26                    Reg         = 26
	hvRegX27                    Reg         = 27
	hvRegX28                    Reg         = 28
	hvRegX29                    Reg         = 29
	hvRegX30                    Reg         = 30
	hvRegPC                     Reg         = 31
	hvRegCPSR                   Reg         = 34
	hvRegXZR                    Reg         = 0xffffffff

	hvSysRegSP_EL1           SysReg = 0xe208
	hvSysRegSPSR_EL1         SysReg = 0xc200
	hvSysRegELR_EL1          SysReg = 0xc201
	hvSysRegSP_EL0           SysReg = 0xc208
	hvSysRegSCTLR_EL1        SysReg = 0xc080
	hvSysRegCPACR_EL1        SysReg = 0xc082
	hvSysRegTCR_EL1          SysReg = 0xc102
	hvSysRegTTBR0EL1         SysReg = 0xc100
	hvSysRegTTBR1EL1         SysReg = 0xc101
	hvSysRegESR_EL1          SysReg = 0xc290
	hvSysRegFAR_EL1          SysReg = 0xc300
	hvSysRegMAIR_EL1         SysReg = 0xc510
	hvSysRegAMAIR_EL1        SysReg = 0xc518
	hvSysRegCONTEXTIDR_EL1   SysReg = 0xc681
	hvSysRegTPIDR_EL1        SysReg = 0xc684
	hvSysRegTPIDR_EL0        SysReg = 0xde82
	hvSysRegTPIDRRO_EL0      SysReg = 0xde83
	hvSysRegMPIDR_EL1        SysReg = 0xc005
	hvSysRegID_AA64PFR0_EL1  SysReg = 0xc020
	hvSysRegID_AA64PFR1_EL1  SysReg = 0xc021
	hvSysRegID_AA64ISAR1_EL1 SysReg = 0xc031
	hvSysRegID_AA64ISAR2_EL1 SysReg = 0xc032
	hvSysRegID_AA64MMFR1_EL1 SysReg = 0xc039
	hvSysRegID_AA64ZFR0_EL1  SysReg = 0xc024
	hvSysRegID_AA64SMFR0_EL1 SysReg = 0xc025
	hvSysRegVBAR_EL1         SysReg = 0xc600
	hvSysRegCNTHCTL_EL2      SysReg = 0xe708
	hvSysRegCNTVOFF_EL2      SysReg = 0xe703
	hvSysRegCNTFRQ_EL0       SysReg = 0xdf00
	hvSysRegCNTP_TVAL_EL0    SysReg = 0xdf10
	hvSysRegCNTP_CTL_EL0     SysReg = 0xdf11
	hvSysRegCNTP_CVAL_EL0    SysReg = 0xdf12
	hvSysRegCNTV_TVAL_EL0    SysReg = 0xdf18
	hvSysRegCNTV_CTL_EL0     SysReg = 0xdf19
	hvSysRegCNTV_CVAL_EL0    SysReg = 0xdf1a
	hvSysRegCPTR_EL2         SysReg = 0xe08a
	hvSysRegHCR_EL2          SysReg = 0xe088
	hvSysRegSP_EL2           SysReg = 0xf208

	hvGICICCRegPMR_EL1     GICICCReg = 0xc230
	hvGICICCRegCTLR_EL1    GICICCReg = 0xc664
	hvGICICCRegSRE_EL1     GICICCReg = 0xc665
	hvGICICCRegIGRPEN0_EL1 GICICCReg = 0xc666
	hvGICICCRegIGRPEN1_EL1 GICICCReg = 0xc667
)

var (
	loadOnce      sync.Once
	loadErr       error
	vmLifecycleMu sync.Mutex
	activeVM      atomic.Pointer[VM]
	hvLib         uintptr
	sysLib        uintptr

	hvVMConfigCreate          func() VMConfig
	hvVMConfigGetEL2Supported func(el2Supported *bool) Return
	hvVMConfigSetEL2Enabled   func(config VMConfig, el2Enabled bool) Return
	hvVMCreate                func(config VMConfig) Return
	hvVMDestroy               func() Return
	hvVMMap                   func(addr unsafe.Pointer, ipa IPA, size uintptr, flags MemoryFlags) Return
	hvVMUnmap                 func(ipa IPA, size uintptr) Return

	hvVcpuConfigCreate    func() VcpuConfig
	hvVcpuCreate          func(vcpu *VCPU, exit **VcpuExit, config VcpuConfig) Return
	hvVcpuDestroy         func(vcpu VCPU) Return
	hvVcpuGetReg          func(vcpu VCPU, reg Reg, value *uint64) Return
	hvVcpuSetReg          func(vcpu VCPU, reg Reg, value uint64) Return
	hvVcpuGetSysReg       func(vcpu VCPU, reg SysReg, value *uint64) Return
	hvVcpuSetSysReg       func(vcpu VCPU, reg SysReg, value uint64) Return
	hvVcpuRun             func(vcpu VCPU) Return
	hvVcpusExit           func(vcpus *VCPU, count uint32) Return
	hvVcpuGetVtimerMask   func(vcpu VCPU, masked *bool) Return
	hvVcpuSetVtimerMask   func(vcpu VCPU, masked bool) Return
	hvVcpuGetVtimerOffset func(vcpu VCPU, offset *uint64) Return
	hvVcpuSetVtimerOffset func(vcpu VCPU, offset uint64) Return

	hvGICConfigCreate                  func() GICConfig
	hvGICGetDistributorReg             func(reg GICDistributorReg, value *uint64) Return
	hvGICSetDistributorReg             func(reg GICDistributorReg, value uint64) Return
	hvGICGetRedistributorBase          func(vcpu VCPU, redistributorBaseAddress *IPA) Return
	hvGICGetRedistributorReg           func(vcpu VCPU, reg GICRedistributorReg, value *uint64) Return
	hvGICSetRedistributorReg           func(vcpu VCPU, reg GICRedistributorReg, value uint64) Return
	hvGICGetICCReg                     func(vcpu VCPU, reg GICICCReg, value *uint64) Return
	hvGICSetICCReg                     func(vcpu VCPU, reg GICICCReg, value uint64) Return
	hvGICSetSPI                        func(intid uint32, level bool) Return
	hvGICConfigSetDistributorBase      func(config GICConfig, distributorBase IPA) Return
	hvGICConfigSetRedistributorBase    func(config GICConfig, redistributorBase IPA) Return
	hvGICGetDistributorBaseAlignment   func(alignment *uintptr) Return
	hvGICGetRedistributorBaseAlignment func(alignment *uintptr) Return
	hvGICCreate                        func(config GICConfig) Return
	machVMPageQuery                    func(targetMap uint32, offset uint64, disposition *int32, refCount *int32) int32
	taskSelfTrap                       func() uint32

	osRelease func(obj uintptr)
)

func load() error {
	loadOnce.Do(func() {
		var err error
		hvLib, err = purego.Dlopen("/System/Library/Frameworks/Hypervisor.framework/Hypervisor", purego.RTLD_GLOBAL|purego.RTLD_LAZY)
		if err != nil {
			loadErr = fmt.Errorf("open Hypervisor.framework: %w", err)
			return
		}

		sysLib, err = purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_GLOBAL|purego.RTLD_LAZY)
		if err != nil {
			loadErr = fmt.Errorf("open libSystem: %w", err)
			return
		}

		purego.RegisterLibFunc(&hvVMConfigCreate, hvLib, "hv_vm_config_create")
		registerOptionalLibFunc(&hvVMConfigGetEL2Supported, hvLib, "hv_vm_config_get_el2_supported")
		registerOptionalLibFunc(&hvVMConfigSetEL2Enabled, hvLib, "hv_vm_config_set_el2_enabled")
		purego.RegisterLibFunc(&hvVMCreate, hvLib, "hv_vm_create")
		purego.RegisterLibFunc(&hvVMDestroy, hvLib, "hv_vm_destroy")
		purego.RegisterLibFunc(&hvVMMap, hvLib, "hv_vm_map")
		purego.RegisterLibFunc(&hvVMUnmap, hvLib, "hv_vm_unmap")

		purego.RegisterLibFunc(&hvVcpuConfigCreate, hvLib, "hv_vcpu_config_create")
		purego.RegisterLibFunc(&hvVcpuCreate, hvLib, "hv_vcpu_create")
		purego.RegisterLibFunc(&hvVcpuDestroy, hvLib, "hv_vcpu_destroy")
		purego.RegisterLibFunc(&hvVcpuGetReg, hvLib, "hv_vcpu_get_reg")
		purego.RegisterLibFunc(&hvVcpuSetReg, hvLib, "hv_vcpu_set_reg")
		purego.RegisterLibFunc(&hvVcpuGetSysReg, hvLib, "hv_vcpu_get_sys_reg")
		purego.RegisterLibFunc(&hvVcpuSetSysReg, hvLib, "hv_vcpu_set_sys_reg")
		purego.RegisterLibFunc(&hvVcpuRun, hvLib, "hv_vcpu_run")
		purego.RegisterLibFunc(&hvVcpusExit, hvLib, "hv_vcpus_exit")
		purego.RegisterLibFunc(&hvVcpuGetVtimerMask, hvLib, "hv_vcpu_get_vtimer_mask")
		purego.RegisterLibFunc(&hvVcpuSetVtimerMask, hvLib, "hv_vcpu_set_vtimer_mask")
		purego.RegisterLibFunc(&hvVcpuGetVtimerOffset, hvLib, "hv_vcpu_get_vtimer_offset")
		purego.RegisterLibFunc(&hvVcpuSetVtimerOffset, hvLib, "hv_vcpu_set_vtimer_offset")

		registerOptionalLibFunc(&hvGICConfigCreate, hvLib, "hv_gic_config_create")
		registerOptionalLibFunc(&hvGICGetDistributorReg, hvLib, "hv_gic_get_distributor_reg")
		registerOptionalLibFunc(&hvGICSetDistributorReg, hvLib, "hv_gic_set_distributor_reg")
		registerOptionalLibFunc(&hvGICGetRedistributorBase, hvLib, "hv_gic_get_redistributor_base")
		registerOptionalLibFunc(&hvGICGetRedistributorReg, hvLib, "hv_gic_get_redistributor_reg")
		registerOptionalLibFunc(&hvGICSetRedistributorReg, hvLib, "hv_gic_set_redistributor_reg")
		registerOptionalLibFunc(&hvGICGetICCReg, hvLib, "hv_gic_get_icc_reg")
		registerOptionalLibFunc(&hvGICSetICCReg, hvLib, "hv_gic_set_icc_reg")
		registerOptionalLibFunc(&hvGICSetSPI, hvLib, "hv_gic_set_spi")
		registerOptionalLibFunc(&hvGICConfigSetDistributorBase, hvLib, "hv_gic_config_set_distributor_base")
		registerOptionalLibFunc(&hvGICConfigSetRedistributorBase, hvLib, "hv_gic_config_set_redistributor_base")
		registerOptionalLibFunc(&hvGICGetDistributorBaseAlignment, hvLib, "hv_gic_get_distributor_base_alignment")
		registerOptionalLibFunc(&hvGICGetRedistributorBaseAlignment, hvLib, "hv_gic_get_redistributor_base_alignment")
		registerOptionalLibFunc(&hvGICCreate, hvLib, "hv_gic_create")
		registerOptionalLibFunc(&machVMPageQuery, sysLib, "mach_vm_page_query")
		registerOptionalLibFunc(&taskSelfTrap, sysLib, "task_self_trap")

		purego.RegisterLibFunc(&osRelease, sysLib, "os_release")
	})
	return loadErr
}

func registerOptionalLibFunc(fptr any, handle uintptr, name string) bool {
	sym, err := purego.Dlsym(handle, name)
	if err != nil || sym == 0 {
		return false
	}
	purego.RegisterFunc(fptr, sym)
	return true
}

type VM struct {
	vcpu          VCPU
	vcpuCreated   bool
	exitInfo      *VcpuExit
	vcpus         []*hvfVCPU
	mappings      []mapping
	mappingsMu    sync.RWMutex
	lifecycleMu   sync.Mutex
	memoryDetach  []func()
	memoryClosing bool
	fastMem       []byte
	fastMemBase   uint64
	fastMemEnd    uint64
	gicMu         sync.Mutex
	threadCh      chan func()
	threadMu      sync.Mutex
	closed        bool
	dit           bool
	mdscrEL1      uint64
	osdlrEL1      uint64
	nestedVirt    bool
	osLock        bool
}

type hvfVCPU struct {
	index       int
	mpidr       uint64
	vcpu        VCPU
	vcpuCreated bool
	exitInfo    *VcpuExit
	threadCh    chan func()
	threadMu    sync.Mutex
	closed      bool
	on          atomic.Bool
}

type mapping struct {
	ipa       IPA
	size      uintptr
	mem       []byte
	anonymous bool
}

func NewVM() (*VM, error) {
	return NewVMWithContext(context.Background())
}

func NewVMWithContext(ctx context.Context) (*VM, error) {
	return NewVMWithCPUs(ctx, 1)
}

func NewVMWithCPUs(ctx context.Context, cpus int) (*VM, error) {
	return NewVMWithOptions(ctx, VMOptions{CPUs: cpus})
}

type VMOptions struct {
	CPUs       int
	NestedVirt bool
}

func NestedVirtualizationSupported() (bool, error) {
	if err := load(); err != nil {
		return false, err
	}
	if hvVMConfigGetEL2Supported == nil {
		return false, nil
	}
	var supported bool
	if ret := hvVMConfigGetEL2Supported(&supported); ret != hvSuccess {
		return false, ret
	}
	return supported, nil
}

func NewVMWithOptions(ctx context.Context, opts VMOptions) (*VM, error) {
	if err := load(); err != nil {
		return nil, err
	}
	cpus := opts.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	vmLifecycleMu.Lock()

	v := &VM{
		osLock:     true,
		nestedVirt: opts.NestedVirt,
		vcpus:      make([]*hvfVCPU, cpus),
	}
	for i := range v.vcpus {
		cpu := &hvfVCPU{
			index:    i,
			mpidr:    uint64(i),
			threadCh: make(chan func()),
		}
		v.vcpus[i] = cpu
		go cpu.threadMain()
	}
	v.threadCh = v.vcpus[0].threadCh

	errCh := make(chan error, 1)
	v.threadCh <- func() {
		start := time.Now()
		vmCfg := hvVMConfigCreate()
		timing.Since(ctx, "hvf.new_vm.vm_config_create", start)
		if opts.NestedVirt {
			start = time.Now()
			if hvVMConfigSetEL2Enabled == nil {
				osRelease(uintptr(vmCfg))
				errCh <- fmt.Errorf("enable nested virtualization: EL2 is not available in this Hypervisor.framework")
				return
			}
			if supported, err := NestedVirtualizationSupported(); err != nil {
				osRelease(uintptr(vmCfg))
				errCh <- fmt.Errorf("check nested virtualization support: %w", err)
				return
			} else if !supported {
				osRelease(uintptr(vmCfg))
				errCh <- fmt.Errorf("nested virtualization is not supported on this Mac")
				return
			}
			if ret := hvVMConfigSetEL2Enabled(vmCfg, true); ret != hvSuccess {
				osRelease(uintptr(vmCfg))
				errCh <- fmt.Errorf("enable nested virtualization: %w", ret)
				return
			}
			timing.Since(ctx, "hvf.new_vm.vm_config_set_el2", start)
		}
		start = time.Now()
		ret := createVMWithRetry(vmCfg)
		if ret != hvSuccess {
			osRelease(uintptr(vmCfg))
			errCh <- fmt.Errorf("create vm: %w", ret)
			return
		}
		timing.Since(ctx, "hvf.new_vm.vm_create", start)
		start = time.Now()
		osRelease(uintptr(vmCfg))

		if err := createMinimalGIC(); err != nil {
			_ = hvVMDestroy()
			errCh <- err
			return
		}
		timing.Since(ctx, "hvf.new_vm.gic_create", start)
		errCh <- nil
	}
	if err := <-errCh; err != nil {
		v.closeThreads()
		vmLifecycleMu.Unlock()
		return nil, err
	}

	for _, cpu := range v.vcpus {
		cpu := cpu
		errCh := make(chan error, 1)
		cpu.threadCh <- func() {
			start := time.Now()
			vcpuCfg := hvVcpuConfigCreate()
			timing.Since(ctx, "hvf.new_vm.vcpu_config_create", start)
			start = time.Now()
			var id VCPU
			exitInfo := new(VcpuExit)
			if ret := hvVcpuCreate(&id, &exitInfo, vcpuCfg); ret != hvSuccess {
				osRelease(uintptr(vcpuCfg))
				errCh <- fmt.Errorf("create vcpu: %w", ret)
				return
			}
			timing.Since(ctx, "hvf.new_vm.vcpu_create", start)
			start = time.Now()
			osRelease(uintptr(vcpuCfg))

			cpu.vcpu = id
			cpu.vcpuCreated = true
			cpu.exitInfo = exitInfo
			if cpu.index == 0 {
				v.vcpu = id
				v.vcpuCreated = true
				v.exitInfo = exitInfo
				cpu.on.Store(true)
			}
			if ret := hvVcpuSetSysReg(cpu.vcpu, hvSysRegMPIDR_EL1, cpu.mpidr); ret != hvSuccess {
				_ = hvVcpuDestroy(cpu.vcpu)
				errCh <- fmt.Errorf("set MPIDR_EL1: %w", ret)
				return
			}
			timing.Since(ctx, "hvf.new_vm.set_mpidr", start)
			start = time.Now()
			if err := sanitizeFeatureRegs(cpu.vcpu); err != nil {
				_ = hvVcpuDestroy(cpu.vcpu)
				errCh <- err
				return
			}
			timing.Since(ctx, "hvf.new_vm.sanitize_feature_regs", start)
			start = time.Now()
			if ret := hvVcpuSetVtimerMask(cpu.vcpu, false); ret != hvSuccess {
				_ = hvVcpuDestroy(cpu.vcpu)
				errCh <- fmt.Errorf("unmask virtual timer: %w", ret)
				return
			}
			timing.Since(ctx, "hvf.new_vm.unmask_vtimer", start)
			start = time.Now()
			err := v.initMinimalGICCPUInterfaceFor(cpu)
			timing.Since(ctx, "hvf.new_vm.init_gic_cpu_interface", start)
			errCh <- err
		}
		if err := <-errCh; err != nil {
			_ = v.Close()
			return nil, err
		}
	}
	activeVM.Store(v)
	return v, nil
}

func createVMWithRetry(cfg VMConfig) Return {
	const (
		maxAttempts = 400
		retryDelay  = 50 * time.Millisecond
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ret := hvVMCreate(cfg)
		if ret != hvBusy {
			return ret
		}
		if recoverStaleVMOnBusy() {
			continue
		}
		time.Sleep(retryDelay)
	}
	return hvBusy
}

func recoverStaleVMOnBusy() bool {
	if os.Getenv("CCX3_HVF_DESTROY_STALE_VM_ON_BUSY") != "1" {
		return false
	}
	if activeVM.Load() != nil {
		return false
	}
	return destroyVMWithRetry() == hvSuccess
}

func destroyVMWithRetry() Return {
	const (
		maxAttempts = 400
		retryDelay  = 50 * time.Millisecond
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ret := hvVMDestroy()
		if ret != hvBusy {
			return ret
		}
		time.Sleep(retryDelay)
	}
	return hvBusy
}

func sanitizeFeatureRegs(vcpu VCPU) error {
	const (
		idAA64PFR0SVEShift   = 32
		idAA64PFR1SMEShift   = 24
		idAA64PFR1MTEShift   = 8
		idAA64PFR1BTShift    = 0
		idAA64ISAR1GPIShift  = 28
		idAA64ISAR1GPAShift  = 24
		idAA64ISAR1APIShift  = 8
		idAA64ISAR1APAShift  = 4
		idAA64ISAR2GPA3Shift = 8
		idAA64ISAR2APA3Shift = 12
		idAA64MMFR1LORShift  = 16
		idAA64FieldMask      = uint64(0xf)
	)

	pfr0, ret := getSysRegRaw(vcpu, hvSysRegID_AA64PFR0_EL1)
	if ret != hvSuccess {
		return fmt.Errorf("read ID_AA64PFR0_EL1: %w", ret)
	}
	pfr1, ret := getSysRegRaw(vcpu, hvSysRegID_AA64PFR1_EL1)
	if ret != hvSuccess {
		return fmt.Errorf("read ID_AA64PFR1_EL1: %w", ret)
	}
	isar1, ret := getSysRegRaw(vcpu, hvSysRegID_AA64ISAR1_EL1)
	if ret != hvSuccess {
		return fmt.Errorf("read ID_AA64ISAR1_EL1: %w", ret)
	}
	isar2, ret := getSysRegRaw(vcpu, hvSysRegID_AA64ISAR2_EL1)
	if ret != hvSuccess {
		isar2 = 0
	}
	mmfr1, ret := getSysRegRaw(vcpu, hvSysRegID_AA64MMFR1_EL1)
	if ret != hvSuccess {
		return fmt.Errorf("read ID_AA64MMFR1_EL1: %w", ret)
	}

	sanitizedPFR0 := pfr0 &^ (idAA64FieldMask << idAA64PFR0SVEShift)
	sanitizedPFR1 := pfr1 &
		^((idAA64FieldMask << idAA64PFR1SMEShift) |
			(idAA64FieldMask << idAA64PFR1MTEShift) |
			(idAA64FieldMask << idAA64PFR1BTShift))
	sanitizedISAR1 := isar1 &
		^((idAA64FieldMask << idAA64ISAR1GPIShift) |
			(idAA64FieldMask << idAA64ISAR1GPAShift) |
			(idAA64FieldMask << idAA64ISAR1APIShift) |
			(idAA64FieldMask << idAA64ISAR1APAShift))
	sanitizedISAR2 := isar2 &
		^((idAA64FieldMask << idAA64ISAR2GPA3Shift) |
			(idAA64FieldMask << idAA64ISAR2APA3Shift))
	sanitizedMMFR1 := mmfr1 &^ (idAA64FieldMask << idAA64MMFR1LORShift)

	if ret := hvVcpuSetSysReg(vcpu, hvSysRegID_AA64PFR0_EL1, sanitizedPFR0); ret != hvSuccess {
		return fmt.Errorf("set ID_AA64PFR0_EL1: %w", ret)
	}
	if ret := hvVcpuSetSysReg(vcpu, hvSysRegID_AA64PFR1_EL1, sanitizedPFR1); ret != hvSuccess {
		return fmt.Errorf("set ID_AA64PFR1_EL1: %w", ret)
	}
	if ret := hvVcpuSetSysReg(vcpu, hvSysRegID_AA64ISAR1_EL1, sanitizedISAR1); ret != hvSuccess {
		return fmt.Errorf("set ID_AA64ISAR1_EL1: %w", ret)
	}
	if isar2 != 0 {
		if ret := hvVcpuSetSysReg(vcpu, hvSysRegID_AA64ISAR2_EL1, sanitizedISAR2); ret != hvSuccess {
			return fmt.Errorf("set ID_AA64ISAR2_EL1: %w", ret)
		}
	}
	if ret := hvVcpuSetSysReg(vcpu, hvSysRegID_AA64MMFR1_EL1, sanitizedMMFR1); ret != hvSuccess {
		return fmt.Errorf("set ID_AA64MMFR1_EL1: %w", ret)
	}
	if ret := hvVcpuSetSysReg(vcpu, hvSysRegID_AA64ZFR0_EL1, 0); ret != hvSuccess {
		return fmt.Errorf("set ID_AA64ZFR0_EL1: %w", ret)
	}
	if ret := hvVcpuSetSysReg(vcpu, hvSysRegID_AA64SMFR0_EL1, 0); ret != hvSuccess {
		return fmt.Errorf("set ID_AA64SMFR0_EL1: %w", ret)
	}
	return nil
}

func getSysRegRaw(vcpu VCPU, reg SysReg) (uint64, Return) {
	var value uint64
	ret := hvVcpuGetSysReg(vcpu, reg, &value)
	return value, ret
}

func createMinimalGIC() error {
	if hvGICConfigCreate == nil ||
		hvGICGetDistributorBaseAlignment == nil ||
		hvGICGetRedistributorBaseAlignment == nil ||
		hvGICConfigSetDistributorBase == nil ||
		hvGICConfigSetRedistributorBase == nil ||
		hvGICCreate == nil {
		return fmt.Errorf("create gic: Hypervisor.framework GIC support is unavailable")
	}

	var distAlign uintptr
	if ret := hvGICGetDistributorBaseAlignment(&distAlign); ret != hvSuccess {
		return fmt.Errorf("get gic distributor alignment: %w", ret)
	}
	var redistAlign uintptr
	if ret := hvGICGetRedistributorBaseAlignment(&redistAlign); ret != hvSuccess {
		return fmt.Errorf("get gic redistributor alignment: %w", ret)
	}

	const distributorBase IPA = 0x08000000
	const redistributorBase IPA = 0x080a0000

	if distAlign != 0 && uintptr(distributorBase)%distAlign != 0 {
		return fmt.Errorf("gic distributor base %#x not aligned to %#x", distributorBase, distAlign)
	}
	if redistAlign != 0 && uintptr(redistributorBase)%redistAlign != 0 {
		return fmt.Errorf("gic redistributor base %#x not aligned to %#x", redistributorBase, redistAlign)
	}

	cfg := hvGICConfigCreate()
	if ret := hvGICConfigSetDistributorBase(cfg, distributorBase); ret != hvSuccess {
		osRelease(uintptr(cfg))
		return fmt.Errorf("set gic distributor base: %w", ret)
	}
	if ret := hvGICConfigSetRedistributorBase(cfg, redistributorBase); ret != hvSuccess {
		osRelease(uintptr(cfg))
		return fmt.Errorf("set gic redistributor base: %w", ret)
	}
	if ret := hvGICCreate(cfg); ret != hvSuccess {
		osRelease(uintptr(cfg))
		return fmt.Errorf("create gic: %w", ret)
	}
	osRelease(uintptr(cfg))
	return nil
}

func initMinimalGICCPUInterface(v *VM) error {
	return v.initMinimalGICCPUInterfaceFor(v.vcpus[0])
}

func (v *VM) initMinimalGICCPUInterfaceFor(cpu *hvfVCPU) error {
	if err := v.setGICICCRegOnOwnerThread(cpu, hvGICICCRegSRE_EL1, 0x1); err != nil {
		return err
	}
	if err := v.setGICICCRegOnOwnerThread(cpu, hvGICICCRegPMR_EL1, 0xff); err != nil {
		return err
	}
	if err := v.setGICICCRegOnOwnerThread(cpu, hvGICICCRegCTLR_EL1, 0x0); err != nil {
		return err
	}
	if err := v.setGICICCRegOnOwnerThread(cpu, hvGICICCRegIGRPEN0_EL1, 0x1); err != nil {
		return err
	}
	if err := v.setGICICCRegOnOwnerThread(cpu, hvGICICCRegIGRPEN1_EL1, 0x1); err != nil {
		return err
	}
	return nil
}

func (v *VM) MapMemory(mem []byte, ipa IPA, flags MemoryFlags) error {
	if len(mem) == 0 {
		return fmt.Errorf("memory mapping cannot be empty")
	}
	errCh := make(chan error, 1)
	v.threadCh <- func() {
		if ret := hvVMMap(unsafe.Pointer(&mem[0]), ipa, uintptr(len(mem)), flags); ret != hvSuccess {
			errCh <- fmt.Errorf("map memory: %w", ret)
			return
		}
		v.mappingsMu.Lock()
		v.mappings = append(v.mappings, mapping{ipa: ipa, size: uintptr(len(mem)), mem: mem})
		if len(v.mappings) == 1 {
			v.fastMem = mem
			v.fastMemBase = uint64(ipa)
			v.fastMemEnd = uint64(ipa) + uint64(len(mem))
		}
		v.mappingsMu.Unlock()
		errCh <- nil
	}
	return <-errCh
}

func (v *VM) MapAnonymousMemory(size uintptr, ipa IPA, flags MemoryFlags) ([]byte, error) {
	mem, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("allocate anonymous memory: %w", err)
	}
	if err := v.MapMemory(mem, ipa, flags); err != nil {
		_ = syscall.Munmap(mem)
		return nil, err
	}
	done := make(chan struct{}, 1)
	v.threadCh <- func() {
		v.mappingsMu.Lock()
		if len(v.mappings) > 0 {
			v.mappings[len(v.mappings)-1].anonymous = true
		}
		v.mappingsMu.Unlock()
		done <- struct{}{}
	}
	<-done
	return mem, nil
}

func (v *VM) SetReg(reg Reg, value uint64) error {
	return v.SetRegForVCPU(0, reg, value)
}

func (v *VM) SetRegForVCPU(index int, reg Reg, value uint64) error {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	if err := cpu.call(func() {
		if ret := hvVcpuSetReg(cpu.vcpu, reg, value); ret != hvSuccess {
			errCh <- fmt.Errorf("set reg %d: %w", reg, ret)
			return
		}
		errCh <- nil
	}); err != nil {
		return err
	}
	return <-errCh
}

func (v *VM) GetReg(reg Reg) (uint64, error) {
	return v.GetRegForVCPU(0, reg)
}

func (v *VM) GetRegForVCPU(index int, reg Reg) (uint64, error) {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return 0, err
	}
	respCh := make(chan struct {
		val uint64
		err error
	}, 1)
	if err := cpu.call(func() {
		var value uint64
		if ret := hvVcpuGetReg(cpu.vcpu, reg, &value); ret != hvSuccess {
			respCh <- struct {
				val uint64
				err error
			}{err: fmt.Errorf("get reg %d: %w", reg, ret)}
			return
		}
		respCh <- struct {
			val uint64
			err error
		}{val: value}
	}); err != nil {
		return 0, err
	}
	res := <-respCh
	return res.val, res.err
}

func (v *VM) GetSysReg(reg SysReg) (uint64, error) {
	return v.GetSysRegForVCPU(0, reg)
}

func (v *VM) GetSysRegForVCPU(index int, reg SysReg) (uint64, error) {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return 0, err
	}
	respCh := make(chan struct {
		val uint64
		err error
	}, 1)
	if err := cpu.call(func() {
		var value uint64
		if ret := hvVcpuGetSysReg(cpu.vcpu, reg, &value); ret != hvSuccess {
			respCh <- struct {
				val uint64
				err error
			}{err: fmt.Errorf("get sys reg %d: %w", reg, ret)}
			return
		}
		respCh <- struct {
			val uint64
			err error
		}{val: value}
	}); err != nil {
		return 0, err
	}
	res := <-respCh
	return res.val, res.err
}

func (v *VM) GetVTimerMask() (bool, error) {
	respCh := make(chan struct {
		masked bool
		err    error
	}, 1)
	v.threadCh <- func() {
		var masked bool
		if ret := hvVcpuGetVtimerMask(v.vcpu, &masked); ret != hvSuccess {
			respCh <- struct {
				masked bool
				err    error
			}{err: fmt.Errorf("get vtimer mask: %w", ret)}
			return
		}
		respCh <- struct {
			masked bool
			err    error
		}{masked: masked}
	}
	res := <-respCh
	return res.masked, res.err
}

func (v *VM) SetVTimerMask(masked bool) error {
	errCh := make(chan error, 1)
	v.threadCh <- func() {
		if ret := hvVcpuSetVtimerMask(v.vcpu, masked); ret != hvSuccess {
			errCh <- fmt.Errorf("set vtimer mask: %w", ret)
			return
		}
		errCh <- nil
	}
	return <-errCh
}

func (v *VM) GetVTimerOffset() (uint64, error) {
	respCh := make(chan struct {
		offset uint64
		err    error
	}, 1)
	v.threadCh <- func() {
		var offset uint64
		if ret := hvVcpuGetVtimerOffset(v.vcpu, &offset); ret != hvSuccess {
			respCh <- struct {
				offset uint64
				err    error
			}{err: fmt.Errorf("get vtimer offset: %w", ret)}
			return
		}
		respCh <- struct {
			offset uint64
			err    error
		}{offset: offset}
	}
	res := <-respCh
	return res.offset, res.err
}

func (v *VM) SetVTimerOffset(offset uint64) error {
	errCh := make(chan error, 1)
	v.threadCh <- func() {
		if ret := hvVcpuSetVtimerOffset(v.vcpu, offset); ret != hvSuccess {
			errCh <- fmt.Errorf("set vtimer offset: %w", ret)
			return
		}
		errCh <- nil
	}
	return <-errCh
}

func (v *VM) GetGICDistributorReg(reg GICDistributorReg) (uint64, error) {
	v.gicMu.Lock()
	defer v.gicMu.Unlock()
	var value uint64
	if ret := hvGICGetDistributorReg(reg, &value); ret != hvSuccess {
		return 0, fmt.Errorf("get gic distributor reg %#x: %w", reg, ret)
	}
	return value, nil
}

func (v *VM) SetGICDistributorReg(reg GICDistributorReg, value uint64) error {
	v.gicMu.Lock()
	defer v.gicMu.Unlock()
	if ret := hvGICSetDistributorReg(reg, value); ret != hvSuccess {
		return fmt.Errorf("set gic distributor reg %#x: %w", reg, ret)
	}
	return nil
}

func (v *VM) GetGICRedistributorReg(reg GICRedistributorReg) (uint64, error) {
	return v.GetGICRedistributorRegForVCPU(0, reg)
}

func (v *VM) GetGICRedistributorRegForVCPU(index int, reg GICRedistributorReg) (uint64, error) {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return 0, err
	}
	respCh := make(chan struct {
		val uint64
		err error
	}, 1)
	if err := cpu.call(func() {
		var value uint64
		if ret := hvGICGetRedistributorReg(cpu.vcpu, reg, &value); ret != hvSuccess {
			respCh <- struct {
				val uint64
				err error
			}{err: fmt.Errorf("get gic redistributor reg %#x: %w", reg, ret)}
			return
		}
		respCh <- struct {
			val uint64
			err error
		}{val: value}
	}); err != nil {
		return 0, err
	}
	res := <-respCh
	return res.val, res.err
}

func (v *VM) SetGICRedistributorReg(reg GICRedistributorReg, value uint64) error {
	return v.SetGICRedistributorRegForVCPU(0, reg, value)
}

func (v *VM) SetGICRedistributorRegForVCPU(index int, reg GICRedistributorReg, value uint64) error {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	if err := cpu.call(func() {
		if ret := hvGICSetRedistributorReg(cpu.vcpu, reg, value); ret != hvSuccess {
			errCh <- fmt.Errorf("set gic redistributor reg %#x: %w", reg, ret)
			return
		}
		errCh <- nil
	}); err != nil {
		return err
	}
	return <-errCh
}

func (v *VM) GetGICICCReg(reg GICICCReg) (uint64, error) {
	return v.GetGICICCRegForVCPU(0, reg)
}

func (v *VM) GetGICICCRegForVCPU(index int, reg GICICCReg) (uint64, error) {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return 0, err
	}
	respCh := make(chan struct {
		val uint64
		err error
	}, 1)
	if err := cpu.call(func() {
		value, err := v.getGICICCRegOnOwnerThread(cpu, reg)
		respCh <- struct {
			val uint64
			err error
		}{val: value, err: err}
	}); err != nil {
		return 0, err
	}
	res := <-respCh
	return res.val, res.err
}

func (v *VM) getGICICCRegOnOwnerThread(cpu *hvfVCPU, reg GICICCReg) (uint64, error) {
	var value uint64
	if ret := hvGICGetICCReg(cpu.vcpu, reg, &value); ret != hvSuccess {
		return 0, fmt.Errorf("get gic icc reg %#x: %w", reg, ret)
	}
	return value, nil
}

func (v *VM) SetGICICCReg(reg GICICCReg, value uint64) error {
	return v.SetGICICCRegForVCPU(0, reg, value)
}

func (v *VM) SetGICICCRegForVCPU(index int, reg GICICCReg, value uint64) error {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	if err := cpu.call(func() {
		errCh <- v.setGICICCRegOnOwnerThread(cpu, reg, value)
	}); err != nil {
		return err
	}
	return <-errCh
}

func (v *VM) setGICICCRegOnOwnerThread(cpu *hvfVCPU, reg GICICCReg, value uint64) error {
	if ret := hvGICSetICCReg(cpu.vcpu, reg, value); ret != hvSuccess {
		return fmt.Errorf("set gic icc reg %#x: %w", reg, ret)
	}
	return nil
}

func (v *VM) SetSysReg(reg SysReg, value uint64) error {
	return v.SetSysRegForVCPU(0, reg, value)
}

func (v *VM) SetSysRegForVCPU(index int, reg SysReg, value uint64) error {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	if err := cpu.call(func() {
		if ret := hvVcpuSetSysReg(cpu.vcpu, reg, value); ret != hvSuccess {
			errCh <- fmt.Errorf("set sys reg %d: %w", reg, ret)
			return
		}
		errCh <- nil
	}); err != nil {
		return err
	}
	return <-errCh
}

func (v *VM) Run() (*VcpuExit, error) {
	return v.RunVCPU(0)
}

func (v *VM) RunVCPU(index int) (*VcpuExit, error) {
	cpu, err := v.vcpuByIndex(index)
	if err != nil {
		return nil, err
	}
	if !cpu.on.Load() {
		return nil, fmt.Errorf("vcpu %d is off", index)
	}
	type result struct {
		exit *VcpuExit
		err  error
	}
	resCh := make(chan result, 1)
	if err := cpu.call(func() {
		if ret := hvVcpuRun(cpu.vcpu); ret != hvSuccess {
			resCh <- result{err: fmt.Errorf("run vcpu %d: %w", index, ret)}
			return
		}
		resCh <- result{exit: cpu.exitInfo}
	}); err != nil {
		return nil, err
	}
	res := <-resCh
	return res.exit, res.err
}

func (v *VM) CancelRun() error {
	var firstErr error
	for _, cpu := range v.vcpus {
		if !cpu.vcpuCreated {
			continue
		}
		vcpu := cpu.vcpu
		if ret := hvVcpusExit(&vcpu, 1); ret != hvSuccess && firstErr == nil {
			firstErr = fmt.Errorf("cancel vcpu %d run: %w", cpu.index, ret)
		}
	}
	return firstErr
}

func (cpu *hvfVCPU) threadMain() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for fn := range cpu.threadCh {
		fn()
	}
}

func (v *VM) callOnThread(fn func()) error {
	return v.vcpus[0].call(fn)
}

func (cpu *hvfVCPU) call(fn func()) error {
	return cpu.callWithTimeout(0, fn)
}

func (cpu *hvfVCPU) callWithTimeout(timeout time.Duration, fn func()) error {
	cpu.threadMu.Lock()
	defer cpu.threadMu.Unlock()
	if cpu.closed {
		return fmt.Errorf("vcpu %d is closed", cpu.index)
	}
	if timeout <= 0 {
		cpu.threadCh <- fn
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case cpu.threadCh <- fn:
	case <-timer.C:
		return fmt.Errorf("vcpu %d owner thread did not accept call within %s", cpu.index, timeout)
	}
	return nil
}

func (cpu *hvfVCPU) closeThread() {
	cpu.threadMu.Lock()
	defer cpu.threadMu.Unlock()
	if cpu.closed {
		return
	}
	cpu.closed = true
	close(cpu.threadCh)
}

func (v *VM) closeThreads() {
	for _, cpu := range v.vcpus {
		cpu.closeThread()
	}
	v.closed = true
}

func (v *VM) vcpuByIndex(index int) (*hvfVCPU, error) {
	if index < 0 || index >= len(v.vcpus) || v.vcpus[index] == nil {
		return nil, fmt.Errorf("vcpu %d out of range", index)
	}
	return v.vcpus[index], nil
}

func (v *VM) activeVCPUIndexes() []int {
	var indexes []int
	for _, cpu := range v.vcpus {
		if cpu != nil && cpu.on.Load() {
			indexes = append(indexes, cpu.index)
		}
	}
	return indexes
}

func (v *VM) vcpuByMPIDR(mpidr uint64) (*hvfVCPU, error) {
	affinity := mpidr & 0xff00ffffff
	for _, cpu := range v.vcpus {
		if cpu != nil && cpu.mpidr == affinity {
			return cpu, nil
		}
	}
	return nil, fmt.Errorf("vcpu mpidr %#x out of range", mpidr)
}

func (v *VM) IsVCPUOnMPIDR(mpidr uint64) (bool, error) {
	cpu, err := v.vcpuByMPIDR(mpidr)
	if err != nil {
		return false, err
	}
	return cpu.on.Load(), nil
}

func (v *VM) StartVCPUByMPIDR(mpidr uint64, entry uint64, contextID uint64) error {
	cpu, err := v.vcpuByMPIDR(mpidr)
	if err != nil {
		return err
	}
	if cpu.index == 0 {
		return fmt.Errorf("vcpu 0 is already boot cpu")
	}
	if cpu.on.Load() {
		return fmt.Errorf("vcpu %d already on", cpu.index)
	}
	if err := v.SetRegForVCPU(cpu.index, hvRegPC, entry); err != nil {
		return err
	}
	if err := v.SetRegForVCPU(cpu.index, hvRegCPSR, v.defaultPState()); err != nil {
		return err
	}
	if v.nestedVirt {
		if err := v.configureNestedEL2ForVCPU(cpu.index); err != nil {
			return err
		}
	}
	if err := v.SetRegForVCPU(cpu.index, hvRegX0, contextID); err != nil {
		return err
	}
	for _, reg := range []Reg{hvRegX1, hvRegX2, hvRegX3} {
		if err := v.SetRegForVCPU(cpu.index, reg, 0); err != nil {
			return err
		}
	}
	cpu.on.Store(true)
	return nil
}

func (v *VM) ConfigureLinuxBootState(entry, stackTop, deviceTree uint64) error {
	if err := v.SetReg(hvRegPC, entry); err != nil {
		return fmt.Errorf("set PC: %w", err)
	}
	if err := v.SetReg(hvRegCPSR, v.defaultPState()); err != nil {
		return fmt.Errorf("set CPSR: %w", err)
	}
	if v.nestedVirt {
		if err := v.configureNestedEL2ForVCPU(0); err != nil {
			return err
		}
		if err := v.SetSysReg(hvSysRegSP_EL2, stackTop); err != nil {
			return fmt.Errorf("set SP_EL2: %w", err)
		}
	} else if err := v.SetSysReg(hvSysRegSP_EL1, stackTop); err != nil {
		return fmt.Errorf("set SP_EL1: %w", err)
	}
	if err := v.SetReg(hvRegX0, deviceTree); err != nil {
		return fmt.Errorf("set X0: %w", err)
	}
	for _, reg := range []Reg{hvRegX1, hvRegX2, hvRegX3} {
		if err := v.SetReg(reg, 0); err != nil {
			return fmt.Errorf("clear reg %d: %w", reg, err)
		}
	}
	return nil
}

func (v *VM) configureNestedEL2ForVCPU(index int) error {
	const (
		hcrEL2RW        = 1 << 31
		cnthctlEL1PCEN  = 1 << 1
		cnthctlEL1PCTEN = 1 << 0
	)
	if err := v.SetSysRegForVCPU(index, hvSysRegHCR_EL2, hcrEL2RW); err != nil {
		return fmt.Errorf("set HCR_EL2: %w", err)
	}
	if err := v.SetSysRegForVCPU(index, hvSysRegCNTHCTL_EL2, cnthctlEL1PCEN|cnthctlEL1PCTEN); err != nil {
		return fmt.Errorf("set CNTHCTL_EL2: %w", err)
	}
	if err := v.SetSysRegForVCPU(index, hvSysRegCNTVOFF_EL2, 0); err != nil {
		return fmt.Errorf("set CNTVOFF_EL2: %w", err)
	}
	if err := v.SetSysRegForVCPU(index, hvSysRegCPTR_EL2, 0); err != nil {
		return fmt.Errorf("set CPTR_EL2: %w", err)
	}
	return nil
}

func DefaultPStateEL1h() uint64 {
	return defaultPState(0x5)
}

func DefaultPStateEL2h() uint64 {
	return defaultPState(0x9)
}

func (v *VM) defaultPState() uint64 {
	if v.nestedVirt {
		return DefaultPStateEL2h()
	}
	return DefaultPStateEL1h()
}

func defaultPState(mode uint64) uint64 {
	const (
		pstateDF = 0x200
		pstateAF = 0x100
		pstateIF = 0x80
		pstateFF = 0x40
	)
	return mode | pstateDF | pstateAF | pstateIF | pstateFF
}

func (v *VM) Close() error {
	var firstErr error
	defer func() {
		activeVM.CompareAndSwap(v, nil)
		vmLifecycleMu.Unlock()
	}()
	if err := v.CancelRun(); err != nil && firstErr == nil {
		firstErr = err
	}
	for _, cpu := range v.vcpus {
		if !cpu.vcpuCreated {
			continue
		}
		errCh := make(chan error, 1)
		err := cpu.callWithTimeout(500*time.Millisecond, func() {
			if ret := hvVcpuDestroy(cpu.vcpu); ret != hvSuccess {
				errCh <- fmt.Errorf("destroy vcpu %d: %w", cpu.index, ret)
				return
			}
			errCh <- nil
		})
		if err != nil && firstErr == nil {
			firstErr = err
			continue
		}
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
		cpu.vcpuCreated = false
		cpu.vcpu = 0
		if cpu.index == 0 {
			v.vcpuCreated = false
			v.vcpu = 0
		}
	}
	v.lifecycleMu.Lock()
	v.memoryClosing = true
	detaches := v.memoryDetach
	v.memoryDetach = nil
	v.lifecycleMu.Unlock()
	for _, detach := range detaches {
		if detach != nil {
			detach()
		}
	}
	v.mappingsMu.Lock()
	mappings := append([]mapping(nil), v.mappings...)
	v.mappings = nil
	v.mappingsMu.Unlock()
	for _, m := range mappings {
		m := m
		errCh := make(chan error, 1)
		v.threadCh <- func() {
			if ret := hvVMUnmap(m.ipa, m.size); ret != hvSuccess {
				errCh <- fmt.Errorf("unmap memory: %w", ret)
				return
			}
			errCh <- nil
		}
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
		if m.anonymous {
			if err := syscall.Munmap(m.mem); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("munmap memory: %w", err)
			}
		}
	}
	errCh := make(chan error, 1)
	v.threadCh <- func() {
		if ret := destroyVMWithRetry(); ret != hvSuccess {
			errCh <- fmt.Errorf("destroy vm: %w", ret)
			return
		}
		errCh <- nil
	}
	if err := <-errCh; err != nil && firstErr == nil {
		firstErr = err
	}
	v.closeThreads()
	return firstErr
}

// RegisterGuestMemoryDetach registers work that must finish before guest
// memory is unmapped. Devices with asynchronous host-side workers use this to
// stop dereferencing virtqueues during VM teardown.
func (v *VM) RegisterGuestMemoryDetach(detach func()) {
	if v == nil || detach == nil {
		return
	}
	v.lifecycleMu.Lock()
	if v.memoryClosing {
		v.lifecycleMu.Unlock()
		detach()
		return
	}
	v.memoryDetach = append(v.memoryDetach, detach)
	v.lifecycleMu.Unlock()
}

type ExceptionClass uint64

const (
	ExceptionClassHVC64            ExceptionClass = 0x16
	ExceptionClassSystemRegister   ExceptionClass = 0x18
	ExceptionClassDataAbortLowerEL ExceptionClass = 0x24
)

type DataAbortInfo struct {
	SizeBytes int
	Write     bool
	Target    Reg
}

type SystemInstructionInfo struct {
	Op0   uint8
	Op1   uint8
	Op2   uint8
	CRn   uint8
	CRm   uint8
	Rt    Reg
	Read  bool
	RawRt uint8
}

func DecodeExceptionClass(syndrome uint64) ExceptionClass {
	return ExceptionClass((syndrome >> 26) & 0x3f)
}

func DecodeDataAbort(syndrome uint64) (DataAbortInfo, error) {
	const (
		dataAbortISSMask uint64 = (1 << 25) - 1
		isvBit                  = 24
		sasShift                = 22
		sasMask          uint64 = 0x3
		srtShift                = 16
		srtMask          uint64 = 0x1f
		wnrBit                  = 6
	)

	iss := syndrome & dataAbortISSMask
	if ((iss >> isvBit) & 0x1) == 0 {
		return DataAbortInfo{}, fmt.Errorf("data abort without ISV set: syndrome=0x%x", syndrome)
	}
	sas := (iss >> sasShift) & sasMask
	if sas > 3 {
		return DataAbortInfo{}, fmt.Errorf("invalid SAS value %d", sas)
	}
	srt := int((iss >> srtShift) & srtMask)
	write := ((iss >> wnrBit) & 0x1) == 1

	var reg Reg
	switch {
	case srt >= 0 && srt <= 31:
		reg = Reg(srt)
		if srt == 31 {
			reg = hvRegXZR
		}
	default:
		return DataAbortInfo{}, fmt.Errorf("unsupported register index %d", srt)
	}

	return DataAbortInfo{
		SizeBytes: 1 << sas,
		Write:     write,
		Target:    reg,
	}, nil
}

func DecodeSystemInstruction(syndrome uint64) (SystemInstructionInfo, error) {
	const (
		systemInstructionISSMask uint64 = (1 << 25) - 1
		ilBit                           = 25
		op0Shift                        = 20
		op0Mask                  uint64 = 0x3
		op2Shift                        = 17
		op2Mask                  uint64 = 0x7
		op1Shift                        = 14
		op1Mask                  uint64 = 0x7
		crnShift                        = 10
		crnMask                  uint64 = 0xf
		rtShift                         = 5
		rtMask                   uint64 = 0x1f
		crmShift                        = 1
		crmMask                  uint64 = 0xf
	)

	if DecodeExceptionClass(syndrome) != ExceptionClassSystemRegister {
		return SystemInstructionInfo{}, fmt.Errorf("not a trapped system instruction: syndrome=%#x", syndrome)
	}
	if ((syndrome >> ilBit) & 0x1) == 0 {
		return SystemInstructionInfo{}, fmt.Errorf("system instruction without IL set: syndrome=%#x", syndrome)
	}

	iss := syndrome & systemInstructionISSMask
	rawRt := uint8((iss >> rtShift) & rtMask)
	rt := Reg(rawRt)
	if rawRt == 31 {
		rt = hvRegXZR
	}

	return SystemInstructionInfo{
		Op0:   uint8((iss >> op0Shift) & op0Mask),
		Op1:   uint8((iss >> op1Shift) & op1Mask),
		Op2:   uint8((iss >> op2Shift) & op2Mask),
		CRn:   uint8((iss >> crnShift) & crnMask),
		CRm:   uint8((iss >> crmShift) & crmMask),
		Rt:    rt,
		Read:  (iss & 0x1) == 1,
		RawRt: rawRt,
	}, nil
}

func (s SystemInstructionInfo) IsDITRegisterAccess() bool {
	return s.Op0 == 0x3 && s.Op1 == 0x3 && s.CRn == 0x4 && s.CRm == 0x2 && s.Op2 == 0x5
}

func (s SystemInstructionInfo) IsDITImmediateAccess() bool {
	return s.Op0 == 0x0 && s.Op1 == 0x3 && s.CRn == 0x4 && s.Op2 == 0x2
}

func (s SystemInstructionInfo) ImmediateValue() uint8 {
	return s.CRm
}

func (s SystemInstructionInfo) IsMDSCREL1Access() bool {
	return s.Op0 == 0x2 && s.Op1 == 0x0 && s.CRn == 0x0 && s.CRm == 0x2 && s.Op2 == 0x2
}

func (s SystemInstructionInfo) IsOSDLREL1Access() bool {
	return s.Op0 == 0x2 && s.Op1 == 0x0 && s.CRn == 0x1 && s.CRm == 0x3 && s.Op2 == 0x4
}

func (s SystemInstructionInfo) IsOSLAREL1Access() bool {
	return s.Op0 == 0x2 && s.Op1 == 0x0 && s.CRn == 0x1 && s.CRm == 0x0 && s.Op2 == 0x4
}

func (s SystemInstructionInfo) IsOSLSREL1Access() bool {
	return s.Op0 == 0x2 && s.Op1 == 0x0 && s.CRn == 0x1 && s.CRm == 0x1 && s.Op2 == 0x4
}

func (v *VM) GetProgramCounter() (uint64, error) {
	return v.GetProgramCounterForVCPU(0)
}

func (v *VM) GetProgramCounterForVCPU(index int) (uint64, error) {
	return v.GetRegForVCPU(index, hvRegPC)
}

func (v *VM) HandleSystemInstruction(syndrome uint64) (bool, error) {
	return v.HandleSystemInstructionForVCPU(0, syndrome)
}

func (v *VM) HandleSystemInstructionForVCPU(index int, syndrome uint64) (bool, error) {
	info, err := DecodeSystemInstruction(syndrome)
	if err != nil {
		return false, err
	}

	switch {
	case info.IsDITRegisterAccess():
		if info.Read {
			var value uint64
			if v.dit {
				value = 1 << 24
			}
			if info.Rt != hvRegXZR {
				if err := v.SetRegForVCPU(index, info.Rt, value); err != nil {
					return false, err
				}
			}
		} else {
			var value uint64
			if info.Rt != hvRegXZR {
				value, err = v.GetRegForVCPU(index, info.Rt)
				if err != nil {
					return false, err
				}
			}
			v.dit = ((value >> 24) & 0x1) == 1
		}
	case info.IsDITImmediateAccess():
		v.dit = (info.ImmediateValue() & 0x1) == 1
	case info.IsMDSCREL1Access():
		if info.Read {
			if info.Rt != hvRegXZR {
				if err := v.SetRegForVCPU(index, info.Rt, v.mdscrEL1); err != nil {
					return false, err
				}
			}
		} else {
			if info.Rt == hvRegXZR {
				v.mdscrEL1 = 0
			} else {
				value, err := v.GetRegForVCPU(index, info.Rt)
				if err != nil {
					return false, err
				}
				v.mdscrEL1 = value
			}
		}
	case info.IsOSDLREL1Access():
		if info.Read {
			if info.Rt != hvRegXZR {
				if err := v.SetRegForVCPU(index, info.Rt, v.osdlrEL1); err != nil {
					return false, err
				}
			}
		} else {
			if info.Rt == hvRegXZR {
				v.osdlrEL1 = 0
			} else {
				value, err := v.GetRegForVCPU(index, info.Rt)
				if err != nil {
					return false, err
				}
				v.osdlrEL1 = value
			}
		}
	case info.IsOSLAREL1Access():
		if info.Read {
			if info.Rt != hvRegXZR {
				if err := v.SetRegForVCPU(index, info.Rt, 0); err != nil {
					return false, err
				}
			}
		} else {
			var value uint64
			if info.Rt != hvRegXZR {
				value, err = v.GetRegForVCPU(index, info.Rt)
				if err != nil {
					return false, err
				}
			}
			v.osLock = (value & 0x1) == 1
		}
	case info.IsOSLSREL1Access():
		value := uint64(0x8)
		if v.osLock {
			value |= 0x2
		}
		if info.Rt != hvRegXZR {
			if err := v.SetRegForVCPU(index, info.Rt, value); err != nil {
				return false, err
			}
		}
	case v.nestedVirt && info.IsEL2SystemRegisterAccess():
		handled, err := v.handleNestedEL2SystemRegisterForVCPU(index, info)
		if err != nil || !handled {
			return handled, err
		}
	default:
		return false, nil
	}

	return true, v.AdvanceProgramCounterForVCPU(index)
}

func (s SystemInstructionInfo) IsEL2SystemRegisterAccess() bool {
	return s.Op0 == 0x3 && s.Op1 == 0x4
}

func (s SystemInstructionInfo) SysReg() SysReg {
	return SysReg(uint16(s.Op0)<<14 |
		uint16(s.Op1)<<11 |
		uint16(s.CRn)<<7 |
		uint16(s.CRm)<<3 |
		uint16(s.Op2))
}

func (v *VM) handleNestedEL2SystemRegisterForVCPU(index int, info SystemInstructionInfo) (bool, error) {
	reg := info.SysReg()
	if info.Read {
		value, err := v.GetSysRegForVCPU(index, reg)
		if err != nil {
			if strings.Contains(err.Error(), "unsupported") {
				return false, nil
			}
			return false, err
		}
		if info.Rt != hvRegXZR {
			if err := v.SetRegForVCPU(index, info.Rt, value); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	var value uint64
	if info.Rt != hvRegXZR {
		var err error
		value, err = v.GetRegForVCPU(index, info.Rt)
		if err != nil {
			return false, err
		}
	}
	if err := v.SetSysRegForVCPU(index, reg, value); err != nil {
		if strings.Contains(err.Error(), "unsupported") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (v *VM) AdvanceProgramCounter() error {
	return v.AdvanceProgramCounterForVCPU(0)
}

func (v *VM) AdvanceProgramCounterForVCPU(index int) error {
	pc, err := v.GetRegForVCPU(index, hvRegPC)
	if err != nil {
		return err
	}
	return v.SetRegForVCPU(index, hvRegPC, pc+4)
}

func (v *VM) ReadIPA(addr uint64, size int) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid read size %d", size)
	}
	data := make([]byte, size)
	if err := v.ReadIPAInto(addr, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (v *VM) ReadIPAInto(addr uint64, dst []byte) error {
	if v.fastMem != nil && addr >= v.fastMemBase && addr+uint64(len(dst)) <= v.fastMemEnd {
		off := addr - v.fastMemBase
		copy(dst, v.fastMem[off:off+uint64(len(dst))])
		return nil
	}
	v.mappingsMu.RLock()
	defer v.mappingsMu.RUnlock()
	for _, m := range v.mappings {
		start := uint64(m.ipa)
		end := start + uint64(m.size)
		if addr < start || addr+uint64(len(dst)) > end {
			continue
		}
		off := addr - start
		copy(dst, m.mem[off:off+uint64(len(dst))])
		return nil
	}
	return fmt.Errorf("read guest memory %#x size %d: unmapped", addr, len(dst))
}

func (v *VM) SliceIPA(addr uint64, size int) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid slice size %d", size)
	}
	if v.fastMem != nil && addr >= v.fastMemBase && addr+uint64(size) <= v.fastMemEnd {
		off := addr - v.fastMemBase
		return v.fastMem[off : off+uint64(size)], nil
	}
	v.mappingsMu.RLock()
	defer v.mappingsMu.RUnlock()
	for _, m := range v.mappings {
		start := uint64(m.ipa)
		end := start + uint64(m.size)
		if addr < start || addr+uint64(size) > end {
			continue
		}
		off := addr - start
		return m.mem[off : off+uint64(size)], nil
	}
	return nil, fmt.Errorf("slice guest memory %#x size %d: unmapped", addr, size)
}

func (v *VM) ReclaimGuestPage(ipa uint64) error {
	page, err := v.SliceIPA(ipa, 4096)
	if err != nil {
		return err
	}
	if uintptr(unsafe.Pointer(&page[0]))%4096 != 0 {
		return fmt.Errorf("reclaim guest page %#x: host mapping is not page-aligned", ipa)
	}
	if err := unix.Madvise(page, unix.MADV_FREE); err != nil {
		return fmt.Errorf("reclaim guest page %#x: %w", ipa, err)
	}
	return nil
}

func (v *VM) ReuseGuestPage(ipa uint64) error {
	return nil
}

func (v *VM) WriteIPA(addr uint64, data []byte) error {
	if v.fastMem != nil && addr >= v.fastMemBase && addr+uint64(len(data)) <= v.fastMemEnd {
		off := addr - v.fastMemBase
		copy(v.fastMem[off:off+uint64(len(data))], data)
		return nil
	}
	v.mappingsMu.RLock()
	defer v.mappingsMu.RUnlock()
	for _, m := range v.mappings {
		start := uint64(m.ipa)
		end := start + uint64(m.size)
		if addr < start || addr+uint64(len(data)) > end {
			continue
		}
		off := addr - start
		copy(m.mem[off:off+uint64(len(data))], data)
		return nil
	}
	return fmt.Errorf("write guest memory %#x size %d: unmapped", addr, len(data))
}

const gicSPIBase = 32

func (v *VM) SetIRQ(irq uint32, level bool) error {
	if irq >= 1020 {
		return fmt.Errorf("irq %d out of range", irq)
	}
	intid := irq + gicSPIBase
	if level {
		_ = v.CancelRun()
	}
	v.gicMu.Lock()
	defer v.gicMu.Unlock()
	if ret := hvGICSetSPI(intid, level); ret != hvSuccess {
		return fmt.Errorf("set gic spi %d level=%v: %w", intid, level, ret)
	}
	return nil
}
