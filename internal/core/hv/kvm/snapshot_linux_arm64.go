//go:build linux && arm64

package kvm

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
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const (
	snapshotTriggerMagic        = 0x43535833534e4150
	snapshotTriggerSerialMarker = "__CCX3_SNAPSHOT__"
	arm64KVMSnapshotFormat      = "ccx3-kvm-arm64-snapshot-v0"
)

type arm64KVMSnapshotManifest struct {
	Format       string                      `json:"format"`
	CapturedAt   string                      `json:"captured_at"`
	TriggerBase  uint64                      `json:"trigger_base"`
	TriggerValue uint64                      `json:"trigger_value"`
	MemoryBase   uint64                      `json:"memory_base"`
	MemorySize   uint64                      `json:"memory_size"`
	MemoryFile   string                      `json:"memory_file"`
	VCPU         map[string][]byte           `json:"vcpu"`
	VGICType     uint32                      `json:"vgic_type"`
	VGIC         []arm64KVMVGICRegister      `json:"vgic"`
	Devices      map[string]virtio.MMIOState `json:"devices,omitempty"`
}

type arm64KVMVGICRegister struct {
	Group uint32 `json:"group"`
	Attr  uint64 `json:"attr"`
	Value uint64 `json:"value"`
	Size  uint8  `json:"size"`
}

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

func newSnapshotTrigger(dir string, mem []byte) *snapshotTrigger {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &snapshotTrigger{base: arm64vm.SnapshotBase, size: arm64vm.SnapshotSize, dir: strings.TrimSpace(dir), mem: mem}
}

func (s *snapshotTrigger) contains(addr uint64, size int) bool {
	return s != nil && size > 0 && addr >= s.base && addr+uint64(size) <= s.base+s.size
}

func (s *snapshotTrigger) handleMMIO(vm *VM, mmio MMIOExit) (bool, error) {
	if !s.contains(mmio.Addr, int(mmio.Len)) {
		return false, nil
	}
	if mmio.Write {
		if s.dir != "" {
			s.markWrite(mmioValue(mmio))
		}
	} else {
		vm.CompleteMMIORead(snapshotTriggerMagic, mmio.Len)
	}
	return true, nil
}

func (s *snapshotTrigger) markWrite(value uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pending, s.value = true, value
	s.mu.Unlock()
}

func (s *snapshotTrigger) takePending() (uint64, bool) {
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
	w.recent += string(data)
	if len(w.recent) > len(snapshotTriggerSerialMarker)*2 {
		w.recent = w.recent[len(w.recent)-len(snapshotTriggerSerialMarker)*2:]
	}
	if strings.Contains(w.recent, snapshotTriggerSerialMarker) {
		w.trigger.markWrite(snapshotTriggerMagic)
		w.recent = ""
	}
	if w.dst == nil {
		return len(data), nil
	}
	return w.dst.Write(data)
}

func (s *snapshotTrigger) captureIfPending(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG) error {
	if s == nil {
		return nil
	}
	value, ok := s.takePending()
	if !ok {
		return nil
	}
	s.once.Do(func() { s.err = s.capture(vm, fsdevs, vsock, rng, value) })
	if s.err != nil {
		return fmt.Errorf("capture KVM arm64 snapshot: %w", s.err)
	}
	return nil
}

func (s *snapshotTrigger) capture(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, value uint64) error {
	outDir := filepath.Join(s.dir, "snapshot-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := writeArm64SparseFile(filepath.Join(outDir, "memory.bin"), s.mem); err != nil {
		return fmt.Errorf("write memory: %w", err)
	}
	vcpu, err := snapshotArm64KVMVCPU(vm)
	if err != nil {
		return err
	}
	vgic, err := snapshotArm64KVMVGIC(vm)
	if err != nil {
		return err
	}
	manifest := arm64KVMSnapshotManifest{
		Format: arm64KVMSnapshotFormat, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		TriggerBase: s.base, TriggerValue: value, MemoryBase: arm64vm.MemoryBase,
		MemorySize: uint64(len(s.mem)), MemoryFile: "memory.bin", VCPU: vcpu,
		VGICType: vm.vgicType, VGIC: vgic, Devices: snapshotArm64KVMDeviceStates(fsdevs, vsock, rng),
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644)
}

func snapshotArm64KVMVCPU(vm *VM) (map[string][]byte, error) {
	ids, err := getRegList(vm.vcpufd)
	if err != nil {
		return nil, fmt.Errorf("list vCPU registers: %w", err)
	}
	out := make(map[string][]byte, len(ids))
	for _, id := range ids {
		size, err := arm64KVMRegisterSize(id)
		if err != nil {
			return nil, err
		}
		value := make([]byte, size)
		if err := getOneReg(vm.vcpufd, id, unsafe.Pointer(&value[0])); err != nil {
			return nil, fmt.Errorf("read vCPU register %#x: %w", id, err)
		}
		out[strconv.FormatUint(id, 16)] = value
	}
	return out, nil
}

func restoreArm64KVMVCPU(vm *VM, state map[string][]byte) error {
	ids := make([]uint64, 0, len(state))
	for key := range state {
		id, err := strconv.ParseUint(key, 16, 64)
		if err != nil {
			return fmt.Errorf("invalid vCPU register %q", key)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		value := state[strconv.FormatUint(id, 16)]
		if len(value) == 0 {
			return fmt.Errorf("vCPU register %#x is empty", id)
		}
		if err := setOneReg(vm.vcpufd, id, unsafe.Pointer(&value[0])); err != nil {
			return fmt.Errorf("restore vCPU register %#x: %w", id, err)
		}
	}
	return nil
}

func arm64KVMRegisterSize(id uint64) (int, error) {
	shift := (id & 0x00f0000000000000) >> 52
	if shift > 8 {
		return 0, fmt.Errorf("unsupported KVM register size in %#x", id)
	}
	return 1 << shift, nil
}

func snapshotArm64KVMVGIC(vm *VM) ([]arm64KVMVGICRegister, error) {
	if vm.vgicfd < 0 {
		return nil, fmt.Errorf("VGIC is unavailable")
	}
	var out []arm64KVMVGICRegister
	probe32 := func(group uint32, attr uint64) error {
		var value uint32
		err := getDeviceAttr(vm.vgicfd, &kvmDeviceAttr{Group: group, Attr: attr, Addr: uint64(uintptr(unsafe.Pointer(&value)))})
		if errors.Is(err, unix.ENXIO) || errors.Is(err, unix.EINVAL) {
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, arm64KVMVGICRegister{Group: group, Attr: attr, Value: uint64(value), Size: 4})
		return nil
	}
	// Probe only state-bearing migration registers. Reading or writing arbitrary
	// GIC MMIO offsets can acknowledge, deactivate, or synthesize interrupts.
	distRanges := [][2]uint64{{0x0, 0xc}, {0x80, 0xc0}, {0x100, 0x120}, {0x200, 0x220}, {0x300, 0x320}, {0x400, 0x500}, {0x800, 0x900}, {0xc00, 0xc40}, {0xe00, 0xe20}}
	for _, span := range distRanges {
		for off := span[0]; off < span[1]; off += 4 {
			if err := probe32(kvmDevArmVgicGrpDistRegs, off); err != nil {
				return nil, fmt.Errorf("read VGIC distributor %#x: %w", off, err)
			}
		}
	}
	if vm.vgicType == kvmDevTypeArmVgicV2 {
		for _, off := range []uint64{0x0, 0x4, 0x8, 0xd0, 0xd4, 0xd8, 0xdc} {
			if err := probe32(kvmDevArmVgicGrpCPURegs, off); err != nil {
				return nil, fmt.Errorf("read VGIC CPU %#x: %w", off, err)
			}
		}
	} else {
		redistRanges := [][2]uint64{{0x0, 0xc}, {0x14, 0x18}, {0x70, 0x80}, {0x10080, 0x10084}, {0x10100, 0x10104}, {0x10200, 0x10204}, {0x10300, 0x10304}, {0x10400, 0x10420}, {0x10c00, 0x10c08}, {0x10e00, 0x10e04}}
		for _, span := range redistRanges {
			for off := span[0]; off < span[1]; off += 4 {
				if err := probe32(kvmDevArmVgicGrpRedistRegs, off); err != nil {
					return nil, fmt.Errorf("read VGIC redistributor %#x: %w", off, err)
				}
			}
		}
		for _, instr := range []uint64{
			arm64SysRegInstruction(3, 0, 4, 6, 0),
			arm64SysRegInstruction(3, 0, 12, 8, 3),
			arm64SysRegInstruction(3, 0, 12, 8, 4),
			arm64SysRegInstruction(3, 0, 12, 8, 5),
			arm64SysRegInstruction(3, 0, 12, 8, 6),
			arm64SysRegInstruction(3, 0, 12, 8, 7),
			arm64SysRegInstruction(3, 0, 12, 9, 0),
			arm64SysRegInstruction(3, 0, 12, 9, 1),
			arm64SysRegInstruction(3, 0, 12, 9, 2),
			arm64SysRegInstruction(3, 0, 12, 9, 3),
			arm64SysRegInstruction(3, 0, 12, 12, 3),
			arm64SysRegInstruction(3, 0, 12, 12, 4),
			arm64SysRegInstruction(3, 0, 12, 12, 5),
			arm64SysRegInstruction(3, 0, 12, 12, 6),
			arm64SysRegInstruction(3, 0, 12, 12, 7),
		} {
			var value uint64
			attr := kvmDeviceAttr{Group: kvmDevArmVgicGrpCPUSysRegs, Attr: instr, Addr: uint64(uintptr(unsafe.Pointer(&value)))}
			err := getDeviceAttr(vm.vgicfd, &attr)
			if errors.Is(err, unix.ENXIO) || errors.Is(err, unix.EINVAL) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read VGIC CPU sysreg %#x: %w", instr, err)
			}
			out = append(out, arm64KVMVGICRegister{Group: attr.Group, Attr: attr.Attr, Value: value, Size: 8})
		}
	}
	return out, nil
}

func arm64SysRegInstruction(op0, op1, crn, crm, op2 uint64) uint64 {
	return (op0 << 14) | (op1 << 11) | (crn << 7) | (crm << 3) | op2
}

func restoreArm64KVMVGIC(vm *VM, typ uint32, state []arm64KVMVGICRegister) error {
	if typ != vm.vgicType {
		return fmt.Errorf("snapshot VGIC type %d does not match host type %d", typ, vm.vgicType)
	}
	ordered := append([]arm64KVMVGICRegister(nil), state...)
	sort.SliceStable(ordered, func(i, j int) bool {
		// Restore IIDR first and distributor CTLR last, as required by the KVM migration ABI.
		rank := func(v arm64KVMVGICRegister) int {
			if (v.Group == kvmDevArmVgicGrpDistRegs && v.Attr == 0x8) || (v.Group == kvmDevArmVgicGrpRedistRegs && v.Attr == 0x4) {
				return 0
			}
			if (v.Group == kvmDevArmVgicGrpDistRegs || v.Group == kvmDevArmVgicGrpCPURegs || v.Group == kvmDevArmVgicGrpRedistRegs) && v.Attr == 0 {
				return 2
			}
			return 1
		}
		return rank(ordered[i]) < rank(ordered[j])
	})
	for _, reg := range ordered {
		value := reg.Value
		addr := unsafe.Pointer(&value)
		if reg.Size == 4 {
			value = uint64(uint32(value))
		} else if reg.Size != 8 {
			return fmt.Errorf("unsupported VGIC register size %d", reg.Size)
		}
		attr := kvmDeviceAttr{Group: reg.Group, Attr: reg.Attr, Addr: uint64(uintptr(addr))}
		if err := setDeviceAttr(vm.vgicfd, &attr); err != nil && !errors.Is(err, unix.ENXIO) {
			return fmt.Errorf("restore VGIC group %d attr %#x: %w", reg.Group, reg.Attr, err)
		}
	}
	return nil
}

func snapshotArm64KVMDeviceStates(fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG) map[string]virtio.MMIOState {
	out := map[string]virtio.MMIOState{}
	if rng != nil {
		out["rng"] = rng.SnapshotState()
	}
	if vsock != nil {
		out["vsock"] = vsock.SnapshotState()
	}
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			out[fmt.Sprintf("fs:%x", fsdev.Base)] = fsdev.SnapshotState()
		}
	}
	return out
}

func restoreArm64KVMDeviceStates(states map[string]virtio.MMIOState, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG) error {
	if state, ok := states["rng"]; ok && rng != nil {
		if err := rng.RestoreState(state); err != nil {
			return err
		}
	}
	if state, ok := states["vsock"]; ok && vsock != nil {
		if err := vsock.RestoreState(state); err != nil {
			return err
		}
	}
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		key := fmt.Sprintf("fs:%x", fsdev.Base)
		state, ok := states[key]
		if !ok {
			return fmt.Errorf("snapshot missing state for fs device %#x", fsdev.Base)
		}
		if err := fsdev.RestoreState(state); err != nil {
			return fmt.Errorf("restore fs device %#x: %w", fsdev.Base, err)
		}
	}
	return nil
}

func writeArm64SparseFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for off := 0; off < len(data); off += 4096 {
		end := off + 4096
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		zero := true
		for _, b := range chunk {
			if b != 0 {
				zero = false
				break
			}
		}
		if !zero {
			if _, err := f.WriteAt(chunk, int64(off)); err != nil {
				return err
			}
		}
	}
	return f.Truncate(int64(len(data)))
}

func loadArm64KVMSnapshot(path string) (arm64KVMSnapshotManifest, string, error) {
	path = strings.TrimSpace(path)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "manifest.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return arm64KVMSnapshotManifest{}, "", err
	}
	var manifest arm64KVMSnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, "", err
	}
	if manifest.Format != arm64KVMSnapshotFormat {
		return manifest, "", fmt.Errorf("unsupported KVM arm64 snapshot format %q", manifest.Format)
	}
	memPath := manifest.MemoryFile
	if !filepath.IsAbs(memPath) {
		memPath = filepath.Join(filepath.Dir(path), memPath)
	}
	info, err := os.Stat(memPath)
	if err != nil {
		return manifest, "", err
	}
	if uint64(info.Size()) != manifest.MemorySize {
		return manifest, "", fmt.Errorf("snapshot memory size %d, want %d", info.Size(), manifest.MemorySize)
	}
	return manifest, memPath, nil
}

func mmapArm64SnapshotMemory(path string, size uint64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	mem, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	return mem, nil
}

func StartManagedSessionFromSnapshot(ctx context.Context, snapshotPath string, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	stageStart := time.Now()
	if err := emitManagedBootStatus(onEvent, "restoring VM snapshot"); err != nil {
		return nil, err
	}
	manifest, memPath, err := loadArm64KVMSnapshot(snapshotPath)
	if err != nil {
		return nil, err
	}
	timing.Since(ctx, "startup.kvm.restore.load_snapshot", stageStart)
	if memoryMB == 0 {
		memoryMB = manifest.MemorySize >> 20
	}
	if arm64vm.MemorySizeBytes(memoryMB) != manifest.MemorySize {
		return nil, fmt.Errorf("snapshot memory does not match requested memory")
	}
	backend := virtio.NewSimpleVsockBackend()
	listener, err := backend.Listen(vmruntime.ControlPort)
	if err != nil {
		return nil, err
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
	stageStart = time.Now()
	vm, err := NewVM()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	mem, err := mmapArm64SnapshotMemory(memPath, manifest.MemorySize)
	if err == nil {
		err = vm.MapMemory(mem, arm64vm.MemoryBase)
		if err != nil {
			_ = unix.Munmap(mem)
		}
	}
	if err != nil {
		_ = vm.Close()
		_ = listener.Close()
		return nil, err
	}
	timing.Since(ctx, "startup.kvm.restore.vm_memory", stageStart)
	stageStart = time.Now()
	serialOut := vmruntime.NewSerialTranscript()
	var serialWriter io.Writer = serialOut
	var bootWriter *vmruntime.BootEventWriter
	if onEvent != nil {
		bootWriter = vmruntime.NewBootEventWriter(onEvent)
		serialWriter = io.MultiWriter(serialOut, bootWriter)
	}
	uart := serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, serialWriter)
	uart.AttachIRQ(vm, arm64vm.UARTSPI)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Attach(vm, vm)
		}
	}
	vsock.Attach(vm, vm)
	rng.Attach(vm, vm)
	if err := restoreArm64KVMDeviceStates(manifest.Devices, fsdevs, vsock, rng); err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		return nil, err
	}
	timing.Since(ctx, "startup.kvm.restore.devices", stageStart)
	stageStart = time.Now()
	if err := restoreArm64KVMVGIC(vm, manifest.VGICType, manifest.VGIC); err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		return nil, err
	}
	if err := restoreArm64KVMVCPU(vm, manifest.VCPU); err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		return nil, err
	}
	timing.Since(ctx, "startup.kvm.restore.machine_state", stageStart)
	stageStart = time.Now()
	runCtx, cancel := context.WithCancel(context.Background())
	done := newSessionDone()
	go func() {
		resumeTrigger := &snapshotTrigger{base: arm64vm.SnapshotBase, size: arm64vm.SnapshotSize}
		err := runManagedExecVMWithSnapshot(runCtx, vm, uart, fsdevs, vsock, rng, nil, serialOut, resumeTrigger)
		closeVMWithFS(vm, fsdevs)
		done.finish(err)
	}()
	stopRestoreKeepalive := startVsockKeepalive(ctx, vsock, execKeepalive)
	defer stopRestoreKeepalive()
	var control virtio.VsockConn
	select {
	case err := <-acceptErrCh:
		cancel()
		return nil, err
	case control = <-connCh:
	case <-done.done():
		cancel()
		return nil, done.result()
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	timing.Since(ctx, "startup.kvm.restore.vcpu_to_control", stageStart)
	stageStart = time.Now()
	if _, err := controlTranscript.WaitFor(ctx, 0, func(text string) bool {
		return strings.Contains(text, vmruntime.InstanceReadyMarker) || vmruntime.HasFatalBootText(text)
	}); err != nil {
		cancel()
		return nil, err
	}
	timing.Since(ctx, "startup.kvm.restore.control_to_ready", stageStart)
	if vmruntime.HasFatalBootText(controlTranscript.String()) {
		cancel()
		return nil, transcriptError(fmt.Errorf("guest reported boot failure"), serialOut.String(), controlTranscript.String())
	}
	if err := adviseSnapshotMemoryMergeable(vm.mem); err != nil {
		cancel()
		return nil, err
	}
	if err := emitManagedBootStatus(onEvent, "guest ready"); err != nil {
		cancel()
		return nil, err
	}
	return &ManagedSession{cancel: cancel, done: done, control: control, listener: listener, vsock: vsock, fsdevs: fsdevs, bootWriter: bootWriter, transcript: controlTranscript, serialOut: serialOut, cleanup: func() { _ = vm.CancelRun() }, dmesg: dmesg}, nil
}
