//go:build windows && arm64

package whp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
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
	done    chan struct{}
}

type whpArm64SnapshotManifest struct {
	Format       string                      `json:"format"`
	Partial      bool                        `json:"partial"`
	CapturedAt   string                      `json:"captured_at"`
	TriggerBase  uint64                      `json:"trigger_base"`
	TriggerValue uint64                      `json:"trigger_value"`
	MemoryBase   uint64                      `json:"memory_base"`
	MemorySize   uint64                      `json:"memory_size"`
	MemoryFile   string                      `json:"memory_file"`
	CapturedVCPU int                         `json:"captured_vcpu"`
	Registers    map[string]snapshotRegister `json:"registers"`
	States       map[string][]byte           `json:"states,omitempty"`
	Devices      map[string]virtio.MMIOState `json:"devices,omitempty"`
	Note         string                      `json:"note"`
}

type snapshotRegister struct {
	Low64  uint64 `json:"low64"`
	High64 uint64 `json:"high64,omitempty"`
}

func newSnapshotTrigger(dir string, mem []byte) *snapshotTrigger {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	return &snapshotTrigger{
		base: arm64vm.SnapshotBase,
		size: arm64vm.SnapshotSize,
		dir:  dir,
		mem:  mem,
		done: make(chan struct{}),
	}
}

func newSnapshotResumeTrigger() *snapshotTrigger {
	return &snapshotTrigger{
		base: arm64vm.SnapshotBase,
		size: arm64vm.SnapshotSize,
	}
}

func (s *snapshotTrigger) contains(addr uint64, size int) bool {
	return s != nil && size > 0 && addr >= s.base && addr+uint64(size) <= s.base+s.size
}

func (s *snapshotTrigger) handleMMIO(vm *VM, mmio MMIOExit) (bool, error) {
	if !s.contains(mmio.Addr, int(mmio.Len)) {
		return false, nil
	}
	if mmio.Write {
		return true, vm.CompleteMMIOWrite(mmio)
	}
	return true, vm.CompleteMMIORead(mmio, snapshotTriggerMagic)
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

func (s *snapshotTrigger) captureIfPending(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, netdev *virtio.Net) error {
	if s == nil {
		return nil
	}
	value, ok := s.takePending()
	if !ok {
		return nil
	}
	s.once.Do(func() {
		s.err = s.capture(vm, fsdevs, vsock, rng, netdev, value)
		if s.done != nil {
			close(s.done)
		}
	})
	if s.err != nil {
		return fmt.Errorf("capture WHP arm64 snapshot: %w", s.err)
	}
	return nil
}

func (s *snapshotTrigger) captureNow(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, netdev *virtio.Net, value uint64) error {
	if s == nil || s.dir == "" {
		return nil
	}
	s.once.Do(func() {
		s.err = s.capture(vm, fsdevs, vsock, rng, netdev, value)
		if s.done != nil {
			close(s.done)
		}
	})
	if s.err != nil {
		return fmt.Errorf("capture WHP arm64 snapshot: %w", s.err)
	}
	return nil
}

func (s *snapshotTrigger) requestHostCapture(vm *VM) error {
	if s == nil || s.dir == "" {
		return nil
	}
	s.markWrite(snapshotTriggerMagic)
	return vm.CancelRun()
}

func (s *snapshotTrigger) wait(ctx context.Context) error {
	if s == nil || s.dir == "" {
		return nil
	}
	select {
	case <-s.done:
		if s.err != nil {
			return fmt.Errorf("capture WHP arm64 snapshot: %w", s.err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *snapshotTrigger) fail(err error) {
	if s == nil || err == nil {
		return
	}
	s.once.Do(func() {
		s.err = err
		if s.done != nil {
			close(s.done)
		}
	})
}

func (s *snapshotTrigger) capture(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, netdev *virtio.Net, value uint64) error {
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
	states, err := snapshotArm64WHPStates(vm)
	if err != nil {
		return fmt.Errorf("capture WHP arm64 vCPU state: %w", err)
	}
	manifest := whpArm64SnapshotManifest{
		Format:       "ccx3-whp-arm64-snapshot-v0",
		Partial:      true,
		CapturedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		TriggerBase:  s.base,
		TriggerValue: value,
		MemoryBase:   arm64vm.MemoryBase,
		MemorySize:   uint64(len(s.mem)),
		MemoryFile:   "memory.bin",
		CapturedVCPU: 0,
		Registers:    snapshotArm64WHPRegisters(vm),
		States:       states,
		Devices:      snapshotArm64WHPDeviceStates(fsdevs, vsock, rng, netdev),
		Note:         "WHP arm64 Linux checkpoint captured after guest init configured non-unique state and before vsock ready.",
	}
	if len(manifest.Registers) == 0 {
		return fmt.Errorf("capture WHP arm64 vCPU registers")
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
	trace := os.Getenv("CC_WHP_ARM64_TIMING") != ""
	startTime := time.Now()
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 snapshot session +0s: starting restore session\n")
	}
	if err := emitManagedBootStatus(onEvent, "restoring VM snapshot"); err != nil {
		return nil, err
	}
	manifest, memPath, err := loadArm64WHPSnapshot(snapshotPath)
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
	vsock := virtio.NewVsock(arm64vm.VsockBase, arm64vm.VsockSize, arm64vm.VsockIRQ, vmruntime.GuestCID, backend)
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)
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
	vm, uart, serialOut, err := restoreArm64ManagedVMFromSnapshot(manifest, memPath, memoryMB, dmesg, fsdevs, vsock, rng, netdev, serialWriter)
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
		err := runManagedExecVM(runCtx, vm, uart, fsdevs, vsock, rng, netdev, serialOut, newSnapshotResumeTrigger(), newWHPPCSampler("linux-restore", nil))
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 snapshot session +%s: run loop stopped err=%v\n", time.Since(startTime).Round(time.Millisecond), err)
		}
		closeFSDevices(fsdevs)
		closeStart := time.Now()
		closeErr := vm.Close()
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 snapshot session +%s: vm close took=%s err=%v\n", time.Since(startTime).Round(time.Millisecond), time.Since(closeStart).Round(time.Millisecond), closeErr)
		}
		err = errors.Join(err, closeErr)
		doneCh <- err
	}()
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 snapshot session +%s: run loop started\n", time.Since(startTime).Round(time.Millisecond))
	}

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
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 snapshot session +%s: control connected\n", time.Since(startTime).Round(time.Millisecond))
		}
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
		return nil, transcriptError(ctx.Err(), serialOut.String(), controlTranscript.String())
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
	if trace {
		_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 snapshot session +%s: ready marker received\n", time.Since(startTime).Round(time.Millisecond))
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

	return &ManagedSession{cancel: cancel, doneCh: doneCh, control: control, listener: listener, vsock: vsock, bootWriter: bootWriter, transcript: controlTranscript, serialOut: serialOut, dmesg: dmesg}, nil
}

func restoreArm64ManagedVMFromSnapshot(manifest whpArm64SnapshotManifest, memPath string, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, netdev *virtio.Net, serialWriter io.Writer) (*VM, *serial.UART8250, *vmruntime.SerialTranscript, error) {
	trace := os.Getenv("CC_WHP_ARM64_TIMING") != ""
	traceStart := time.Now()
	traceRestore := func(format string, args ...any) {
		if trace {
			_, _ = fmt.Fprintf(os.Stderr, "whp-arm64 snapshot restore +%s: %s\n", time.Since(traceStart).Round(time.Millisecond), fmt.Sprintf(format, args...))
		}
	}
	traceRestore("manifest registers=%d states=%s devices=%d", len(manifest.Registers), snapshotArm64StateSummary(manifest.States), len(manifest.Devices))
	memorySize := arm64vm.MemorySizeBytes(memoryMB)
	if manifest.MemorySize != 0 && manifest.MemorySize != memorySize {
		return nil, nil, nil, fmt.Errorf("snapshot memory size %d does not match requested VM memory %d", manifest.MemorySize, memorySize)
	}
	mem, err := loadArm64SnapshotMemory(memPath, memorySize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load snapshot memory: %w", err)
	}
	traceRestore("loaded memory bytes=%d", memorySize)
	vm, err := newVMWithAllocation(memorySize, arm64vm.MemoryBase, VMOptions{CNTVOverflowInterrupt: linuxWHPCNTVOverflowInterrupt}, mem)
	if err != nil {
		_ = mem.free()
		return nil, nil, nil, err
	}
	traceRestore("created VM")

	serialOut := vmruntime.NewSerialTranscript()
	if serialWriter == nil {
		serialWriter = serialOut
	} else {
		serialWriter = io.MultiWriter(serialOut, serialWriter)
	}
	uart := serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, serialWriter)
	uart.AttachIRQ(vm, arm64vm.UARTSPI)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Async = false
			fsdev.Attach(vm, vm)
		}
	}
	if vsock != nil {
		vsock.Attach(vm, vm)
	}
	if rng != nil {
		rng.Attach(vm, vm)
	}
	if netdev != nil {
		netdev.Attach(vm, vm)
	}
	if err := restoreArm64WHPDeviceStates(manifest.Devices, fsdevs, vsock, rng, netdev); err != nil {
		closeFSDevices(fsdevs)
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	traceRestore("restored devices")
	if err := restoreArm64WHPPreStateRegisters(vm, manifest.Registers); err != nil {
		closeFSDevices(fsdevs)
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	traceRestore("restored pre-state registers")
	if err := restoreArm64WHPStates(vm, manifest.States); err != nil {
		closeFSDevices(fsdevs)
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	traceRestore("restored WHP state")
	if err := restoreArm64WHPRegisters(vm, manifest.Registers); err != nil {
		closeFSDevices(fsdevs)
		_ = vm.Close()
		return nil, nil, serialOut, err
	}
	traceRestore("restored registers pc=%#x pstate=%#x", manifest.Registers["pc"].Low64, manifest.Registers["pstate"].Low64)
	return vm, uart, serialOut, nil
}

func loadArm64SnapshotMemory(path string, size uint64) (*allocation, error) {
	mem, err := virtualAlloc(uintptr(size))
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		_ = mem.free()
		return nil, err
	}
	defer file.Close()
	if _, err := io.ReadFull(file, mem.bytes()); err != nil {
		_ = mem.free()
		return nil, err
	}
	return mem, nil
}

func snapshotArm64StateSummary(states map[string][]byte) string {
	if len(states) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(states))
	for key, state := range states {
		keys = append(keys, fmt.Sprintf("%s:%d", key, len(state)))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func loadArm64WHPSnapshot(path string) (whpArm64SnapshotManifest, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return whpArm64SnapshotManifest{}, "", fmt.Errorf("snapshot path is required")
	}
	manifestPath := path
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		manifestPath = filepath.Join(path, "manifest.json")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return whpArm64SnapshotManifest{}, "", fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest whpArm64SnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return whpArm64SnapshotManifest{}, "", fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.Format != "ccx3-whp-arm64-snapshot-v0" {
		return whpArm64SnapshotManifest{}, "", fmt.Errorf("unsupported WHP arm64 snapshot format %q", manifest.Format)
	}
	memPath, err := vmruntime.ResolveSnapshotMemoryPath(manifestPath, manifest.MemoryFile)
	if err != nil {
		return whpArm64SnapshotManifest{}, "", err
	}
	info, err := os.Stat(memPath)
	if err != nil {
		return whpArm64SnapshotManifest{}, "", fmt.Errorf("stat snapshot memory: %w", err)
	}
	if info.Size() <= 0 {
		return whpArm64SnapshotManifest{}, "", fmt.Errorf("snapshot memory is empty")
	}
	if manifest.MemorySize != 0 && uint64(info.Size()) != manifest.MemorySize {
		return whpArm64SnapshotManifest{}, "", fmt.Errorf("snapshot memory size = %d, want %d", info.Size(), manifest.MemorySize)
	}
	return manifest, memPath, nil
}

func snapshotArm64WHPRegisters(vm *VM) map[string]snapshotRegister {
	names := whpArm64SnapshotRegisterNames()
	values := make([]registerValue, len(names))
	if err := getVirtualProcessorRegisters(vm.part, 0, names, values); err == nil {
		out := make(map[string]snapshotRegister, len(names))
		for i, name := range names {
			out[whpArm64RegisterName(name)] = snapshotRegister{Low64: values[i].raw.Low64, High64: values[i].raw.High64}
		}
		return out
	}
	out := make(map[string]snapshotRegister, len(names))
	for _, name := range names {
		value, ok := snapshotArm64OptionalWHPRegister(vm, name)
		if !ok {
			continue
		}
		out[whpArm64RegisterName(name)] = value
	}
	return out
}

func snapshotArm64OptionalWHPRegister(vm *VM, name registerName) (snapshotRegister, bool) {
	values := make([]registerValue, 1)
	if err := getVirtualProcessorRegisters(vm.part, 0, []registerName{name}, values); err != nil {
		return snapshotRegister{}, false
	}
	return snapshotRegister{Low64: values[0].raw.Low64, High64: values[0].raw.High64}, true
}

func restoreArm64WHPPreStateRegisters(vm *VM, regs map[string]snapshotRegister) error {
	if len(regs) == 0 {
		return fmt.Errorf("snapshot contains no WHP arm64 vCPU registers")
	}
	return restoreArm64WHPRegisterSet(vm, regs, whpArm64SnapshotPreStateRegisterNames())
}

func restoreArm64WHPRegisters(vm *VM, regs map[string]snapshotRegister) error {
	if len(regs) == 0 {
		return fmt.Errorf("snapshot contains no WHP arm64 vCPU registers")
	}
	if err := restoreArm64WHPRegisterSet(vm, regs, whpArm64SnapshotRegisterRestoreNames()); err != nil {
		return err
	}
	return verifyRestoredArm64WHPRegisters(vm, regs)
}

func restoreArm64WHPRegisterSet(vm *VM, regs map[string]snapshotRegister, names []registerName) error {
	setNames := make([]registerName, 0, len(names))
	values := make([]registerValue, 0, len(names))
	for _, name := range names {
		state, ok := regs[whpArm64RegisterName(name)]
		if !ok {
			continue
		}
		setNames = append(setNames, name)
		values = append(values, registerValue{raw: uint128{Low64: state.Low64, High64: state.High64}})
	}
	if len(setNames) == 0 {
		return nil
	}
	if err := setVirtualProcessorRegisters(vm.part, 0, setNames, values); err != nil {
		return fmt.Errorf("restore WHP arm64 registers: %w", err)
	}
	return nil
}

func snapshotArm64WHPStates(vm *VM) (map[string][]byte, error) {
	out := make(map[string][]byte, len(whpArm64SnapshotStateTypes()))
	for _, spec := range whpArm64SnapshotStateTypes() {
		state, err := getVirtualProcessorState(vm.part, spec.vpIndex, spec.stateType)
		if err != nil {
			if spec.required {
				return nil, fmt.Errorf("%s: %w", spec.key, err)
			}
			continue
		}
		out[spec.key] = state
	}
	return out, nil
}

func restoreArm64WHPStates(vm *VM, states map[string][]byte) error {
	for _, spec := range whpArm64SnapshotStateTypes() {
		state, ok := states[spec.key]
		if !ok {
			if spec.required {
				return fmt.Errorf("snapshot missing WHP arm64 state %q", spec.key)
			}
			continue
		}
		if err := setVirtualProcessorState(vm.part, spec.vpIndex, spec.stateType, state); err != nil {
			return fmt.Errorf("restore WHP arm64 state %s: %w", spec.key, err)
		}
	}
	return nil
}

type whpArm64SnapshotStateType struct {
	key       string
	stateType virtualProcessorStateType
	vpIndex   uint32
	required  bool
}

func whpArm64SnapshotStateTypes() []whpArm64SnapshotStateType {
	return []whpArm64SnapshotStateType{
		{key: "interrupt_controller", stateType: virtualProcessorStateTypeArm64InterruptControllerState, vpIndex: 0, required: true},
		{key: "global_interrupt_controller", stateType: virtualProcessorStateTypeArm64GlobalInterruptState, vpIndex: 0xffffffff, required: false},
	}
}

func verifyRestoredArm64WHPRegisters(vm *VM, regs map[string]snapshotRegister) error {
	for _, name := range []registerName{registerPC, registerPSTATE, registerSPEL1, registerSCTLREL1, registerTTBR0EL1, registerTTBR1EL1, registerTCREL1} {
		key := whpArm64RegisterName(name)
		want, ok := regs[key]
		if !ok {
			continue
		}
		got, ok := snapshotArm64OptionalWHPRegister(vm, name)
		if !ok {
			return fmt.Errorf("read restored WHP arm64 register %s", key)
		}
		if got.Low64 != want.Low64 || got.High64 != want.High64 {
			return fmt.Errorf("restore WHP arm64 register %s = %#x:%#x, want %#x:%#x", key, got.High64, got.Low64, want.High64, want.Low64)
		}
	}
	return nil
}

func whpArm64SnapshotRegisterNames() []registerName {
	names := make([]registerName, 0, 96)
	for i := 0; i <= 30; i++ {
		names = append(names, registerX(i))
	}
	for i := 0; i <= 31; i++ {
		names = append(names, registerName(uint32(registerQ0)+uint32(i)))
	}
	names = append(names,
		registerSP, registerSPEL0, registerSPEL1, registerPC, registerPSTATE,
		registerCurrentEL, registerDAIF, registerDIT, registerNZCV, registerPAN,
		registerSPSEL, registerSSBS, registerTCO, registerUAO,
		registerELREL1, registerSPSREL1, registerFPCR, registerFPSR,
		registerACTLREL1, registerCPACREL1, registerCSSELREL1,
		registerESREL1, registerFAREL1, registerMAIREL1, registerPAREL1,
		registerSCTLREL1, registerTCREL1,
		registerTPIDREL0, registerTPIDREL1, registerTPIDRROEL0,
		registerTTBR0EL1, registerTTBR1EL1, registerVBAREL1,
		registerCNTKCTLEL1, registerCNTVCTLEL0, registerCNTVCVALEL0, registerCNTVCTEL0,
		registerGICR,
		registerInternalActivityState, registerPendingEvent0, registerPendingEvent1,
		registerDeliverabilityNotification, registerPendingEvent2, registerPendingEvent3,
	)
	return names
}

func whpArm64SnapshotPreStateRegisterNames() []registerName {
	return []registerName{
		registerPSTATE, registerCurrentEL, registerDAIF, registerPAN, registerSPSEL,
		registerSCTLREL1, registerTCREL1, registerMAIREL1,
		registerTTBR0EL1, registerTTBR1EL1, registerVBAREL1,
		registerCNTKCTLEL1, registerCNTVCTLEL0, registerCNTVCVALEL0,
		registerGICR,
		registerInternalActivityState, registerPendingEvent0, registerPendingEvent1,
		registerDeliverabilityNotification, registerPendingEvent2, registerPendingEvent3,
	}
}

func whpArm64SnapshotRegisterRestoreNames() []registerName {
	names := make([]registerName, 0, len(whpArm64SnapshotRegisterNames()))
	for _, name := range whpArm64SnapshotRegisterNames() {
		switch name {
		case registerCNTVCTEL0:
			continue
		default:
			names = append(names, name)
		}
	}
	return names
}

func whpArm64RegisterName(name registerName) string {
	if name >= registerX0 && name <= registerLR {
		switch name {
		case registerFP:
			return "fp"
		case registerLR:
			return "lr"
		default:
			return "x" + strconv.FormatUint(uint64(name-registerX0), 10)
		}
	}
	if name >= registerQ0 && name <= registerQ0+31 {
		return "q" + strconv.FormatUint(uint64(name-registerQ0), 10)
	}
	switch name {
	case registerSP:
		return "sp"
	case registerSPEL0:
		return "sp_el0"
	case registerSPEL1:
		return "sp_el1"
	case registerPC:
		return "pc"
	case registerPSTATE:
		return "pstate"
	case registerCurrentEL:
		return "current_el"
	case registerDAIF:
		return "daif"
	case registerDIT:
		return "dit"
	case registerNZCV:
		return "nzcv"
	case registerPAN:
		return "pan"
	case registerSPSEL:
		return "spsel"
	case registerSSBS:
		return "ssbs"
	case registerTCO:
		return "tco"
	case registerUAO:
		return "uao"
	case registerELREL1:
		return "elr_el1"
	case registerSPSREL1:
		return "spsr_el1"
	case registerFPCR:
		return "fpcr"
	case registerFPSR:
		return "fpsr"
	case registerACTLREL1:
		return "actlr_el1"
	case registerCPACREL1:
		return "cpacr_el1"
	case registerCSSELREL1:
		return "csselr_el1"
	case registerESREL1:
		return "esr_el1"
	case registerFAREL1:
		return "far_el1"
	case registerMAIREL1:
		return "mair_el1"
	case registerPAREL1:
		return "par_el1"
	case registerSCTLREL1:
		return "sctlr_el1"
	case registerTCREL1:
		return "tcr_el1"
	case registerTPIDREL0:
		return "tpidr_el0"
	case registerTPIDREL1:
		return "tpidr_el1"
	case registerTPIDRROEL0:
		return "tpidrro_el0"
	case registerTTBR0EL1:
		return "ttbr0_el1"
	case registerTTBR1EL1:
		return "ttbr1_el1"
	case registerVBAREL1:
		return "vbar_el1"
	case registerCNTKCTLEL1:
		return "cntkctl_el1"
	case registerCNTVCTLEL0:
		return "cntv_ctl_el0"
	case registerCNTVCVALEL0:
		return "cntv_cval_el0"
	case registerCNTVCTEL0:
		return "cntvct_el0"
	case registerGICR:
		return "gicr_base_gpa"
	case registerInternalActivityState:
		return "internal_activity_state"
	case registerPendingEvent0:
		return "pending_event0"
	case registerPendingEvent1:
		return "pending_event1"
	case registerDeliverabilityNotification:
		return "deliverability_notifications"
	case registerPendingEvent2:
		return "pending_event2"
	case registerPendingEvent3:
		return "pending_event3"
	default:
		return "reg_" + strconv.FormatUint(uint64(name), 16)
	}
}

func snapshotArm64WHPDeviceStates(fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, netdev *virtio.Net) map[string]virtio.MMIOState {
	out := make(map[string]virtio.MMIOState)
	if rng != nil {
		out["rng"] = rng.SnapshotState()
	}
	if vsock != nil {
		out["vsock"] = vsock.SnapshotState()
	}
	if netdev != nil {
		out["net"] = netdev.SnapshotState()
	}
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		out[whpArm64FSDeviceStateKey(fsdev)] = fsdev.SnapshotState()
	}
	return out
}

func restoreArm64WHPDeviceStates(states map[string]virtio.MMIOState, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, netdev *virtio.Net) error {
	if state, ok := states["rng"]; ok && rng != nil {
		if err := rng.RestoreState(state); err != nil {
			return fmt.Errorf("restore rng state: %w", err)
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
		key := whpArm64FSDeviceStateKey(fsdev)
		state, ok := states[key]
		if !ok {
			return fmt.Errorf("snapshot missing state for fs device %#x", fsdev.Base)
		}
		if err := fsdev.RestoreState(state); err != nil {
			return fmt.Errorf("restore fs device %#x state: %w", fsdev.Base, err)
		}
	}
	return nil
}

func whpArm64FSDeviceStateKey(fsdev *virtio.FS) string {
	return fmt.Sprintf("fs:%x", fsdev.Base)
}
