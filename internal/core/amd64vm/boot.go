package amd64vm

import (
	"fmt"
	"strings"

	bootamd64 "github.com/tinyrange/crumblecracker/internal/core/linux/boot/amd64"
)

const (
	MemoryBase        = 0
	DefaultMemorySize = 512 << 20
	LowMemorySize     = 3 << 30
	HighMemoryBase    = 4 << 30

	COM1Base = 0x3f8
	COM1IRQ  = 4
)

type BootConfig struct {
	MemoryMB     uint64
	NumCPUs      int
	Dmesg        bool
	ExtraCmdline []string
}

func MemorySizeBytes(memoryMB uint64) uint64 {
	if memoryMB == 0 {
		return DefaultMemorySize
	}
	return memoryMB << 20
}

func LowMemorySizeBytes(memoryMB uint64) uint64 {
	total := MemorySizeBytes(memoryMB)
	if total > LowMemorySize {
		return LowMemorySize
	}
	return total
}

func HighMemorySizeBytes(memoryMB uint64) uint64 {
	total := MemorySizeBytes(memoryMB)
	if total <= LowMemorySize {
		return 0
	}
	return total - LowMemorySize
}

func BootCommandLine(dmesg bool, extra ...string) string {
	args := []string{
		"console=ttyS0,115200n8",
		"nokaslr",
		"panic=-1",
		"rdinit=/init",
	}
	if dmesg {
		args = append([]string{
			fmt.Sprintf("earlycon=uart8250,io,0x%x,115200n8", COM1Base),
			"loglevel=8",
		}, args...)
	}
	for _, arg := range extra {
		if strings.TrimSpace(arg) != "" {
			args = append(args, arg)
		}
	}
	return strings.Join(args, " ")
}

func PrepareBoot(memory []byte, kernel []byte, initrd []byte, cfg BootConfig) (*bootamd64.BootPlan, error) {
	memorySize := MemorySizeBytes(cfg.MemoryMB)
	return bootamd64.PrepareBoot(memory, kernel, bootamd64.BootOptions{
		MemoryBase: MemoryBase,
		MemorySize: memorySize,
		NumCPUs:    cfg.NumCPUs,
		Initrd:     initrd,
		Cmdline:    BootCommandLine(cfg.Dmesg, cfg.ExtraCmdline...),
		E820:       E820Map(memorySize),
	})
}

func E820Map(memorySize uint64) []bootamd64.E820Entry {
	if memorySize <= LowMemorySize {
		return bootamd64.DefaultE820Map(MemoryBase, memorySize)
	}
	entries := bootamd64.DefaultE820Map(MemoryBase, LowMemorySize)
	entries = append(entries, bootamd64.E820Entry{
		Addr: HighMemoryBase,
		Size: memorySize - LowMemorySize,
		Type: 1,
	})
	return entries
}
