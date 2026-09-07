package arm64vm

import (
	"fmt"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fdt"
	bootarm64 "github.com/tinyrange/crumblecracker/internal/core/linux/boot/arm64"
)

type BootConfig struct {
	MemoryMB             uint64
	NumCPUs              int
	GICVersion           bootarm64.GICVersion
	Dmesg                bool
	DisableUART          bool
	DisableSerialConsole bool
	HyperVTimer          bool
	ExtraNodes           []fdt.Node
	RecordTime           func(name string, duration time.Duration)
}

func MemorySizeBytes(memoryMB uint64) uint64 {
	if memoryMB == 0 {
		return DefaultMemorySize
	}
	return memoryMB << 20
}

func BootCommandLine(dmesg bool, serialConsole bool) string {
	args := []string{
		"nokaslr",
		"panic=-1",
		"rdinit=/init",
	}
	if !dmesg {
		args = append(args, "quiet")
	}
	if serialConsole {
		args = append([]string{"console=ttyS0,115200n8"}, args...)
	}
	if dmesg {
		args = append([]string{
			fmt.Sprintf("earlycon=uart8250,mmio,0x%x", bootarm64.DefaultUARTBase),
			"loglevel=8",
		}, args...)
	}
	return strings.Join(args, " ")
}

func PrepareBoot(memory []byte, kernel []byte, initrd []byte, cfg BootConfig) (*bootarm64.BootPlan, error) {
	serialConsole := !cfg.DisableUART && !cfg.DisableSerialConsole
	cmdline := BootCommandLine(cfg.Dmesg, serialConsole)
	return bootarm64.PrepareBoot(memory, kernel, bootarm64.BootOptions{
		MemoryBase:  MemoryBase,
		MemorySize:  MemorySizeBytes(cfg.MemoryMB),
		NumCPUs:     cfg.NumCPUs,
		GICVersion:  cfg.GICVersion,
		Initrd:      initrd,
		DisableUART: cfg.DisableUART,
		Console:     serialConsole,
		HyperVTimer: cfg.HyperVTimer,
		ExtraNodes:  append([]fdt.Node(nil), cfg.ExtraNodes...),
		RecordTime:  cfg.RecordTime,
		Cmdline:     cmdline,
	})
}
