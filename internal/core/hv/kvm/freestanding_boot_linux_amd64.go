//go:build linux && amd64

package kvm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/amd64vm"
	bootamd64 "github.com/tinyrange/crumblecracker/internal/core/freestanding/boot/amd64"
	"golang.org/x/sys/unix"
)

// BootFreestandingELFToMarker directly loads a higher-half ELF kernel, enters
// it in long mode, and captures COM1 until marker is printed.
func BootFreestandingELFToMarker(ctx context.Context, kernel []byte, memoryMB uint64, marker string) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("boot marker is required")
	}
	vm, err := NewVM()
	if err != nil {
		return "", err
	}
	defer vm.Close()

	memory, err := mapAMD64GuestMemory(vm, memoryMB)
	if err != nil {
		return "", fmt.Errorf("map guest memory: %w", err)
	}
	plan, err := bootamd64.PrepareBoot(memory, kernel, bootamd64.BootOptions{
		MemorySize: amd64vm.MemorySizeBytes(memoryMB),
	})
	if err != nil {
		return "", fmt.Errorf("prepare freestanding boot: %w", err)
	}
	if err := vm.SetFreestandingLongMode(plan.EntryGVA, plan.BootInfoGPA, plan.StackTopGPA, plan.PagingGPA); err != nil {
		return "", fmt.Errorf("set freestanding long mode: %w", err)
	}
	if got := binary.LittleEndian.Uint64(memory[plan.PagingGPA+0x2000+2*8:]); got != (plan.PagingGPA+0x3000)|7 {
		return "", fmt.Errorf("invalid bootstrap user PDE %#x", got)
	}

	var serialOut bytes.Buffer
	uart := newAMD64UART(vm, &serialOut)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	vm.SetVCPUTID(0, unix.Gettid())
	defer vm.SetVCPUTID(0, 0)
	cancelDone := make(chan struct{})
	defer close(cancelDone)
	go func() {
		select {
		case <-ctx.Done():
			vm.RequestImmediateExit()
		case <-cancelDone:
		}
	}()

	var exit Exit
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return serialOut.String(), err
		}
		if err := vm.RunVCPUInterruptible(0, &exit); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return serialOut.String(), fmt.Errorf("run step %d: %w", step, err)
		}
		if strings.Contains(serialOut.String(), marker) {
			return serialOut.String(), nil
		}
		switch exit.Reason {
		case ExitIO:
			if err := handleBootIO(uart, exit.IO); err != nil {
				return serialOut.String(), err
			}
		case ExitHLT:
			return serialOut.String(), fmt.Errorf("freestanding guest halted before marker")
		case ExitShutdown:
			pc, _ := vm.GetPC()
			fault, _ := vm.GetFaultAddress()
			registers := vm.VCPURegisters(0)
			faultEntries := traceFreestandingPageTables(memory, plan.PagingGPA, fault)
			code := []byte(nil)
			if physical, ok := walkFreestandingPageTables(memory, plan.PagingGPA, pc); ok && physical < uint64(len(memory)) {
				end := physical + 8
				if end > uint64(len(memory)) {
					end = uint64(len(memory))
				}
				code = memory[physical:end]
			}
			return serialOut.String(), fmt.Errorf("freestanding guest shut down before marker at pc=%#x fault=%#x code=% x pages=%#x registers=%v", pc, fault, code, faultEntries, registers)
		case ExitSystemEvent:
			return serialOut.String(), fmt.Errorf("unexpected system event %d before marker", exit.SystemEvent)
		default:
			pc, _ := vm.GetPC()
			return serialOut.String(), fmt.Errorf("unexpected exit reason %d at pc=%#x", exit.Reason, pc)
		}
		if strings.Contains(serialOut.String(), marker) {
			return serialOut.String(), nil
		}
	}
}

func traceFreestandingPageTables(memory []byte, root uint64, virtual uint64) [4]uint64 {
	var entries [4]uint64
	indices := [4]uint64{(virtual >> 39) & 511, (virtual >> 30) & 511, (virtual >> 21) & 511, (virtual >> 12) & 511}
	table := root
	for level := 0; level < len(entries); level++ {
		address := table + indices[level]*8
		if address > uint64(len(memory)) || 8 > uint64(len(memory))-address {
			break
		}
		entries[level] = binary.LittleEndian.Uint64(memory[address : address+8])
		if entries[level]&1 == 0 || level == 2 && entries[level]&(1<<7) != 0 {
			break
		}
		table = entries[level] & 0x000ffffffffff000
	}
	return entries
}

func walkFreestandingPageTables(memory []byte, root uint64, virtual uint64) (uint64, bool) {
	read := func(address uint64) (uint64, bool) {
		if address > uint64(len(memory)) || 8 > uint64(len(memory))-address {
			return 0, false
		}
		return binary.LittleEndian.Uint64(memory[address : address+8]), true
	}
	entry, ok := read(root + ((virtual>>39)&511)*8)
	if !ok || entry&1 == 0 {
		return 0, false
	}
	entry, ok = read(entry&0x000ffffffffff000 + ((virtual>>30)&511)*8)
	if !ok || entry&1 == 0 {
		return 0, false
	}
	entry, ok = read(entry&0x000ffffffffff000 + ((virtual>>21)&511)*8)
	if !ok || entry&1 == 0 {
		return 0, false
	}
	if entry&(1<<7) != 0 {
		return entry&0x000fffffffe00000 + (virtual & 0x1fffff), true
	}
	entry, ok = read(entry&0x000ffffffffff000 + ((virtual>>12)&511)*8)
	if !ok || entry&1 == 0 {
		return 0, false
	}
	return entry&0x000ffffffffff000 + (virtual & 0xfff), true
}
