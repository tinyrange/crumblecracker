//go:build windows && amd64

package whp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/linux/initramfs"
)

func TestLinuxBootsWithMultipleWHPVCPUs(t *testing.T) {
	if os.Getenv("CC_TEST_WINDOWS_WHP_SMP") == "" {
		t.Skip("set CC_TEST_WINDOWS_WHP_SMP=1 to run the Windows WHP SMP boot test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cacheRoot := filepath.Join(t.TempDir(), "kernel")
	if existing := strings.TrimSpace(os.Getenv("CC_TEST_KERNEL_CACHE")); existing != "" {
		cacheRoot = existing
	}
	kernelManager := alpine.NewManager(cacheRoot)
	if err := kernelManager.EnsureWithProgress(ctx, nil); err != nil {
		t.Fatalf("prepare Alpine Linux kernel: %v", err)
	}
	kernel, err := kernelManager.ReadKernel()
	if err != nil {
		t.Fatalf("read kernel: %v", err)
	}
	initrd := buildWHPSMPInitramfs(t)

	const (
		cpus   = 4
		marker = "WHP_SMP_CPUS=0-3"
	)
	serial, err := BootInitramfsToMarkerWithFSAndNetAndCPUs(ctx, kernel, initrd, 512, cpus, true, marker, nil, nil)
	if err != nil {
		t.Fatalf("boot %d-vCPU VM: %v\nserial:\n%s", cpus, err, serial)
	}
	if line := findSMPSerialLine(serial); line != marker {
		t.Fatalf("online CPU marker = %q, want %q", line, marker)
	}
}

func findSMPSerialLine(serial string) string {
	for _, line := range strings.Split(serial, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "WHP_SMP_CPUS=") {
			return line
		}
	}
	return ""
}

func buildWHPSMPInitramfs(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "init.go")
	if err := os.WriteFile(src, []byte(whpSMPInitSource), 0o644); err != nil {
		t.Fatalf("write SMP init source: %v", err)
	}
	initPath := filepath.Join(dir, "init")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", initPath, src)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build SMP init: %v\n%s", err, output)
	}
	initBin, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatalf("read SMP init: %v", err)
	}
	initrd, err := initramfs.Build([]initramfs.File{
		{Path: "/dev", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/dev/console", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 5, DevMinor: 1},
		{Path: "/dev/null", Mode: 0o666, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 3},
		{Path: "/sys", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/init", Mode: 0o755, Data: initBin, Type: initramfs.TypeRegular},
	})
	if err != nil {
		t.Fatalf("build SMP initramfs: %v", err)
	}
	return initrd
}

const whpSMPInitSource = `package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func main() {
	if err := syscall.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		fmt.Printf("WHP_SMP_ERROR=mount-sysfs:%v\n", err)
		for {
			_ = syscall.Pause()
		}
	}
	online, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		fmt.Printf("WHP_SMP_ERROR=read-online:%v\n", err)
		for {
			_ = syscall.Pause()
		}
	}
	fmt.Printf("WHP_SMP_CPUS=%s\n", strings.TrimSpace(string(online)))
	for {
		_ = syscall.Pause()
	}
}
`
