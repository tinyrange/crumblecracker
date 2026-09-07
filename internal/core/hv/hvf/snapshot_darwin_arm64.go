//go:build darwin && arm64

package hvf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/hv/snapshotstore"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const snapshotTriggerMagic = 0x43535833534e4150

type snapshotTrigger struct {
	base    uint64
	size    uint64
	dir     string
	mem     []byte
	mmio    *snapshotMMIORecorder
	devices snapshotDevices

	once sync.Once
	err  error
}

type snapshotDevices struct {
	console *virtio.Console
	rng     *virtio.RNG
	balloon *virtio.Balloon
	vsock   *virtio.Vsock
	fsdevs  []*virtio.FS
	netdev  *virtio.Net
}

type snapshotMMIORecorder struct {
	mu     sync.Mutex
	writes []snapshotMMIOWrite
}

type snapshotMMIOWrite struct {
	Addr  uint64 `json:"addr"`
	Size  int    `json:"size"`
	Value uint64 `json:"value"`
}

type snapshotManifest struct {
	Format           string                      `json:"format"`
	Partial          bool                        `json:"partial"`
	CapturedAt       string                      `json:"captured_at"`
	TriggerBase      uint64                      `json:"trigger_base"`
	TriggerValue     uint64                      `json:"trigger_value"`
	MemoryBase       uint64                      `json:"memory_base"`
	MemorySize       uint64                      `json:"memory_size"`
	MemoryFile       string                      `json:"memory_file"`
	CapturedVCPU     int                         `json:"captured_vcpu"`
	VCPURegisters    map[string]uint64           `json:"vcpu_registers"`
	VCPUSysRegisters map[string]uint64           `json:"vcpu_sys_registers"`
	VTimerMasked     bool                        `json:"vtimer_masked"`
	VTimerOffset     uint64                      `json:"vtimer_offset"`
	GICDistributor   map[string]uint64           `json:"gic_distributor,omitempty"`
	GICRedistributor map[string]uint64           `json:"gic_redistributor,omitempty"`
	GICICC           map[string]uint64           `json:"gic_icc,omitempty"`
	Devices          map[string]virtio.MMIOState `json:"devices,omitempty"`
	MMIOWrites       []snapshotMMIOWrite         `json:"mmio_writes"`
	Note             string                      `json:"note"`
}

func StartContainerFromSnapshot(ctx context.Context, req ContainerRunRequest, snapshotPath string, onEvent func(client.BootEvent) error) (*ContainerSession, error) {
	startTotal := time.Now()
	start := time.Now()
	manifest, mem, err := loadSnapshot(snapshotPath)
	if err != nil {
		return nil, err
	}
	if err := validateSnapshotRequest(manifest, req); err != nil {
		_ = syscall.Munmap(mem)
		return nil, err
	}
	timing.Since(ctx, "hvf.restore.load_snapshot", start)
	if allocated, err := snapshotMemoryAllocatedBytes(snapshotMemoryPath(snapshotPath, manifest.MemoryFile)); err == nil {
		timingLog("hvf.restore load snapshot took=%s logical=%d allocated=%d", time.Since(start), len(mem), allocated)
	} else {
		timingLog("hvf.restore load snapshot took=%s logical=%d", time.Since(start), len(mem))
	}
	if req.CPUs <= 0 {
		req.CPUs = 1
	}
	user := strings.TrimSpace(req.User)
	if user == "" && req.Image != nil {
		user = strings.TrimSpace(req.Image.Config.User)
	}
	if err := validateGuestUser(user); err != nil {
		return nil, err
	}
	workDir := req.WorkDir
	if workDir == "" && req.Image != nil {
		workDir = req.Image.Config.WorkingDir
	}
	if workDir == "" {
		workDir = "/"
	}
	var baseEnv []string
	if req.Image != nil {
		baseEnv = append([]string(nil), req.Image.Config.Env...)
	}
	baseEnv = vmruntime.WithDefaultEnv(vmruntime.MergeEnv(baseEnv, req.Env))

	start = time.Now()
	vm, err := NewVMWithOptions(ctx, VMOptions{CPUs: req.CPUs, NestedVirt: req.NestedVirt})
	if err != nil {
		return nil, err
	}
	timing.Since(ctx, "hvf.restore.new_vm", start)
	timingLog("hvf.restore NewVM took=%s", time.Since(start))
	start = time.Now()
	if err := vm.MapMemory(mem, IPA(manifest.MemoryBase), hvMemoryRead|hvMemoryWrite|hvMemoryExec); err != nil {
		_ = syscall.Munmap(mem)
		vm.Close()
		return nil, fmt.Errorf("map snapshot memory: %w", err)
	}
	vm.markLastMappingOwned()
	timing.Since(ctx, "hvf.restore.map_memory", start)
	timingLog("hvf.restore map memory took=%s", time.Since(start))

	start = time.Now()
	var serialOut = newSerialTranscript()
	var serialWriter io.Writer = serialOut
	var bootWriter *bootEventWriter
	if onEvent != nil && req.Dmesg {
		bootWriter = newBootEventWriter(onEvent)
		serialWriter = io.MultiWriter(serialOut, bootWriter)
		defer bootWriter.Close()
	}
	var consoleOut bytes.Buffer
	var fsTrace bytes.Buffer
	var runTrace bytes.Buffer
	var uart *serial.UART8250
	if req.Dmesg {
		uart = serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, serialWriter)
		uart.AttachIRQ(vm, arm64vm.UARTSPI)
	}
	console := virtio.NewConsole(arm64vm.ConsoleBase, arm64vm.ConsoleSize, arm64vm.ConsoleIRQ, &consoleOut)
	console.Attach(vm, vm)
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)
	rng.Attach(vm, vm)
	balloon := virtio.NewBalloon(arm64vm.BalloonBase, arm64vm.BalloonSize, arm64vm.BalloonIRQ)
	balloon.Attach(vm, vm)
	var netdev *virtio.Net
	if req.NetDevice != nil {
		netdev = req.NetDevice
		netdev.Attach(vm, vm)
	}
	vsockBackend := virtio.NewSimpleVsockBackend()
	listener, err := vsockBackend.Listen(vmruntime.ControlPort)
	if err != nil {
		vm.Close()
		return nil, fmt.Errorf("listen vsock control: %w", err)
	}
	vsock := virtio.NewVsock(arm64vm.VsockBase, arm64vm.VsockSize, arm64vm.VsockIRQ, vmruntime.GuestCID, vsockBackend)
	vsock.Attach(vm, vm)
	fsdevs, rootFS, err := arm64vm.BuildFSDevices(req, &fsTrace)
	if err != nil {
		_ = listener.Close()
		vm.Close()
		return nil, err
	}
	attachFSDeviceTiming(ctx, fsdevs)
	for _, fsdev := range fsdevs {
		fsdev.Async = false
	}
	for _, fsdev := range fsdevs {
		fsdev.Attach(vm, vm)
	}
	timing.Since(ctx, "hvf.restore.device_setup", start)
	timingLog("hvf.restore device setup took=%s fsdevs=%d", time.Since(start), len(fsdevs))
	start = time.Now()
	if len(manifest.Devices) > 0 {
		if err := restoreDeviceStates(manifest.Devices, console, rng, balloon, balloonTargetPages(req.BalloonMB), fsdevs, vsock, netdev); err != nil {
			_ = listener.Close()
			vm.Close()
			return nil, err
		}
	} else {
		if err := replaySnapshotMMIO(vm, manifest.MMIOWrites, uart, console, rng, balloon, fsdevs, vsock, netdev); err != nil {
			_ = listener.Close()
			vm.Close()
			return nil, err
		}
	}
	timing.Since(ctx, "hvf.restore.replay_mmio", start)
	timingLog("hvf.restore device state took=%s writes=%d explicit=%t", time.Since(start), len(manifest.MMIOWrites), len(manifest.Devices) > 0)
	start = time.Now()
	if err := restoreSnapshotGIC(vm, manifest); err != nil {
		_ = listener.Close()
		vm.Close()
		return nil, err
	}
	if err := restoreSnapshotVTimer(vm, manifest); err != nil {
		_ = listener.Close()
		vm.Close()
		return nil, err
	}
	if err := restoreSnapshotRegisters(vm, manifest); err != nil {
		_ = listener.Close()
		vm.Close()
		return nil, err
	}
	timing.Since(ctx, "hvf.restore.restore_state", start)
	timingLog("hvf.restore restore state took=%s", time.Since(start))

	runCtx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan error, 1)
	doneCh := make(chan sessionRunResult, 1)
	closeDone := make(chan struct{})
	controlTranscript := newSerialTranscript()
	controlAcceptCh := make(chan readyResult, 1)
	controlConnCh := make(chan readyResult, 1)
	activeExecs := &atomic.Int32{}
	guestReady := &atomic.Bool{}
	sendReady := func(err error) {
		select {
		case readyCh <- err:
		default:
		}
	}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			controlAcceptCh <- readyResult{err: err}
			return
		}
		go func() {
			_, _ = io.Copy(controlTranscript, conn)
		}()
		controlAcceptCh <- readyResult{conn: conn}
	}()
	go func() {
		select {
		case res := <-controlAcceptCh:
			if res.err != nil {
				sendReady(res.err)
				return
			}
			text, err := controlTranscript.WaitFor(runCtx, 0, func(text string) bool {
				return strings.Contains(text, instanceReadyMarker)
			})
			if err != nil {
				_ = res.conn.Close()
				sendReady(err)
				return
			}
			_ = text
			guestReady.Store(true)
			controlConnCh <- res
			sendReady(nil)
		case <-runCtx.Done():
			sendReady(runCtx.Err())
		}
	}()
	go func() {
		defer close(closeDone)
		defer func() {
			_ = vm.Close()
			exitTiming.Dump()
		}()
		defer cancel()
		runner := newVMRunManager(vm)
		for {
			active := activeExecs.Load() > 0
			runStart := time.Now()
			runRes, err, stalled := runner.Run(runCtx, persistentRunSlice(guestReady.Load(), active))
			timing.Since(ctx, "hvf.restore.run_loop.run_with_cancel", runStart)
			if stalled {
				if runCtx.Err() != nil {
					doneCh <- sessionRunResult{err: runCtx.Err()}
					return
				}
				continue
			}
			if err != nil {
				doneCh <- sessionRunResult{err: fmt.Errorf("%w\n%s\nrun:\n%sserial:\n%s\nvirtio-fs:\n%s", err, vsock.Summary(), runTrace.String(), serialOut.String(), fsTrace.String())}
				return
			}
			if runRes == nil || runRes.exit == nil {
				doneCh <- sessionRunResult{err: fmt.Errorf("vcpu returned nil exit info")}
				return
			}
			exitInfo := runRes.exit
			vcpuIndex := runRes.index
			if exitInfo.Reason == hvExitReasonVTimerActivated {
				if err := injectVirtualTimerPPI(vm, vcpuIndex); err != nil {
					doneCh <- sessionRunResult{err: fmt.Errorf("inject virtual timer ppi: %w", err)}
					return
				}
				continue
			}
			if exitInfo.Reason == hvExitReasonCanceled {
				continue
			}
			if exitInfo.Reason != hvExitReasonException {
				doneCh <- sessionRunResult{err: fmt.Errorf("unexpected exit reason %v", exitInfo.Reason)}
				return
			}
			switch DecodeExceptionClass(exitInfo.Exception.Syndrome) {
			case ExceptionClassDataAbortLowerEL:
				if err := handleContainerDataAbort(ctx, vm, vcpuIndex, uart, console, rng, balloon, fsdevs, vsock, netdev, nil, nil, nil, exitInfo); err != nil {
					doneCh <- sessionRunResult{err: err}
					return
				}
			case ExceptionClassSystemRegister:
				handled, err := vm.HandleSystemInstructionForVCPU(vcpuIndex, exitInfo.Exception.Syndrome)
				if err != nil {
					doneCh <- sessionRunResult{err: err}
					return
				}
				if !handled {
					doneCh <- sessionRunResult{err: fmt.Errorf("unsupported system instruction trap")}
					return
				}
			case ExceptionClassHVC64:
				halt, err := handleContainerHVC(vm, vcpuIndex)
				if err != nil {
					doneCh <- sessionRunResult{err: err}
					return
				}
				if halt {
					doneCh <- sessionRunResult{}
					return
				}
			default:
				pc, _ := vm.GetProgramCounterForVCPU(vcpuIndex)
				doneCh <- sessionRunResult{err: fmt.Errorf("unexpected exception class %#x pc=%#x syndrome=%#x physical=%#x\n%s\nserial:\n%s\nvirtio-fs:\n%s",
					DecodeExceptionClass(exitInfo.Exception.Syndrome), pc, exitInfo.Exception.Syndrome, uint64(exitInfo.Exception.PhysicalAddress), vsock.Summary(), serialOut.String(), fsTrace.String())}
				return
			}
		}
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			cancel()
			_ = listener.Close()
			res := <-doneCh
			<-closeDone
			if res.err != nil {
				return nil, res.err
			}
			return nil, err
		}
		res, ok := <-controlConnCh
		if !ok || res.err != nil || res.conn == nil {
			cancel()
			_ = listener.Close()
			resDone := <-doneCh
			<-closeDone
			if resDone.err != nil {
				return nil, resDone.err
			}
			if res.err != nil {
				return nil, res.err
			}
			return nil, fmt.Errorf("guest control connection became ready without an accepted vsock connection")
		}
		_ = listener.Close()
		shareState := make(map[string]client.ShareMount, len(req.Shares))
		for _, share := range req.Shares {
			shareState[strings.TrimSpace(share.Mount)] = client.ShareMount{
				Source: share.Source, Mount: share.Mount, Writable: share.Writable,
				MapOwner: share.MapOwner, OwnerUID: share.OwnerUID, OwnerGID: share.OwnerGID, Cache: share.Cache,
			}
		}
		session := &ContainerSession{
			cancel: cancel, runCtx: runCtx, doneCh: doneCh, closeDone: closeDone, image: req.Image, baseEnv: baseEnv, workDir: workDir,
			dmesg: req.Dmesg, uart: uart, control: res.conn, transcript: controlTranscript, serialOut: serialOut,
			listener: listener, vsock: vsock, rootFS: rootFS, fsdevs: fsdevs, shares: shareState,
			balloon:     balloon,
			imageMounts: map[string]string{}, activeExecs: activeExecs,
		}
		timing.Since(ctx, "hvf.restore.total_ready", startTotal)
		timingLog("hvf.restore total ready=%s", time.Since(startTotal))
		return session, nil
	case res := <-doneCh:
		cancel()
		_ = listener.Close()
		<-closeDone
		if res.err != nil {
			return nil, res.err
		}
		return nil, fmt.Errorf("guest exited before control connection became ready")
	case <-ctx.Done():
		cancel()
		_ = listener.Close()
		res := <-doneCh
		<-closeDone
		if res.err != nil {
			return nil, res.err
		}
		return nil, ctx.Err()
	}
}

func newSnapshotTrigger(dir string, mem []byte, mmio *snapshotMMIORecorder) *snapshotTrigger {
	return &snapshotTrigger{
		base: arm64vm.SnapshotBase,
		size: arm64vm.SnapshotSize,
		dir:  dir,
		mem:  mem,
		mmio: mmio,
	}
}

func (s *snapshotTrigger) setDevices(devices snapshotDevices) {
	if s == nil {
		return
	}
	s.devices = devices
}

func newSnapshotMMIORecorder() *snapshotMMIORecorder {
	return &snapshotMMIORecorder{}
}

func (r *snapshotMMIORecorder) record(addr uint64, size int, value uint64) {
	if r == nil || size <= 0 {
		return
	}
	r.mu.Lock()
	r.writes = append(r.writes, snapshotMMIOWrite{Addr: addr, Size: size, Value: value})
	r.mu.Unlock()
}

func (r *snapshotMMIORecorder) snapshot() []snapshotMMIOWrite {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]snapshotMMIOWrite(nil), r.writes...)
}

func (s *snapshotTrigger) contains(addr uint64, size int) bool {
	return s != nil && addr >= s.base && addr+uint64(size) <= s.base+s.size
}

func (s *snapshotTrigger) handleDataAbort(vm *VM, vcpuIndex int, info DataAbortInfo, value uint64) error {
	if !info.Write {
		if err := writeAbortValue(vm, vcpuIndex, info, s.readyValue()); err != nil {
			return err
		}
		return vm.AdvanceProgramCounterForVCPU(vcpuIndex)
	}
	if err := vm.AdvanceProgramCounterForVCPU(vcpuIndex); err != nil {
		return err
	}
	s.once.Do(func() {
		s.err = s.capture(vm, vcpuIndex, value)
	})
	if s.err != nil {
		fmt.Fprintf(os.Stderr, "ccx3: startup snapshot capture failed: %v\n", s.err)
	}
	return nil
}

func (s *snapshotTrigger) readyValue() uint64 {
	if s != nil && s.devices.balloon != nil && !s.devices.balloon.AtTarget() {
		return 0
	}
	return snapshotTriggerMagic
}

func (s *snapshotTrigger) capture(vm *VM, vcpuIndex int, value uint64) error {
	if s == nil || s.dir == "" {
		timingLog("snapshot trigger captured vcpu=%d value=%#x without file dump", vcpuIndex, value)
		return nil
	}
	capture, err := snapshotstore.Begin(s.dir)
	if err != nil {
		return err
	}
	defer capture.Abort()
	outDir := capture.Dir()
	memoryPath := filepath.Join(outDir, "memory.bin")
	memoryStarted := time.Now()
	if err := writeSparseSnapshotMemory(memoryPath, s.mem, 0o600); err != nil {
		return fmt.Errorf("write snapshot memory: %w", err)
	}
	if allocated, err := snapshotMemoryAllocatedBytes(memoryPath); err == nil {
		timingLog("hvf.snapshot memory capture took=%s logical=%d allocated=%d", time.Since(memoryStarted), len(s.mem), allocated)
	} else {
		timingLog("hvf.snapshot memory capture took=%s logical=%d", time.Since(memoryStarted), len(s.mem))
	}
	manifest := snapshotManifest{
		Format:       "ccx3-hvf-snapshot-v0",
		Partial:      true,
		CapturedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		TriggerBase:  s.base,
		TriggerValue: value,
		MemoryBase:   arm64vm.MemoryBase,
		MemorySize:   uint64(len(s.mem)),
		MemoryFile:   "memory.bin",
		CapturedVCPU: vcpuIndex,
		Note:         "Initial checkpoint capture. Restore still needs full vCPU, GIC, timer, and device model serialization.",
	}
	manifest.VCPURegisters = snapshotVCPURegisters(vm, vcpuIndex)
	manifest.VCPUSysRegisters = snapshotVCPUSysRegisters(vm, vcpuIndex)
	manifest.VTimerMasked, manifest.VTimerOffset = snapshotVTimer(vm)
	manifest.GICDistributor = snapshotGICDistributor(vm)
	manifest.GICRedistributor = snapshotGICRedistributor(vm, vcpuIndex)
	manifest.GICICC = snapshotGICICC(vm, vcpuIndex)
	manifest.Devices = snapshotDeviceStates(s.devices)
	manifest.MMIOWrites = s.mmio.snapshot()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644); err != nil {
		return fmt.Errorf("write snapshot manifest: %w", err)
	}
	final, err := capture.Publish("memory.bin", "manifest.json")
	if err != nil {
		return err
	}
	timingLog("snapshot trigger wrote %s", final)
	return nil
}

func snapshotVCPURegisters(vm *VM, vcpuIndex int) map[string]uint64 {
	regs := make(map[string]uint64, 34)
	for i := 0; i <= 30; i++ {
		value, err := vm.GetRegForVCPU(vcpuIndex, Reg(i))
		if err != nil {
			continue
		}
		regs[fmt.Sprintf("x%d", i)] = value
	}
	for name, reg := range map[string]Reg{
		"pc":   hvRegPC,
		"cpsr": hvRegCPSR,
	} {
		value, err := vm.GetRegForVCPU(vcpuIndex, reg)
		if err != nil {
			continue
		}
		regs[name] = value
	}
	return regs
}

func snapshotVCPUSysRegisters(vm *VM, vcpuIndex int) map[string]uint64 {
	regs := map[string]SysReg{
		"amair_el1":      hvSysRegAMAIR_EL1,
		"contextidr_el1": hvSysRegCONTEXTIDR_EL1,
		"cntfrq_el0":     hvSysRegCNTFRQ_EL0,
		"cntp_ctl_el0":   hvSysRegCNTP_CTL_EL0,
		"cntp_cval_el0":  hvSysRegCNTP_CVAL_EL0,
		"cntp_tval_el0":  hvSysRegCNTP_TVAL_EL0,
		"cntv_ctl_el0":   hvSysRegCNTV_CTL_EL0,
		"cntv_cval_el0":  hvSysRegCNTV_CVAL_EL0,
		"cntv_tval_el0":  hvSysRegCNTV_TVAL_EL0,
		"cntvoff_el2":    hvSysRegCNTVOFF_EL2,
		"cpacr_el1":      hvSysRegCPACR_EL1,
		"elr_el1":        hvSysRegELR_EL1,
		"esr_el1":        hvSysRegESR_EL1,
		"far_el1":        hvSysRegFAR_EL1,
		"mair_el1":       hvSysRegMAIR_EL1,
		"sctlr_el1":      hvSysRegSCTLR_EL1,
		"sp_el0":         hvSysRegSP_EL0,
		"sp_el1":         hvSysRegSP_EL1,
		"sp_el2":         hvSysRegSP_EL2,
		"spsr_el1":       hvSysRegSPSR_EL1,
		"tcr_el1":        hvSysRegTCR_EL1,
		"tpidr_el0":      hvSysRegTPIDR_EL0,
		"tpidr_el1":      hvSysRegTPIDR_EL1,
		"tpidrro_el0":    hvSysRegTPIDRRO_EL0,
		"ttbr0_el1":      hvSysRegTTBR0EL1,
		"ttbr1_el1":      hvSysRegTTBR1EL1,
		"vbar_el1":       hvSysRegVBAR_EL1,
	}
	out := make(map[string]uint64, len(regs))
	for name, reg := range regs {
		value, err := vm.GetSysRegForVCPU(vcpuIndex, reg)
		if err != nil {
			continue
		}
		out[name] = value
	}
	return out
}

func snapshotVTimer(vm *VM) (bool, uint64) {
	masked, err := vm.GetVTimerMask()
	if err != nil {
		timingLog("snapshot vtimer mask failed: %v", err)
	}
	offset, err := vm.GetVTimerOffset()
	if err != nil {
		timingLog("snapshot vtimer offset failed: %v", err)
	}
	return masked, offset
}

func snapshotGICDistributor(vm *VM) map[string]uint64 {
	var regs []GICDistributorReg
	regs = append(regs,
		0x000,        // GICD_CTLR
		0x084, 0x088, // GICD_IGROUPR for SPIs 32-95
		0x104, 0x108, // GICD_ISENABLER for SPIs 32-95
		0x204, 0x208, // GICD_ISPENDR for SPIs 32-95
		0x304, 0x308, // GICD_ISACTIVER for SPIs 32-95
	)
	for reg := GICDistributorReg(0x420); reg <= 0x45c; reg += 4 {
		regs = append(regs, reg) // GICD_IPRIORITYR for SPIs 32-95
	}
	for reg := GICDistributorReg(0xc08); reg <= 0xc1c; reg += 4 {
		regs = append(regs, reg) // GICD_ICFGR for SPIs 32-95
	}
	regs = append(regs, 0xd04, 0xd08) // GICD_IGRPMODR for SPIs 32-95
	for intid := 32; intid < 96; intid++ {
		regs = append(regs, GICDistributorReg(0x6100+intid*8)) // GICD_IROUTER
	}
	return snapshotGICDistributorRegs(vm, regs)
}

func snapshotGICDistributorRegs(vm *VM, regs []GICDistributorReg) map[string]uint64 {
	out := make(map[string]uint64, len(regs))
	for _, reg := range regs {
		value, err := vm.GetGICDistributorReg(reg)
		if err != nil {
			continue
		}
		out[gicRegKey(uint64(reg))] = value
	}
	return out
}

func snapshotGICRedistributor(vm *VM, vcpuIndex int) map[string]uint64 {
	var regs []GICRedistributorReg
	regs = append(regs,
		0x00000,          // GICR_CTLR
		0x00014,          // GICR_WAKER
		0x10080,          // GICR_IGROUPR0
		0x10100,          // GICR_ISENABLER0
		0x10200,          // GICR_ISPENDR0
		0x10300,          // GICR_ISACTIVER0
		0x10c00, 0x10c04, // GICR_ICFGR0/1
	)
	for reg := GICRedistributorReg(0x10400); reg <= 0x1041c; reg += 4 {
		regs = append(regs, reg) // GICR_IPRIORITYR
	}
	out := make(map[string]uint64, len(regs))
	for _, reg := range regs {
		value, err := vm.GetGICRedistributorRegForVCPU(vcpuIndex, reg)
		if err != nil {
			continue
		}
		out[gicRegKey(uint64(reg))] = value
	}
	return out
}

func snapshotGICICC(vm *VM, vcpuIndex int) map[string]uint64 {
	regs := map[string]GICICCReg{
		"ctlr_el1":    hvGICICCRegCTLR_EL1,
		"igrpen0_el1": hvGICICCRegIGRPEN0_EL1,
		"igrpen1_el1": hvGICICCRegIGRPEN1_EL1,
		"pmr_el1":     hvGICICCRegPMR_EL1,
		"sre_el1":     hvGICICCRegSRE_EL1,
	}
	out := make(map[string]uint64, len(regs))
	for name, reg := range regs {
		value, err := vm.GetGICICCRegForVCPU(vcpuIndex, reg)
		if err != nil {
			continue
		}
		out[name] = value
	}
	return out
}

func gicRegKey(reg uint64) string {
	return fmt.Sprintf("0x%x", reg)
}

func snapshotDeviceStates(devices snapshotDevices) map[string]virtio.MMIOState {
	out := make(map[string]virtio.MMIOState)
	if devices.console != nil {
		out["console"] = devices.console.SnapshotState()
	}
	if devices.rng != nil {
		out["rng"] = devices.rng.SnapshotState()
	}
	if devices.balloon != nil {
		out["balloon"] = devices.balloon.SnapshotState()
	}
	if devices.vsock != nil {
		out["vsock"] = devices.vsock.SnapshotState()
	}
	if devices.netdev != nil {
		out["net"] = devices.netdev.SnapshotState()
	}
	for _, fsdev := range devices.fsdevs {
		if fsdev == nil {
			continue
		}
		out[fsDeviceStateKey(fsdev)] = fsdev.SnapshotState()
	}
	return out
}

func restoreDeviceStates(states map[string]virtio.MMIOState, console *virtio.Console, rng *virtio.RNG, balloon *virtio.Balloon, balloonTarget uint32, fsdevs []*virtio.FS, vsock *virtio.Vsock, netdev *virtio.Net) error {
	if state, ok := states["console"]; ok && console != nil {
		if err := console.RestoreState(state); err != nil {
			return fmt.Errorf("restore console state: %w", err)
		}
	}
	if state, ok := states["rng"]; ok && rng != nil {
		if err := rng.RestoreState(state); err != nil {
			return fmt.Errorf("restore rng state: %w", err)
		}
	}
	if state, ok := states["balloon"]; ok && balloon != nil {
		if err := balloon.RestoreState(state); err != nil {
			return fmt.Errorf("restore balloon state: %w", err)
		}
	}
	if balloon != nil {
		if err := balloon.SetTargetPages(balloonTarget); err != nil {
			return fmt.Errorf("retarget restored balloon: %w", err)
		}
	}
	if state, ok := states["vsock"]; ok && vsock != nil {
		if err := vsock.RestoreState(state); err != nil {
			return fmt.Errorf("restore vsock state: %w", err)
		}
	}
	if state, ok := states["net"]; ok && netdev != nil {
		if err := netdev.RestoreState(state); err != nil {
			return fmt.Errorf("restore net state: %w", err)
		}
	}
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		state, ok := states[fsDeviceStateKey(fsdev)]
		if !ok {
			return fmt.Errorf("snapshot missing state for fs device %#x", fsdev.Base)
		}
		if err := fsdev.RestoreState(state); err != nil {
			return fmt.Errorf("restore fs device %#x state: %w", fsdev.Base, err)
		}
	}
	return nil
}

func fsDeviceStateKey(fsdev *virtio.FS) string {
	return fmt.Sprintf("fs:%x", fsdev.Base)
}

func loadSnapshot(path string) (snapshotManifest, []byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return snapshotManifest{}, nil, fmt.Errorf("snapshot path is required")
	}
	manifestPath := path
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		manifestPath = filepath.Join(path, "manifest.json")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	memPath, err := vmruntime.ResolveSnapshotMemoryPath(manifestPath, manifest.MemoryFile)
	if err != nil {
		return snapshotManifest{}, nil, err
	}
	memFile, err := os.Open(memPath)
	if err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("open snapshot memory: %w", err)
	}
	defer memFile.Close()
	info, err := memFile.Stat()
	if err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("stat snapshot memory: %w", err)
	}
	if info.Size() <= 0 {
		return snapshotManifest{}, nil, fmt.Errorf("snapshot memory is empty")
	}
	if manifest.MemorySize != 0 && uint64(info.Size()) != manifest.MemorySize {
		return snapshotManifest{}, nil, fmt.Errorf("snapshot memory size = %d, want %d", info.Size(), manifest.MemorySize)
	}
	mem, err := syscall.Mmap(int(memFile.Fd()), 0, int(info.Size()), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE)
	if err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("mmap snapshot memory: %w", err)
	}
	return manifest, mem, nil
}

func snapshotMemoryPath(snapshotPath, memoryFile string) string {
	manifestPath := strings.TrimSpace(snapshotPath)
	if info, err := os.Stat(manifestPath); err == nil && info.IsDir() {
		manifestPath = filepath.Join(manifestPath, "manifest.json")
	}
	path, err := vmruntime.ResolveSnapshotMemoryPath(manifestPath, memoryFile)
	if err != nil {
		return ""
	}
	return path
}

func validateSnapshotRequest(manifest snapshotManifest, req ContainerRunRequest) error {
	if req.MemoryMB != 0 {
		want := arm64vm.MemorySizeBytes(req.MemoryMB)
		if manifest.MemorySize != 0 && manifest.MemorySize != want {
			return fmt.Errorf("snapshot memory size = %d, want %d for %d MB VM", manifest.MemorySize, want, req.MemoryMB)
		}
	}
	return nil
}

func (v *VM) markLastMappingOwned() {
	v.mappingsMu.Lock()
	defer v.mappingsMu.Unlock()
	if len(v.mappings) == 0 {
		return
	}
	v.mappings[len(v.mappings)-1].anonymous = true
}

func replaySnapshotMMIO(vm *VM, writes []snapshotMMIOWrite, uart *serial.UART8250, console *virtio.Console, rng *virtio.RNG, balloon *virtio.Balloon, fsdevs []*virtio.FS, vsock *virtio.Vsock, netdev *virtio.Net) error {
	const virtioRegQueueNotify = 0x50
	const virtioRegInterruptAck = 0x64
	for _, write := range writes {
		addr, size, value := write.Addr, write.Size, write.Value
		if addr%0x1000 == virtioRegQueueNotify || addr%0x1000 == virtioRegInterruptAck {
			continue
		}
		switch {
		case addr >= arm64vm.SnapshotBase && addr+uint64(size) <= arm64vm.SnapshotBase+arm64vm.SnapshotSize:
			continue
		case uart != nil && uart.Contains(addr, size):
			continue
		case mmioInRange(addr, arm64vm.GICDistributorMin, arm64vm.GICDistributorMax) || mmioInRange(addr, arm64vm.GICRedistributorMin, arm64vm.GICRedistributorMax):
			if err := replayGICWrite(vm, addr, value); err != nil {
				return err
			}
		case console != nil && console.Contains(addr, size):
			if err := console.Write(addr, size, value); err != nil {
				return fmt.Errorf("replay console write %#x: %w", addr, err)
			}
		case rng != nil && rng.Contains(addr, size):
			if err := rng.Write(addr, size, value); err != nil {
				return fmt.Errorf("replay rng write %#x: %w", addr, err)
			}
		case balloon != nil && balloon.Contains(addr, size):
			if err := balloon.Write(addr, size, value); err != nil {
				return fmt.Errorf("replay balloon write %#x: %w", addr, err)
			}
		case vsock != nil && vsock.Contains(addr, size):
			if err := vsock.Write(addr, size, value); err != nil {
				return fmt.Errorf("replay vsock write %#x: %w", addr, err)
			}
		case netdev != nil && netdev.Contains(addr, size):
			if err := netdev.Write(addr, size, value); err != nil {
				return fmt.Errorf("replay net write %#x: %w", addr, err)
			}
		default:
			if fsdev := findFSDevice(fsdevs, addr, size); fsdev != nil {
				if err := fsdev.Write(addr, size, value); err != nil {
					return fmt.Errorf("replay fs write %#x: %w", addr, err)
				}
			}
		}
	}
	return nil
}

func replayGICWrite(vm *VM, addr, value uint64) error {
	switch {
	case mmioInRange(addr, arm64vm.GICDistributorMin, arm64vm.GICDistributorMax):
		reg := GICDistributorReg(addr - arm64vm.GICDistributorMin)
		if err := vm.SetGICDistributorReg(reg, value); err != nil {
			return fmt.Errorf("replay gic distributor write %#x: %w", addr, err)
		}
	case mmioInRange(addr, arm64vm.GICRedistributorMin, arm64vm.GICRedistributorMax):
		const redistStride = 0x20000
		redistIndex := int((addr - arm64vm.GICRedistributorMin) / redistStride)
		reg := GICRedistributorReg((addr - arm64vm.GICRedistributorMin) % redistStride)
		if err := vm.SetGICRedistributorRegForVCPU(redistIndex, reg, value); err != nil {
			return fmt.Errorf("replay gic redistributor write %#x: %w", addr, err)
		}
	}
	return nil
}

func restoreSnapshotRegisters(vm *VM, manifest snapshotManifest) error {
	for name, value := range manifest.VCPURegisters {
		reg, ok := snapshotRegByName(name)
		if !ok {
			continue
		}
		if err := vm.SetRegForVCPU(manifest.CapturedVCPU, reg, value); err != nil {
			return fmt.Errorf("restore register %s: %w", name, err)
		}
	}
	for name, value := range manifest.VCPUSysRegisters {
		reg, ok := snapshotSysRegByName(name)
		if !ok {
			continue
		}
		if err := vm.SetSysRegForVCPU(manifest.CapturedVCPU, reg, value); err != nil {
			timingLog("restore sysreg %s failed: %v", name, err)
		}
	}
	return nil
}

func restoreSnapshotGIC(vm *VM, manifest snapshotManifest) error {
	for name, value := range manifest.GICDistributor {
		reg, err := parseGICDistributorReg(name)
		if err != nil {
			continue
		}
		if gicDistributorRuntimeReg(reg) {
			continue
		}
		if reg == 0 {
			value &^= 1 // Keep group 0 disabled; Linux has no root FIQ handler.
		}
		if err := vm.SetGICDistributorReg(reg, value); err != nil {
			timingLog("restore gic distributor %s failed: %v", name, err)
		}
	}
	for _, reg := range []GICDistributorReg{0x084, 0x088} {
		if err := vm.SetGICDistributorReg(reg, 0xffffffff); err != nil {
			timingLog("force gic distributor group %s failed: %v", gicRegKey(uint64(reg)), err)
		}
	}
	if err := vm.SetGICRedistributorRegForVCPU(manifest.CapturedVCPU, 0x10080, 0xffffffff); err != nil {
		timingLog("force gic redistributor group 0x10080 failed: %v", err)
	}
	for name, value := range manifest.GICRedistributor {
		reg, err := parseGICRedistributorReg(name)
		if err != nil || !gicRedistributorConfigReg(reg) {
			continue
		}
		if err := vm.SetGICRedistributorRegForVCPU(manifest.CapturedVCPU, reg, value); err != nil {
			timingLog("restore gic redistributor %s failed: %v", name, err)
		}
	}
	for name, value := range manifest.GICICC {
		reg, ok := gicICCRegByName(name)
		if !ok {
			continue
		}
		if err := vm.SetGICICCRegForVCPU(manifest.CapturedVCPU, reg, value); err != nil {
			timingLog("restore gic icc %s failed: %v", name, err)
		}
	}
	return nil
}

func restoreSnapshotVTimer(vm *VM, manifest snapshotManifest) error {
	if err := vm.SetVTimerOffset(manifest.VTimerOffset); err != nil {
		return fmt.Errorf("restore vtimer offset: %w", err)
	}
	if err := vm.SetVTimerMask(manifest.VTimerMasked); err != nil {
		return fmt.Errorf("restore vtimer mask: %w", err)
	}
	return nil
}

func gicDistributorRuntimeReg(reg GICDistributorReg) bool {
	return (reg >= 0x200 && reg < 0x300) || (reg >= 0x300 && reg < 0x400)
}

func gicRedistributorConfigReg(reg GICRedistributorReg) bool {
	return reg == 0x10100 ||
		(reg >= 0x10400 && reg < 0x10500) ||
		(reg >= 0x10c00 && reg < 0x10d00)
}

func parseGICDistributorReg(name string) (GICDistributorReg, error) {
	value, err := strconv.ParseUint(strings.TrimPrefix(name, "0x"), 16, 16)
	if err != nil {
		return 0, err
	}
	return GICDistributorReg(value), nil
}

func parseGICRedistributorReg(name string) (GICRedistributorReg, error) {
	value, err := strconv.ParseUint(strings.TrimPrefix(name, "0x"), 16, 32)
	if err != nil {
		return 0, err
	}
	return GICRedistributorReg(value), nil
}

func gicICCRegByName(name string) (GICICCReg, bool) {
	switch name {
	case "ctlr_el1":
		return hvGICICCRegCTLR_EL1, true
	case "igrpen0_el1":
		return hvGICICCRegIGRPEN0_EL1, true
	case "igrpen1_el1":
		return hvGICICCRegIGRPEN1_EL1, true
	case "pmr_el1":
		return hvGICICCRegPMR_EL1, true
	case "sre_el1":
		return hvGICICCRegSRE_EL1, true
	default:
		return 0, false
	}
}

func snapshotRegByName(name string) (Reg, bool) {
	switch name {
	case "pc":
		return hvRegPC, true
	case "cpsr":
		return hvRegCPSR, true
	}
	if strings.HasPrefix(name, "x") {
		n, err := strconv.Atoi(strings.TrimPrefix(name, "x"))
		if err == nil && n >= 0 && n <= 30 {
			return Reg(n), true
		}
	}
	return 0, false
}

func snapshotSysRegByName(name string) (SysReg, bool) {
	switch name {
	case "amair_el1":
		return hvSysRegAMAIR_EL1, true
	case "contextidr_el1":
		return hvSysRegCONTEXTIDR_EL1, true
	case "cntfrq_el0":
		return hvSysRegCNTFRQ_EL0, true
	case "cntp_ctl_el0":
		return hvSysRegCNTP_CTL_EL0, true
	case "cntp_cval_el0":
		return hvSysRegCNTP_CVAL_EL0, true
	case "cntp_tval_el0":
		return hvSysRegCNTP_TVAL_EL0, true
	case "cntv_ctl_el0":
		return hvSysRegCNTV_CTL_EL0, true
	case "cntv_cval_el0":
		return hvSysRegCNTV_CVAL_EL0, true
	case "cntv_tval_el0":
		return hvSysRegCNTV_TVAL_EL0, true
	case "cntvoff_el2":
		return hvSysRegCNTVOFF_EL2, true
	case "cpacr_el1":
		return hvSysRegCPACR_EL1, true
	case "elr_el1":
		return hvSysRegELR_EL1, true
	case "esr_el1":
		return hvSysRegESR_EL1, true
	case "far_el1":
		return hvSysRegFAR_EL1, true
	case "mair_el1":
		return hvSysRegMAIR_EL1, true
	case "sctlr_el1":
		return hvSysRegSCTLR_EL1, true
	case "sp_el0":
		return hvSysRegSP_EL0, true
	case "sp_el1":
		return hvSysRegSP_EL1, true
	case "sp_el2":
		return hvSysRegSP_EL2, true
	case "spsr_el1":
		return hvSysRegSPSR_EL1, true
	case "tcr_el1":
		return hvSysRegTCR_EL1, true
	case "tpidr_el0":
		return hvSysRegTPIDR_EL0, true
	case "tpidr_el1":
		return hvSysRegTPIDR_EL1, true
	case "tpidrro_el0":
		return hvSysRegTPIDRRO_EL0, true
	case "ttbr0_el1":
		return hvSysRegTTBR0EL1, true
	case "ttbr1_el1":
		return hvSysRegTTBR1EL1, true
	case "vbar_el1":
		return hvSysRegVBAR_EL1, true
	default:
		return 0, false
	}
}
