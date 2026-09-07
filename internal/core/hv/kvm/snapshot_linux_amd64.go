//go:build linux && amd64

package kvm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	"github.com/tinyrange/crumblecracker/internal/core/hv/snapshotstore"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
	"golang.org/x/sys/unix"
)

const (
	snapshotTriggerMagic        = 0x43535833534e4150
	snapshotTriggerSerialMarker = "__CCX3_SNAPSHOT__"
	ia32TSCDeadlineMSR          = 0x000006e0
	kvmClockHostTSC             = 1 << 3
)

var kvmSnapshotMSRs = []uint32{
	ia32TSCMSR,
	ia32BIOSSignIDMSR,
	ia32MiscEnableMSR,
	ia32TSCAuxMSR,
	ia32TSCDeadlineMSR,
	0x00000174, // IA32_SYSENTER_CS
	0x00000175, // IA32_SYSENTER_ESP
	0x00000176, // IA32_SYSENTER_EIP
	0x00000277, // IA32_PAT
	0xc0000080, // IA32_EFER
	0xc0000081, // IA32_STAR
	0xc0000082, // IA32_LSTAR
	0xc0000083, // IA32_CSTAR
	0xc0000084, // IA32_FMASK
	0xc0000100, // IA32_FS_BASE
	0xc0000101, // IA32_GS_BASE
	0xc0000102, // IA32_KERNEL_GS_BASE
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

type kvmSnapshotManifest struct {
	Format       string                      `json:"format"`
	Partial      bool                        `json:"partial"`
	CapturedAt   string                      `json:"captured_at"`
	TriggerBase  uint64                      `json:"trigger_base"`
	TriggerValue uint64                      `json:"trigger_value"`
	MemoryBase   uint64                      `json:"memory_base"`
	MemorySize   uint64                      `json:"memory_size"`
	MemoryFile   string                      `json:"memory_file"`
	NumCPUs      int                         `json:"num_cpus"`
	CapturedVCPU int                         `json:"captured_vcpu"`
	VCPU         kvmSnapshotVCPU             `json:"vcpu"`
	IRQChips     []kvmIRQChip                `json:"irq_chips,omitempty"`
	PIT          kvmPITState2                `json:"pit"`
	Clock        kvmClockData                `json:"clock"`
	Devices      map[string]virtio.MMIOState `json:"devices,omitempty"`
	Note         string                      `json:"note"`
}

type kvmSnapshotVCPU struct {
	Regs      kvmRegs       `json:"regs"`
	SRegs     kvmSRegs      `json:"sregs"`
	MSRs      []kvmMSREntry `json:"msrs,omitempty"`
	FPU       kvmFPU        `json:"fpu"`
	LAPIC     kvmLAPICState `json:"lapic"`
	MPState   kvmMPState    `json:"mp_state"`
	Events    kvmVCPUEvents `json:"events"`
	DebugRegs kvmDebugRegs  `json:"debug_regs"`
	XSAVE     *kvmXSAVE     `json:"xsave,omitempty"`
	XSAVEData []byte        `json:"xsave_data,omitempty"`
	XCRS      kvmXCRS       `json:"xcrs"`
}

type kvmSnapshotCacheKey struct {
	path    string
	size    int64
	modTime int64
}

type cachedKVMSnapshot struct {
	manifest kvmSnapshotManifest
	memPath  string
}

var kvmSnapshotCache sync.Map
var kvmSnapshotCacheMu sync.Mutex

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

func (s *snapshotTrigger) handleMMIO(vm *VM, balloon *virtio.Balloon, mmio MMIOExit) (bool, error) {
	if !s.contains(mmio.Addr, int(mmio.Len)) {
		return false, nil
	}
	if mmio.Write {
		s.markWrite(mmioValue(mmio))
		return true, nil
	}
	var ready uint64 = snapshotTriggerMagic
	if balloon != nil && !balloon.AtTarget() {
		ready = 0
	}
	vm.CompleteMMIORead(ready, mmio.Len)
	return true, nil
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

func (s *snapshotTrigger) captureIfPending(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net) error {
	if s == nil {
		return nil
	}
	value, ok := s.takePending()
	if !ok {
		return nil
	}
	s.once.Do(func() {
		s.err = s.capture(vm, fsdevs, vsock, rng, balloon, netdev, value)
	})
	if s.err != nil {
		return fmt.Errorf("capture KVM snapshot: %w", s.err)
	}
	return nil
}

func (s *snapshotTrigger) capture(vm *VM, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net, value uint64) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return nil
	}
	if vm == nil || len(vm.vcpus) != 1 {
		return fmt.Errorf("KVM startup snapshots require exactly one vCPU")
	}
	capture, err := snapshotstore.Begin(s.dir)
	if err != nil {
		return err
	}
	defer capture.Abort()
	outDir := capture.Dir()
	if err := writeSparseFile(filepath.Join(outDir, "memory.bin"), s.mem, 0o600); err != nil {
		return fmt.Errorf("write snapshot memory: %w", err)
	}
	vcpu, err := snapshotKVMVCPU(vm, 0)
	if err != nil {
		return err
	}
	irqChips, err := snapshotKVMIRQChips(vm.vmfd)
	if err != nil {
		return err
	}
	pit, err := getPIT2(vm.vmfd)
	if err != nil {
		return fmt.Errorf("capture KVM pit: %w", err)
	}
	clock, err := getClock(vm.vmfd)
	if err != nil {
		return fmt.Errorf("capture KVM clock: %w", err)
	}
	manifest := kvmSnapshotManifest{
		Format:       "ccx3-kvm-snapshot-v0",
		Partial:      true,
		CapturedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		TriggerBase:  s.base,
		TriggerValue: value,
		MemoryBase:   amd64vm.MemoryBase,
		MemorySize:   uint64(len(s.mem)),
		MemoryFile:   "memory.bin",
		NumCPUs:      len(vm.vcpus),
		CapturedVCPU: 0,
		VCPU:         vcpu,
		IRQChips:     irqChips,
		PIT:          pit,
		Clock:        clock,
		Devices:      snapshotKVMDeviceStates(fsdevs, vsock, rng, balloon, netdev),
		Note:         "KVM Linux checkpoint captured after guest init configured non-unique state and before vsock ready.",
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

func writeSparseFile(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(int64(len(data))); err != nil {
		return err
	}
	pageSize := unix.Getpagesize()
	resident, ok := residentSnapshotPages(data, pageSize)
	if !ok {
		resident = nil
	}
	if err := writePopulatedSnapshotPages(file, data, resident, pageSize); err != nil {
		return err
	}
	return file.Close()
}

// residentSnapshotPages identifies anonymous guest-memory pages that currently
// have backing storage. Non-resident, non-swapped pages in the guest's private
// anonymous mapping have never been populated (or were reclaimed by the
// balloon) and will read as zero, so snapshot capture can avoid faulting and
// scanning the entire configured address space.
func residentSnapshotPages(data []byte, pageSize int) ([]bool, bool) {
	if len(data) == 0 {
		return nil, true
	}
	base := uintptr(unsafe.Pointer(&data[0]))
	if pageSize <= 0 || base%uintptr(pageSize) != 0 {
		return nil, false
	}
	file, err := os.Open("/proc/self/pagemap")
	if err != nil {
		return nil, false
	}
	defer file.Close()

	pageCount := (len(data) + pageSize - 1) / pageSize
	resident := make([]bool, pageCount)
	const entriesPerRead = 32 * 1024
	entries := make([]byte, entriesPerRead*8)
	firstVirtualPage := base / uintptr(pageSize)
	for first := 0; first < pageCount; first += entriesPerRead {
		count := min(entriesPerRead, pageCount-first)
		buf := entries[:count*8]
		offset := int64(firstVirtualPage+uintptr(first)) * 8
		n, err := file.ReadAt(buf, offset)
		if n != len(buf) || (err != nil && err != io.EOF) {
			return nil, false
		}
		for i := 0; i < count; i++ {
			entry := binary.LittleEndian.Uint64(buf[i*8:])
			const presentOrSwapped = uint64(3) << 62
			resident[first+i] = entry&presentOrSwapped != 0
		}
	}
	return resident, true
}

func writePopulatedSnapshotPages(file *os.File, data []byte, resident []bool, pageSize int) error {
	runStart := -1
	flush := func(end int) error {
		if runStart < 0 {
			return nil
		}
		_, err := file.WriteAt(data[runStart:end], int64(runStart))
		runStart = -1
		return err
	}
	for off, page := 0, 0; off < len(data); off, page = off+pageSize, page+1 {
		end := min(off+pageSize, len(data))
		if resident != nil && !resident[page] {
			if err := flush(off); err != nil {
				return err
			}
			continue
		}
		if allZero(data[off:end]) {
			if err := flush(off); err != nil {
				return err
			}
			continue
		}
		if runStart < 0 {
			runStart = off
		}
	}
	return flush(len(data))
}

var zeroSnapshotPage = make([]byte, unix.Getpagesize())

func allZero(data []byte) bool {
	return len(data) <= len(zeroSnapshotPage) && bytes.Equal(data, zeroSnapshotPage[:len(data)])
}

func StartManagedSessionFromSnapshot(ctx context.Context, snapshotPath string, memoryMB uint64, balloonMB uint64, dmesg bool, fsdevs []*virtio.FS, netdev *virtio.Net, onEvent func(client.BootEvent) error) (*ManagedSession, error) {
	if err := emitManagedBootStatus(onEvent, "restoring VM snapshot"); err != nil {
		return nil, err
	}
	manifest, memPath, err := loadKVMSnapshot(snapshotPath)
	if err != nil {
		return nil, err
	}
	if manifest.NumCPUs > 1 {
		return nil, fmt.Errorf("KVM startup snapshots currently support only one vCPU, snapshot has %d", manifest.NumCPUs)
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
	balloon := virtio.NewBalloon(amd64vm.BalloonBase, amd64vm.BalloonSize, amd64vm.BalloonIRQ)
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
	vm, uart, rng, serialOut, err := restoreManagedVMFromSnapshot(manifest, memPath, memoryMB, dmesg, fsdevs, vsock, balloon, netdev, serialWriter)
	if err != nil {
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, err
	}
	if err := retargetRestoredBalloon(balloon, balloonTargetPages(balloonMB)); err != nil {
		closeVMWithFS(vm, fsdevs)
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := newSessionDone()
	go func() {
		err := runManagedExecVM(runCtx, vm, uart, fsdevs, vsock, rng, balloon, netdev, serialOut)
		closeVMWithFS(vm, fsdevs)
		done.finish(err)
	}()
	// A restored guest may block with the control interrupt already pending.
	// Keep pulsing the IRQ until both the control connection and ready marker
	// have arrived, just as managed exec does for later command traffic.
	stopRestoreKeepalive := startVsockKeepalive(ctx, vsock, execKeepalive)
	defer stopRestoreKeepalive()

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
	case <-done.done():
		err := done.result()
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
	if err := adviseSnapshotMemoryMergeable(vm.mem); err != nil {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	}
	if err := emitManagedBootStatus(onEvent, "guest ready"); err != nil {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		vsock.Close()
		if bootWriter != nil {
			_ = bootWriter.Close()
		}
		return nil, transcriptError(err, serialOut.String(), controlTranscript.String())
	}

	return &ManagedSession{
		cancel:     cancel,
		done:       done,
		control:    control,
		listener:   listener,
		vsock:      vsock,
		balloon:    balloon,
		fsdevs:     fsdevs,
		bootWriter: bootWriter,
		cleanup: func() {
			_ = vm.CancelRun()
		},
		transcript: controlTranscript,
		serialOut:  serialOut,
		dmesg:      dmesg,
		inlineExec: true,
	}, nil
}

func restoreManagedVMFromSnapshot(manifest kvmSnapshotManifest, memPath string, memoryMB uint64, dmesg bool, fsdevs []*virtio.FS, vsock *virtio.Vsock, balloon *virtio.Balloon, netdev *virtio.Net, serialWriter io.Writer) (*VM, *serial.UART8250, *virtio.RNG, *vmruntime.SerialTranscript, error) {
	memorySize := amd64vm.MemorySizeBytes(memoryMB)
	if manifest.MemorySize != 0 && manifest.MemorySize != memorySize {
		return nil, nil, nil, nil, fmt.Errorf("snapshot memory size %d does not match requested VM memory %d", manifest.MemorySize, memorySize)
	}
	vm, err := NewVMWithCPUs(1)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	mem, err := mmapSnapshotMemory(memPath, memorySize)
	if err != nil {
		_ = vm.Close()
		return nil, nil, nil, nil, err
	}
	if err := mapAMD64GuestMemoryBytes(vm, memoryMB, mem); err != nil {
		_ = unix.Munmap(mem)
		_ = vm.Close()
		return nil, nil, nil, nil, err
	}

	serialOut := vmruntime.NewSerialTranscript()
	if serialWriter == nil {
		serialWriter = serialOut
	} else {
		serialWriter = io.MultiWriter(serialOut, serialWriter)
	}
	rng := virtio.NewRNG(amd64vm.RNGBase, amd64vm.RNGSize, amd64vm.RNGIRQ)
	for _, fsdev := range fsdevs {
		if fsdev != nil {
			fsdev.Attach(vm, vm)
		}
	}
	if vsock != nil {
		vsock.Attach(vm, vm)
	}
	rng.Attach(vm, vm)
	if balloon != nil {
		balloon.Attach(vm, vm)
	}
	if netdev != nil {
		netdev.Attach(vm, vm)
	}
	if err := restoreKVMDeviceStates(manifest.Devices, fsdevs, vsock, rng, balloon, netdev); err != nil {
		closeVMWithFS(vm, fsdevs)
		return nil, nil, nil, serialOut, err
	}
	if err := restoreKVMIRQChips(vm.vmfd, manifest.IRQChips); err != nil {
		closeVMWithFS(vm, fsdevs)
		return nil, nil, nil, serialOut, err
	}
	if err := setPIT2(vm.vmfd, &manifest.PIT); err != nil {
		closeVMWithFS(vm, fsdevs)
		return nil, nil, nil, serialOut, fmt.Errorf("restore KVM pit: %w", err)
	}
	restoredClock := manifest.Clock
	// host_tsc is an observation from KVM_GET_CLOCK, not a portable clock
	// origin. Replaying it after a later restore can leave LAPIC timers and the
	// guest scheduler based on an obsolete host TSC. Preserve guest time while
	// asking KVM to rebase it against the current host.
	restoredClock.Flags &^= kvmClockHostTSC
	restoredClock.HostTSC = 0
	if err := setClock(vm.vmfd, &restoredClock); err != nil {
		closeVMWithFS(vm, fsdevs)
		return nil, nil, nil, serialOut, fmt.Errorf("restore KVM clock: %w", err)
	}
	if err := restoreKVMVCPU(vm, 0, manifest.VCPU); err != nil {
		closeVMWithFS(vm, fsdevs)
		return nil, nil, nil, serialOut, err
	}
	uart := newAMD64UART(vm, serialWriter)
	return vm, uart, rng, serialOut, nil
}

func loadKVMSnapshot(path string) (kvmSnapshotManifest, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return kvmSnapshotManifest{}, "", fmt.Errorf("snapshot path is required")
	}
	manifestPath := path
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		manifestPath = filepath.Join(path, "manifest.json")
	}
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		return kvmSnapshotManifest{}, "", fmt.Errorf("stat snapshot manifest: %w", err)
	}
	cacheKey := kvmSnapshotCacheKey{path: manifestPath, size: manifestInfo.Size(), modTime: manifestInfo.ModTime().UnixNano()}
	if cached, ok := kvmSnapshotCache.Load(cacheKey); ok {
		snapshot := cached.(cachedKVMSnapshot)
		return snapshot.manifest, snapshot.memPath, nil
	}
	kvmSnapshotCacheMu.Lock()
	defer kvmSnapshotCacheMu.Unlock()
	if cached, ok := kvmSnapshotCache.Load(cacheKey); ok {
		snapshot := cached.(cachedKVMSnapshot)
		return snapshot.manifest, snapshot.memPath, nil
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return kvmSnapshotManifest{}, "", fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest kvmSnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return kvmSnapshotManifest{}, "", fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.Format != "ccx3-kvm-snapshot-v0" {
		return kvmSnapshotManifest{}, "", fmt.Errorf("unsupported KVM snapshot format %q", manifest.Format)
	}
	memPath, err := vmruntime.ResolveSnapshotMemoryPath(manifestPath, manifest.MemoryFile)
	if err != nil {
		return kvmSnapshotManifest{}, "", err
	}
	info, err := os.Stat(memPath)
	if err != nil {
		return kvmSnapshotManifest{}, "", fmt.Errorf("stat snapshot memory: %w", err)
	}
	if info.Size() <= 0 {
		return kvmSnapshotManifest{}, "", fmt.Errorf("snapshot memory is empty")
	}
	if manifest.MemorySize != 0 && uint64(info.Size()) != manifest.MemorySize {
		return kvmSnapshotManifest{}, "", fmt.Errorf("snapshot memory size = %d, want %d", info.Size(), manifest.MemorySize)
	}
	kvmSnapshotCache.Store(cacheKey, cachedKVMSnapshot{manifest: manifest, memPath: memPath})
	return manifest, memPath, nil
}

func mmapSnapshotMemory(path string, size uint64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open snapshot memory: %w", err)
	}
	defer file.Close()
	mem, err := unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap snapshot memory: %w", err)
	}
	return mem, nil
}

func snapshotKVMVCPU(vm *VM, index int) (kvmSnapshotVCPU, error) {
	vcpu, err := kvmVCPU(vm, index)
	if err != nil {
		return kvmSnapshotVCPU{}, err
	}
	regs, err := getRegs(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM regs: %w", err)
	}
	sregs, err := getSRegs(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM sregs: %w", err)
	}
	fpu, err := getFPU(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM fpu: %w", err)
	}
	lapic, err := getLAPIC(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM lapic: %w", err)
	}
	mpState, err := getMPState(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM mp state: %w", err)
	}
	events, err := getVCPUEvents(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM vcpu events: %w", err)
	}
	debugRegs, err := getDebugRegs(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM debug regs: %w", err)
	}
	xsave, err := getXSAVEBytes(vm.vmfd, vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM xsave: %w", err)
	}
	xcrs, err := getXCRS(vcpu.fd)
	if err != nil {
		return kvmSnapshotVCPU{}, fmt.Errorf("capture KVM xcrs: %w", err)
	}
	return kvmSnapshotVCPU{
		Regs:      regs,
		SRegs:     sregs,
		MSRs:      snapshotKVMMSRs(vcpu.fd),
		FPU:       fpu,
		LAPIC:     lapic,
		MPState:   mpState,
		Events:    events,
		DebugRegs: debugRegs,
		XSAVEData: xsave,
		XCRS:      xcrs,
	}, nil
}

func restoreKVMVCPU(vm *VM, index int, state kvmSnapshotVCPU) error {
	vcpu, err := kvmVCPU(vm, index)
	if err != nil {
		return err
	}
	if err := setSRegs(vcpu.fd, &state.SRegs); err != nil {
		return fmt.Errorf("restore KVM sregs: %w", err)
	}
	var deadlineMSR *kvmMSREntry
	regularMSRs := make([]kvmMSREntry, 0, len(state.MSRs))
	for i := range state.MSRs {
		if state.MSRs[i].Index == ia32TSCDeadlineMSR {
			entry := state.MSRs[i]
			deadlineMSR = &entry
			continue
		}
		regularMSRs = append(regularMSRs, state.MSRs[i])
	}
	if err := restoreKVMMSRs(vcpu.fd, regularMSRs); err != nil {
		return err
	}
	if err := setFPU(vcpu.fd, &state.FPU); err != nil {
		return fmt.Errorf("restore KVM fpu: %w", err)
	}
	if err := setXCRS(vcpu.fd, &state.XCRS); err != nil {
		return fmt.Errorf("restore KVM xcrs: %w", err)
	}
	xsave := state.XSAVEData
	if len(xsave) == 0 {
		xsave = legacyXSAVEBytes(state.XSAVE)
	}
	if err := setXSAVEBytes(vm.vmfd, vcpu.fd, xsave); err != nil {
		return fmt.Errorf("restore KVM xsave: %w", err)
	}
	if err := setLAPIC(vcpu.fd, &state.LAPIC); err != nil {
		return fmt.Errorf("restore KVM lapic: %w", err)
	}
	if err := setMPState(vcpu.fd, &state.MPState); err != nil {
		return fmt.Errorf("restore KVM mp state: %w", err)
	}
	if err := setVCPUEvents(vcpu.fd, &state.Events); err != nil {
		return fmt.Errorf("restore KVM vcpu events: %w", err)
	}
	if err := setDebugRegs(vcpu.fd, &state.DebugRegs); err != nil {
		return fmt.Errorf("restore KVM debug regs: %w", err)
	}
	if err := setRegs(vcpu.fd, &state.Regs); err != nil {
		return fmt.Errorf("restore KVM regs: %w", err)
	}
	// KVM derives deadline-timer state from both the LAPIC and this MSR. Write
	// the deadline after restoring the LAPIC so it cannot be reset by SET_LAPIC.
	if deadlineMSR != nil {
		if err := setVCPUMSR(vcpu.fd, deadlineMSR.Index, deadlineMSR.Data); err != nil {
			return fmt.Errorf("restore KVM TSC deadline: %w", err)
		}
	}
	return nil
}

func kvmVCPU(vm *VM, index int) (*VCPU, error) {
	if vm == nil || index < 0 || index >= len(vm.vcpus) {
		return nil, fmt.Errorf("vcpu %d out of range", index)
	}
	vcpu := vm.vcpus[index]
	if vcpu == nil || vcpu.fd < 0 {
		return nil, fmt.Errorf("vcpu %d is closed", index)
	}
	return vcpu, nil
}

func snapshotKVMMSRs(vcpuFd int) []kvmMSREntry {
	out := make([]kvmMSREntry, 0, len(kvmSnapshotMSRs))
	for _, index := range kvmSnapshotMSRs {
		value, err := getVCPUMSR(vcpuFd, index)
		if err != nil {
			continue
		}
		out = append(out, kvmMSREntry{Index: index, Data: value})
	}
	return out
}

func restoreKVMMSRs(vcpuFd int, entries []kvmMSREntry) error {
	for _, entry := range entries {
		if err := setVCPUMSR(vcpuFd, entry.Index, entry.Data); err != nil {
			return fmt.Errorf("restore KVM msr %s: %w", strconv.FormatUint(uint64(entry.Index), 16), err)
		}
	}
	return nil
}

func snapshotKVMIRQChips(vmFd int) ([]kvmIRQChip, error) {
	const kvmNrIRQChips = 3
	chips := make([]kvmIRQChip, 0, kvmNrIRQChips)
	for chipID := uint32(0); chipID < kvmNrIRQChips; chipID++ {
		chip, err := getIRQChip(vmFd, chipID)
		if err != nil {
			return nil, fmt.Errorf("capture KVM irqchip %d: %w", chipID, err)
		}
		chips = append(chips, chip)
	}
	return chips, nil
}

func restoreKVMIRQChips(vmFd int, chips []kvmIRQChip) error {
	for i := range chips {
		chip := chips[i]
		if err := setIRQChip(vmFd, &chip); err != nil {
			return fmt.Errorf("restore KVM irqchip %d: %w", chip.ChipID, err)
		}
	}
	return nil
}

func snapshotKVMDeviceStates(fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net) map[string]virtio.MMIOState {
	out := map[string]virtio.MMIOState{}
	if rng != nil {
		out["rng"] = rng.SnapshotState()
	}
	if vsock != nil {
		out["vsock"] = vsock.SnapshotState()
	}
	if balloon != nil {
		out["balloon"] = balloon.SnapshotState()
	}
	if netdev != nil {
		out["net"] = netdev.SnapshotState()
	}
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		out[kvmFSDeviceStateKey(fsdev)] = fsdev.SnapshotState()
	}
	return out
}

func restoreKVMDeviceStates(states map[string]virtio.MMIOState, fsdevs []*virtio.FS, vsock *virtio.Vsock, rng *virtio.RNG, balloon *virtio.Balloon, netdev *virtio.Net) error {
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
	if state, ok := states["balloon"]; ok && balloon != nil {
		if err := balloon.RestoreState(state); err != nil {
			return fmt.Errorf("restore balloon state: %w", err)
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
		key := kvmFSDeviceStateKey(fsdev)
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

func retargetRestoredBalloon(balloon *virtio.Balloon, target uint32) error {
	if balloon == nil {
		return nil
	}
	if err := balloon.SetTargetPages(target); err != nil {
		return fmt.Errorf("retarget restored balloon: %w", err)
	}
	return nil
}

func kvmFSDeviceStateKey(fsdev *virtio.FS) string {
	if fsdev == nil {
		return "fs@0"
	}
	return "fs@" + strconv.FormatUint(fsdev.Base, 16)
}
