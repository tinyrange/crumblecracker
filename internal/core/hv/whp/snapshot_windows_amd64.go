//go:build windows && amd64

package whp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	"github.com/tinyrange/crumblecracker/internal/core/hv/snapshotstore"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const (
	snapshotTriggerMagic        = 0x43535833534e4150
	snapshotTriggerSerialMarker = "__CCX3_SNAPSHOT__"
)

type snapshotTrigger struct {
	base uint64
	size uint64
	dir  string
	mem  []byte

	mu      sync.Mutex
	pending bool
	value   uint64
	once    sync.Once
	err     error
}

type whpSnapshotManifest struct {
	Format        string                      `json:"format"`
	Partial       bool                        `json:"partial"`
	CapturedAt    string                      `json:"captured_at"`
	TriggerBase   uint64                      `json:"trigger_base"`
	TriggerValue  uint64                      `json:"trigger_value"`
	MemoryBase    uint64                      `json:"memory_base"`
	MemorySize    uint64                      `json:"memory_size"`
	MemoryFile    string                      `json:"memory_file"`
	CapturedVCPU  int                         `json:"captured_vcpu"`
	VCPURegisters map[string]snapshotRegister `json:"vcpu_registers"`
	VCPUStates    map[string][]byte           `json:"vcpu_states,omitempty"`
	Devices       map[string]virtio.MMIOState `json:"devices,omitempty"`
	Platform      whpSnapshotPlatformManifest `json:"platform"`
	Note          string                      `json:"note"`
}

type snapshotRegister struct {
	Low64  uint64 `json:"low64"`
	High64 uint64 `json:"high64,omitempty"`
}

type whpSnapshotPlatformManifest struct {
	PIC    bootPICState    `json:"pic"`
	PIT    bootPITState    `json:"pit"`
	IOAPIC bootIOAPICState `json:"ioapic"`
	HPET   bootHPETState   `json:"hpet"`
}

func newSnapshotTrigger(dir string, mem []byte) *snapshotTrigger {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	return &snapshotTrigger{
		base: amd64vm.SnapshotBase,
		size: amd64vm.SnapshotSize,
		dir:  dir,
		mem:  mem,
	}
}

func (s *snapshotTrigger) contains(addr uint64, size int) bool {
	return s != nil && size > 0 && addr >= s.base && addr+uint64(size) <= s.base+s.size
}

func (s *snapshotTrigger) markWrite(value uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pending = true
	s.value = value
	s.mu.Unlock()
}

func (s *snapshotTrigger) takePending() (uint64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending {
		return 0, false
	}
	s.pending = false
	return s.value, true
}

func (s *snapshotTrigger) wrapSerialWriter(w io.Writer) io.Writer {
	if s == nil {
		return w
	}
	return &snapshotSerialWriter{dst: w, trigger: s}
}

type snapshotSerialWriter struct {
	dst     io.Writer
	trigger *snapshotTrigger
	recent  string
}

func (w *snapshotSerialWriter) Write(data []byte) (int, error) {
	if w.trigger != nil && len(data) != 0 {
		w.recent += string(data)
		if len(w.recent) > len(snapshotTriggerSerialMarker)*2 {
			w.recent = w.recent[len(w.recent)-len(snapshotTriggerSerialMarker)*2:]
		}
		if strings.Contains(w.recent, snapshotTriggerSerialMarker) {
			w.trigger.markWrite(snapshotTriggerMagic)
			w.recent = ""
		}
	}
	if w.dst == nil {
		return len(data), nil
	}
	return w.dst.Write(data)
}

func (p *bootPlatform) captureSnapshotIfPending(vm *VM) error {
	if p == nil || p.snapshot == nil {
		return nil
	}
	value, ok := p.snapshot.takePending()
	if !ok {
		return nil
	}
	p.snapshot.once.Do(func() {
		p.snapshot.err = p.snapshot.capture(vm, p, value)
	})
	if p.snapshot.err != nil {
		return fmt.Errorf("capture WHP snapshot: %w", p.snapshot.err)
	}
	return nil
}

func (s *snapshotTrigger) capture(vm *VM, platform *bootPlatform, value uint64) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return nil
	}
	capture, err := snapshotstore.Begin(s.dir)
	if err != nil {
		return err
	}
	defer capture.Abort()
	outDir := capture.Dir()
	if err := os.WriteFile(filepath.Join(outDir, "memory.bin"), s.mem, 0o600); err != nil {
		return fmt.Errorf("write snapshot memory: %w", err)
	}
	states, err := snapshotWHPStates(vm)
	if err != nil {
		return fmt.Errorf("capture WHP vCPU state: %w", err)
	}
	manifest := whpSnapshotManifest{
		Format:        "ccx3-whp-snapshot-v0",
		Partial:       true,
		CapturedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		TriggerBase:   s.base,
		TriggerValue:  value,
		MemoryBase:    amd64vm.MemoryBase,
		MemorySize:    uint64(len(s.mem)),
		MemoryFile:    "memory.bin",
		CapturedVCPU:  0,
		VCPURegisters: snapshotWHPRegisters(vm),
		VCPUStates:    states,
		Devices:       snapshotWHPDeviceStates(platform),
		Platform:      snapshotWHPPlatform(platform),
		Note:          "WHP Linux checkpoint captured after guest init configured non-unique state and before vsock ready.",
	}
	if manifest.VCPURegisters == nil {
		return fmt.Errorf("capture WHP vCPU registers")
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644); err != nil {
		return fmt.Errorf("write snapshot manifest: %w", err)
	}
	_, err = capture.Publish("memory.bin", "manifest.json")
	return err
}

func StartManagedSessionFromSnapshot(ctx context.Context, snapshotPath string, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	if err := emitManagedBootStatus(onEvent, "restoring VM snapshot"); err != nil {
		return nil, err
	}
	manifest, memPath, err := loadWHPSnapshot(snapshotPath)
	if err != nil {
		return nil, err
	}
	if memoryMB == 0 {
		memoryMB = manifest.MemorySize >> 20
	}
	backend := virtio.NewSimpleVsockBackend()
	listener, err := backend.Listen(vmruntime.ControlPort)
	if err != nil {
		return nil, fmt.Errorf("listen vsock control: %w", err)
	}
	vsock := virtio.NewVsock(amd64vm.VsockBase, amd64vm.VsockSize, amd64vm.VsockIRQ, vmruntime.GuestCID, backend)
	connCh := make(chan virtio.VsockConn, 1)
	acceptErrCh := make(chan error, 1)
	controlTranscript := vmruntime.NewSerialTranscript()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		connCh <- conn
		_, _ = io.Copy(controlTranscript, conn)
	}()

	var bootWriter *vmruntime.BootEventWriter
	var serialWriter io.Writer
	if onEvent != nil {
		bootWriter = vmruntime.NewBootEventWriter(onEvent)
		serialWriter = bootWriter
	}
	vm, platform, serialOut, err := restoreManagedVMFromSnapshot(manifest, memPath, memoryMB, dmesg, fsdevs, vsock, netdev, serialWriter)
	if err != nil {
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		err := runManagedExecVM(runCtx, vm, platform, serialOut)
		platform.Close()
		vm.Close()
		doneCh <- err
	}()

	var control virtio.VsockConn
	select {
	case err := <-acceptErrCh:
		cancel()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	case conn := <-connCh:
		control = conn
	case err := <-doneCh:
		cancel()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	case <-ctx.Done():
		cancel()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(fmt.Errorf("%w (%s)", ctx.Err(), platform.Summary()), serialOut.String(), controlTranscript.String())
	}

	if _, err := controlTranscript.WaitFor(ctx, 0, func(text string) bool {
		return strings.Contains(text, vmruntime.InstanceReadyMarker) || vmruntime.HasFatalBootText(text)
	}); err != nil {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%s)", err, platform.Summary())
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	}
	if vmruntime.HasFatalBootText(controlTranscript.String()) {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(fmt.Errorf("guest reported boot failure"), serialOut.String(), controlTranscript.String())
	}
	if err := emitManagedBootStatus(onEvent, "guest ready"); err != nil {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, err
	}

	return &ManagedSession{
		cancel:     cancel,
		doneCh:     doneCh,
		control:    control,
		listener:   listener,
		vsock:      vsock,
		bootWriter: bootWriter,
		transcript: controlTranscript,
		serialOut:  serialOut,
		platform:   platform,
		dmesg:      dmesg,
	}, nil
}

func restoreManagedVMFromSnapshot(manifest whpSnapshotManifest, memPath string, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, vsock *virtio.Vsock, netdev *virtio.Net, serialWriter io.Writer) (*VM, *bootPlatform, *vmruntime.SerialTranscript, error) {
	memorySize := amd64vm.MemorySizeBytes(memoryMB)
	if manifest.MemorySize != 0 && manifest.MemorySize != memorySize {
		return nil, nil, nil, fmt.Errorf("snapshot memory size %d does not match requested VM memory %d", manifest.MemorySize, memorySize)
	}
	mem, err := virtualMapFile(memPath, uintptr(memorySize))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("map snapshot memory: %w", err)
	}
	vm, err := newVMWithAllocation(memorySize, true, true, 1, mem)
	if err != nil {
		return nil, nil, nil, err
	}

	serialOut := vmruntime.NewSerialTranscript()
	if serialWriter == nil {
		serialWriter = serialOut
	} else {
		serialWriter = io.MultiWriter(serialOut, serialWriter)
	}
	platform := newRestoredBootPlatform(vm, serial.NewUART8250(amd64vm.COM1Base, 0, serialWriter))
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Async = false
			platform.AttachFS(fsdev)
		}
	}
	if vsock != nil {
		platform.AttachVsock(vsock)
	}
	if netdev != nil {
		platform.AttachNet(netdev)
	}
	rng := virtio.NewRNG(amd64vm.RNGBase, amd64vm.RNGSize, amd64vm.RNGIRQ)
	platform.AttachRNG(rng)
	restoreWHPPlatform(platform, manifest.Platform)
	if err := restoreWHPDeviceStates(manifest.Devices, platform); err != nil {
		platform.Close()
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	platform.deassertDeviceIRQs()
	if err := restoreWHPPreStateRegisters(vm, manifest.VCPURegisters); err != nil {
		platform.Close()
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	if err := restoreWHPStates(vm, manifest.VCPUStates); err != nil {
		platform.Close()
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	if err := restoreWHPRegisters(vm, manifest.VCPURegisters); err != nil {
		platform.Close()
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	platform.resampleDeviceIRQs()
	if err := vm.EnableEmulation(platform); err != nil {
		platform.Close()
		_ = vm.Close()
		return nil, nil, serialOut, fmt.Errorf("enable emulation: %w", err)
	}
	return vm, platform, serialOut, nil
}

func loadWHPSnapshot(path string) (whpSnapshotManifest, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return whpSnapshotManifest{}, "", fmt.Errorf("snapshot path is required")
	}
	manifestPath := path
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		manifestPath = filepath.Join(path, "manifest.json")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return whpSnapshotManifest{}, "", fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest whpSnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return whpSnapshotManifest{}, "", fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.Format != "ccx3-whp-snapshot-v0" {
		return whpSnapshotManifest{}, "", fmt.Errorf("unsupported WHP snapshot format %q", manifest.Format)
	}
	memPath, err := vmruntime.ResolveSnapshotMemoryPath(manifestPath, manifest.MemoryFile)
	if err != nil {
		return whpSnapshotManifest{}, "", err
	}
	info, err := os.Stat(memPath)
	if err != nil {
		return whpSnapshotManifest{}, "", fmt.Errorf("stat snapshot memory: %w", err)
	}
	if info.Size() <= 0 {
		return whpSnapshotManifest{}, "", fmt.Errorf("snapshot memory is empty")
	}
	if manifest.MemorySize != 0 && uint64(info.Size()) != manifest.MemorySize {
		return whpSnapshotManifest{}, "", fmt.Errorf("snapshot memory size = %d, want %d", info.Size(), manifest.MemorySize)
	}
	return manifest, memPath, nil
}

func snapshotWHPRegisters(vm *VM) map[string]snapshotRegister {
	names := whpSnapshotRegisterNames()
	values := make([]registerValue, len(names))
	if err := getVirtualProcessorRegisters(vm.part, 0, names, values); err != nil {
		return nil
	}
	out := make(map[string]snapshotRegister, len(names))
	for i, name := range names {
		out[whpRegisterName(name)] = snapshotRegister{Low64: values[i].raw.Low64, High64: values[i].raw.High64}
	}
	for _, name := range whpOptionalSnapshotRegisterNames() {
		value, ok := snapshotOptionalWHPRegister(vm, name)
		if ok {
			out[whpRegisterName(name)] = value
		}
	}
	return out
}

func snapshotOptionalWHPRegister(vm *VM, name registerName) (snapshotRegister, bool) {
	values := make([]registerValue, 1)
	if err := getVirtualProcessorRegisters(vm.part, 0, []registerName{name}, values); err != nil {
		return snapshotRegister{}, false
	}
	return snapshotRegister{Low64: values[0].raw.Low64, High64: values[0].raw.High64}, true
}

func restoreWHPPreStateRegisters(vm *VM, regs map[string]snapshotRegister) error {
	names := make([]registerName, 0, len(regs))
	values := make([]registerValue, 0, len(regs))
	for _, name := range whpSnapshotPreStateRegisterNames() {
		state, ok := regs[whpRegisterName(name)]
		if !ok {
			continue
		}
		names = append(names, name)
		values = append(values, registerValue{raw: uint128{Low64: state.Low64, High64: state.High64}})
	}
	if len(names) == 0 {
		return fmt.Errorf("snapshot contains no pre-state WHP vCPU registers")
	}
	return setVirtualProcessorRegisters(vm.part, 0, names, values)
}

func restoreWHPRegisters(vm *VM, regs map[string]snapshotRegister) error {
	names := make([]registerName, 0, len(regs))
	values := make([]registerValue, 0, len(regs))
	for _, name := range whpSnapshotRegisterRestoreNames() {
		state, ok := regs[whpRegisterName(name)]
		if !ok {
			continue
		}
		names = append(names, name)
		values = append(values, registerValue{raw: uint128{Low64: state.Low64, High64: state.High64}})
	}
	if len(names) == 0 {
		return fmt.Errorf("snapshot contains no WHP vCPU registers")
	}
	if err := setVirtualProcessorRegisters(vm.part, 0, names, values); err != nil {
		return err
	}
	return verifyRestoredWHPRegisters(vm, regs)
}

func snapshotWHPStates(vm *VM) (map[string][]byte, error) {
	out := make(map[string][]byte, len(whpSnapshotStateTypes()))
	for _, spec := range whpSnapshotStateTypes() {
		state, err := getVirtualProcessorState(vm.part, 0, spec.stateType)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", spec.key, err)
		}
		out[spec.key] = state
	}
	return out, nil
}

func restoreWHPStates(vm *VM, states map[string][]byte) error {
	if len(states) == 0 {
		return fmt.Errorf("snapshot contains no WHP vCPU saved state")
	}
	for _, spec := range whpSnapshotStateTypes() {
		state, ok := states[spec.key]
		if !ok {
			return fmt.Errorf("snapshot missing WHP vCPU state %q", spec.key)
		}
		if err := setVirtualProcessorState(vm.part, 0, spec.stateType, state); err != nil {
			return fmt.Errorf("restore WHP vCPU state %s: %w", spec.key, err)
		}
	}
	return nil
}

type whpSnapshotStateType struct {
	key       string
	stateType virtualProcessorStateType
}

func whpSnapshotStateTypes() []whpSnapshotStateType {
	return []whpSnapshotStateType{
		{key: "local_apic", stateType: virtualProcessorStateTypeInterruptControllerState2},
		{key: "xsave", stateType: virtualProcessorStateTypeXsaveState},
	}
}

func verifyRestoredWHPRegisters(vm *VM, regs map[string]snapshotRegister) error {
	critical := []registerName{registerRip, registerRsp, registerRflags, registerCr0, registerCr3, registerCr4, registerEfer, registerIdtr, registerGdtr}
	values := make([]registerValue, len(critical))
	if err := getVirtualProcessorRegisters(vm.part, 0, critical, values); err != nil {
		return fmt.Errorf("read restored WHP vCPU registers: %w", err)
	}
	for i, name := range critical {
		key := whpRegisterName(name)
		want, ok := regs[key]
		if !ok {
			continue
		}
		if got := values[i].raw.Low64; got != want.Low64 {
			return fmt.Errorf("restore WHP register %s = %#x, want %#x", key, got, want.Low64)
		}
	}
	return nil
}

func whpSnapshotRegisterRestoreNames() []registerName {
	return []registerName{
		registerCr3, registerCr4, registerCr0, registerCr2, registerCr8, registerEfer, registerXCr0,
		registerXss, registerUCet, registerSCet, registerSsp,
		registerPl0Ssp, registerPl1Ssp, registerPl2Ssp, registerPl3Ssp, registerInterruptSsp,
		registerTscDeadline, registerTscAdjust, registerXfd, registerXfdErr,
		registerPat, registerApicBase,
		registerSysenterCs, registerSysenterEip, registerSysenterEsp,
		registerStar, registerLstar, registerCstar, registerSfmask,
		registerKernelGsBase,
		registerGdtr, registerIdtr,
		registerEs, registerCs, registerSs, registerDs, registerFs, registerGs, registerLdtr, registerTr,
		registerRax, registerRcx, registerRdx, registerRbx,
		registerRsp, registerRbp, registerRsi, registerRdi,
		registerR8, registerR9, registerR10, registerR11,
		registerR12, registerR13, registerR14, registerR15,
		registerRip, registerRflags,
		registerTsc,
		registerPendingInterruption, registerDeliverabilityNotifications, registerInternalActivityState,
	}
}

func whpSnapshotPreStateRegisterNames() []registerName {
	return []registerName{
		registerCr0, registerCr4, registerXCr0, registerXss,
		registerEfer, registerPat,
	}
}

func whpSnapshotRegisterNames() []registerName {
	return []registerName{
		registerRax, registerRcx, registerRdx, registerRbx,
		registerRsp, registerRbp, registerRsi, registerRdi,
		registerR8, registerR9, registerR10, registerR11,
		registerR12, registerR13, registerR14, registerR15,
		registerRip, registerRflags,
		registerEs, registerCs, registerSs, registerDs, registerFs, registerGs, registerLdtr, registerTr,
		registerIdtr, registerGdtr,
		registerCr0, registerCr2, registerCr3, registerCr4, registerCr8, registerXCr0,
		registerTsc, registerEfer, registerKernelGsBase, registerApicBase, registerPat,
		registerSysenterCs, registerSysenterEip, registerSysenterEsp,
		registerStar, registerLstar, registerCstar, registerSfmask,
		registerPendingInterruption, registerDeliverabilityNotifications, registerInternalActivityState,
	}
}

func whpOptionalSnapshotRegisterNames() []registerName {
	return []registerName{
		registerXss, registerUCet, registerSCet, registerSsp,
		registerPl0Ssp, registerPl1Ssp, registerPl2Ssp, registerPl3Ssp, registerInterruptSsp,
		registerTscDeadline, registerTscAdjust, registerXfd, registerXfdErr,
	}
}

func whpRegisterName(name registerName) string {
	if name >= registerXmm0 && name <= registerXmm15 {
		return fmt.Sprintf("xmm%d", int(name-registerXmm0))
	}
	if name >= registerFpMmx0 && name <= registerFpMmx7 {
		return fmt.Sprintf("fp_mmx%d", int(name-registerFpMmx0))
	}
	switch name {
	case registerRax:
		return "rax"
	case registerRcx:
		return "rcx"
	case registerRdx:
		return "rdx"
	case registerRbx:
		return "rbx"
	case registerRsp:
		return "rsp"
	case registerRbp:
		return "rbp"
	case registerRsi:
		return "rsi"
	case registerRdi:
		return "rdi"
	case registerR8:
		return "r8"
	case registerR9:
		return "r9"
	case registerR10:
		return "r10"
	case registerR11:
		return "r11"
	case registerR12:
		return "r12"
	case registerR13:
		return "r13"
	case registerR14:
		return "r14"
	case registerR15:
		return "r15"
	case registerRip:
		return "rip"
	case registerRflags:
		return "rflags"
	case registerEs:
		return "es"
	case registerCs:
		return "cs"
	case registerSs:
		return "ss"
	case registerDs:
		return "ds"
	case registerFs:
		return "fs"
	case registerGs:
		return "gs"
	case registerLdtr:
		return "ldtr"
	case registerTr:
		return "tr"
	case registerIdtr:
		return "idtr"
	case registerGdtr:
		return "gdtr"
	case registerCr0:
		return "cr0"
	case registerCr2:
		return "cr2"
	case registerCr3:
		return "cr3"
	case registerCr4:
		return "cr4"
	case registerCr8:
		return "cr8"
	case registerXCr0:
		return "xcr0"
	case registerFpControlStatus:
		return "fp_control_status"
	case registerXmmControlStatus:
		return "xmm_control_status"
	case registerTsc:
		return "tsc"
	case registerEfer:
		return "efer"
	case registerKernelGsBase:
		return "kernel_gs_base"
	case registerApicBase:
		return "apic_base"
	case registerPat:
		return "pat"
	case registerSysenterCs:
		return "sysenter_cs"
	case registerSysenterEip:
		return "sysenter_eip"
	case registerSysenterEsp:
		return "sysenter_esp"
	case registerStar:
		return "star"
	case registerLstar:
		return "lstar"
	case registerCstar:
		return "cstar"
	case registerSfmask:
		return "sfmask"
	case registerXss:
		return "xss"
	case registerUCet:
		return "u_cet"
	case registerSCet:
		return "s_cet"
	case registerSsp:
		return "ssp"
	case registerPl0Ssp:
		return "pl0_ssp"
	case registerPl1Ssp:
		return "pl1_ssp"
	case registerPl2Ssp:
		return "pl2_ssp"
	case registerPl3Ssp:
		return "pl3_ssp"
	case registerInterruptSsp:
		return "interrupt_ssp"
	case registerTscDeadline:
		return "tsc_deadline"
	case registerTscAdjust:
		return "tsc_adjust"
	case registerXfd:
		return "xfd"
	case registerXfdErr:
		return "xfd_err"
	case registerPendingInterruption:
		return "pending_interruption"
	case registerDeliverabilityNotifications:
		return "deliverability_notifications"
	case registerInternalActivityState:
		return "internal_activity_state"
	default:
		return "reg_" + strconv.FormatUint(uint64(name), 16)
	}
}

func snapshotWHPDeviceStates(platform *bootPlatform) map[string]virtio.MMIOState {
	out := make(map[string]virtio.MMIOState)
	if platform == nil {
		return out
	}
	if platform.rng != nil {
		out["rng"] = platform.rng.SnapshotState()
	}
	if platform.vsock != nil {
		out["vsock"] = platform.vsock.SnapshotState()
	}
	if platform.netdev != nil {
		out["net"] = platform.netdev.SnapshotState()
	}
	for _, fsdev := range platform.fsdevs {
		if fsdev == nil {
			continue
		}
		out[whpFSDeviceStateKey(fsdev)] = fsdev.SnapshotState()
	}
	return out
}

func restoreWHPDeviceStates(states map[string]virtio.MMIOState, platform *bootPlatform) error {
	if platform == nil {
		return nil
	}
	if state, ok := states["rng"]; ok && platform.rng != nil {
		if err := platform.rng.RestoreState(state); err != nil {
			return fmt.Errorf("restore rng state: %w", err)
		}
	}
	if state, ok := states["vsock"]; ok && platform.vsock != nil {
		if err := platform.vsock.RestoreState(state); err != nil {
			return fmt.Errorf("restore vsock state: %w", err)
		}
	}
	if state, ok := states["net"]; ok && platform.netdev != nil {
		if err := platform.netdev.RestoreState(state); err != nil {
			return fmt.Errorf("restore net state: %w", err)
		}
	}
	for _, fsdev := range platform.fsdevs {
		if fsdev == nil {
			continue
		}
		state, ok := states[whpFSDeviceStateKey(fsdev)]
		if !ok {
			return fmt.Errorf("snapshot missing state for fs device %#x", fsdev.Base)
		}
		if err := fsdev.RestoreState(state); err != nil {
			return fmt.Errorf("restore fs device %#x state: %w", fsdev.Base, err)
		}
	}
	return nil
}

func whpFSDeviceStateKey(fsdev *virtio.FS) string {
	return fmt.Sprintf("fs:%x", fsdev.Base)
}

func snapshotWHPPlatform(p *bootPlatform) whpSnapshotPlatformManifest {
	if p == nil {
		return whpSnapshotPlatformManifest{}
	}
	return whpSnapshotPlatformManifest{
		PIC:    p.pic.SnapshotState(),
		PIT:    p.pit.SnapshotState(),
		IOAPIC: p.ioapic.SnapshotState(),
		HPET:   p.hpet.SnapshotState(),
	}
}

func restoreWHPPlatform(p *bootPlatform, state whpSnapshotPlatformManifest) {
	if p == nil {
		return
	}
	p.pic.RestoreState(state.PIC)
	p.pit.RestoreState(state.PIT)
	p.ioapic.RestoreState(state.IOAPIC)
	p.hpet.RestoreState(state.HPET)
}
