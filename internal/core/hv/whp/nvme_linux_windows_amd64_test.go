//go:build windows && amd64

package whp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/linux/initramfs"
	"github.com/tinyrange/crumblecracker/internal/core/nvme"
)

func TestLinuxBootsWithWHPNVMeBlock(t *testing.T) {
	if os.Getenv("CC_TEST_WINDOWS_WHP_NVME_LINUX") == "" {
		t.Skip("set CC_TEST_WINDOWS_WHP_NVME_LINUX=1 to run Windows WHP Linux NVMe block smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	kernel, modules := prepareLinuxWHPNVMeKernel(t, ctx)
	initrd := buildLinuxWHPNVMeInitramfs(t, modules, false)

	disk := newWHPNVMeLinuxDisk(16 * 1024 * 1024)
	ctrl := nvme.NewController(disk)
	serialOut, err := BootInitramfsToMarkerWithNVMeBlock(ctx, kernel, initrd, 512, true, "WHP_NVME_LINUX_OK", ctrl)
	if err != nil {
		t.Fatalf("boot Linux with WHP NVMe: %v\nserial:\n%s", err, serialOut)
	}
	if got := disk.stringAt(512, len("cc-whp-nvme\n")); got != "cc-whp-nvme\n" {
		t.Fatalf("disk write = %q", got)
	}
	if !disk.hasAt(1024, "WHP_NVME_LINUX_OK\n") {
		t.Fatalf("disk missing success marker; serial:\n%s", serialOut)
	}
}

func TestLinuxWHPNVMeBlockPerformance(t *testing.T) {
	if os.Getenv("CC_TEST_WINDOWS_WHP_NVME_LINUX_PERF") == "" {
		t.Skip("set CC_TEST_WINDOWS_WHP_NVME_LINUX_PERF=1 to run Windows WHP Linux NVMe block performance test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	kernel, modules := prepareLinuxWHPNVMeKernel(t, ctx)
	initrd := buildLinuxWHPNVMeInitramfs(t, modules, true)

	disk := newWHPNVMeLinuxDisk(272 * 1024 * 1024)
	ctrl := nvme.NewController(disk)
	serialOut, err := BootInitramfsToMarkerWithNVMeBlock(ctx, kernel, initrd, 512, true, "WHP_NVME_LINUX_PERF_OK", ctrl)
	if err != nil {
		t.Fatalf("boot Linux with WHP NVMe perf: %v\nserial:\n%s", err, serialOut)
	}
	if line := findSerialLine(serialOut, "BLOCK_PERF "); line != "" {
		t.Log(strings.Replace(line, "BLOCK_PERF", "NVME_PERF", 1))
	} else {
		t.Fatalf("missing BLOCK_PERF line\nserial:\n%s", serialOut)
	}
	for _, prefix := range []string{"max_hw_sectors_kb=", "max_sectors_kb=", "max_sectors_kb_after="} {
		if line := findSerialLine(serialOut, prefix); line != "" {
			t.Log(line)
		}
	}
	t.Log(disk.stats("NVMe"))
}

func prepareLinuxWHPNVMeKernel(t *testing.T, ctx context.Context) ([]byte, []alpine.Module) {
	t.Helper()
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
	modules, err := kernelManager.PlanModuleLoad(
		[]string{"CONFIG_BLK_DEV_NVME"},
		map[string]string{"CONFIG_BLK_DEV_NVME": "kernel/drivers/nvme/host/nvme.ko.gz"},
	)
	if err != nil {
		t.Fatalf("plan Linux NVMe modules: %v", err)
	}
	return kernel, modules
}

func buildLinuxWHPNVMeInitramfs(t *testing.T, modules []alpine.Module, perf bool) []byte {
	t.Helper()
	initBin := buildLinuxWHPNVMeInit(t)
	files := []initramfs.File{
		{Path: "/dev", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/proc", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/sys", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/modules", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/dev/console", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 5, DevMinor: 1},
		{Path: "/dev/kmsg", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 11},
		{Path: "/dev/null", Mode: 0o666, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 3},
		{Path: "/init", Mode: 0o755, Data: initBin, Type: initramfs.TypeRegular},
		{Path: "/block-prefix", Mode: 0o644, Data: []byte("nvme\n"), Type: initramfs.TypeRegular},
	}
	var loadOrder strings.Builder
	for _, mod := range modules {
		files = append(files, initramfs.File{
			Path: "/modules/" + mod.Name + ".ko",
			Mode: 0o644,
			Data: mod.Data,
			Type: initramfs.TypeRegular,
		})
		files = append(files, initramfs.File{
			Path: "/modules/" + mod.Name,
			Mode: 0o644,
			Data: mod.Data,
			Type: initramfs.TypeRegular,
		})
		loadOrder.WriteString(mod.Name)
		loadOrder.WriteString(".ko\n")
	}
	files = append(files, initramfs.File{
		Path: "/modules/load-order",
		Mode: 0o644,
		Data: []byte(loadOrder.String()),
		Type: initramfs.TypeRegular,
	})
	if perf {
		files = append(files, initramfs.File{
			Path: "/perf-mode",
			Mode: 0o644,
			Data: []byte("1\n"),
			Type: initramfs.TypeRegular,
		})
		if chunk := strings.TrimSpace(os.Getenv("CC_TEST_LINUX_BLOCK_PERF_CHUNK")); chunk != "" {
			files = append(files, initramfs.File{
				Path: "/perf-chunk",
				Mode: 0o644,
				Data: []byte(chunk + "\n"),
				Type: initramfs.TypeRegular,
			})
		}
	}
	initrd, err := initramfs.Build(files)
	if err != nil {
		t.Fatalf("build initramfs: %v", err)
	}
	return initrd
}

func buildLinuxWHPNVMeInit(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "init.go")
	if err := os.WriteFile(src, []byte(linuxWHPNVMeInitSource), 0o644); err != nil {
		t.Fatalf("write init source: %v", err)
	}
	out := filepath.Join(dir, "init")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, src)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build init: %v\n%s", err, data)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	return data
}

func findSerialLine(serial, prefix string) string {
	for _, line := range strings.Split(serial, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

type whpNVMeLinuxDisk struct {
	mu         sync.Mutex
	size       int64
	data       []byte
	readOps    uint64
	readBytes  uint64
	writeOps   uint64
	writeBytes uint64
}

func newWHPNVMeLinuxDisk(size int) *whpNVMeLinuxDisk {
	return &whpNVMeLinuxDisk{size: int64(size)}
}

func (d *whpNVMeLinuxDisk) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off >= d.size {
		return 0, nil
	}
	if max := d.size - off; int64(len(p)) > max {
		p = p[:max]
	}
	n := 0
	if off < int64(len(d.data)) {
		n = copy(p, d.data[off:])
	}
	clear(p[n:])
	d.readOps++
	d.readBytes += uint64(len(p))
	return len(p), nil
}

func (d *whpNVMeLinuxDisk) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off >= d.size {
		return 0, nil
	}
	if max := d.size - off; int64(len(p)) > max {
		p = p[:max]
	}
	if d.data == nil {
		d.data = make([]byte, int(d.size))
	}
	end := int(off) + len(p)
	if len(d.data) < end {
		d.data = d.data[:end]
	}
	n := copy(d.data[off:], p)
	d.writeOps++
	d.writeBytes += uint64(n)
	return n, nil
}

func (d *whpNVMeLinuxDisk) Size() int64 {
	return d.size
}

func (d *whpNVMeLinuxDisk) hasAt(off int64, value string) bool {
	return d.stringAt(off, len(value)) == value
}

func (d *whpNVMeLinuxDisk) stringAt(off int64, size int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off+int64(size) > d.size {
		return ""
	}
	buf := make([]byte, size)
	if off < int64(len(d.data)) {
		copy(buf, d.data[off:])
	}
	return string(buf)
}

func (d *whpNVMeLinuxDisk) stats(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	avgRead := uint64(0)
	if d.readOps > 0 {
		avgRead = d.readBytes / d.readOps
	}
	avgWrite := uint64(0)
	if d.writeOps > 0 {
		avgWrite = d.writeBytes / d.writeOps
	}
	return fmt.Sprintf("%s backend reads=%d read_bytes=%d avg_read=%d writes=%d write_bytes=%d avg_write=%d",
		name, d.readOps, d.readBytes, avgRead, d.writeOps, d.writeBytes, avgWrite)
}

const linuxWHPNVMeInitSource = `package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func logf(format string, args ...any) {
	msg := fmt.Sprintf(format+"\n", args...)
	_, _ = os.Stdout.WriteString(msg)
	if f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); err == nil {
		_, _ = f.WriteString(msg)
		_ = f.Close()
	}
}

func main() {
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	_ = syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	if f, err := os.OpenFile("/dev/console", os.O_RDWR, 0); err == nil {
		_ = dup2(int(f.Fd()), 0)
		_ = dup2(int(f.Fd()), 1)
		_ = dup2(int(f.Fd()), 2)
	}
	logf("init starting")
	loadModules()
	dev, prefix, err := waitBlock()
	if err != nil {
		logf("wait %s block: %v", prefix, err)
		stayAlive()
	}
	if err := makeNode(dev); err != nil {
		logf("mknod %s: %v", dev, err)
		stayAlive()
	}
	tuneBlockQueue(dev)
	path := "/dev/" + dev
	perfMode := fileExists("/perf-mode")
	flags := os.O_RDWR | syscall.O_SYNC
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		logf("open %s: %v", path, err)
		stayAlive()
	}
	payload := []byte("cc-whp-nvme\n")
	if _, err := f.WriteAt(payload, 512); err != nil {
		logf("write payload: %v", err)
		stayAlive()
	}
	buf := make([]byte, len(payload))
	if _, err := f.ReadAt(buf, 512); err != nil {
		logf("read payload: %v", err)
		stayAlive()
	}
	if !bytes.Equal(buf, payload) {
		logf("readback mismatch got=%q", string(buf))
		stayAlive()
	}
	if perfMode {
		if err := f.Close(); err != nil {
			logf("close smoke device: %v", err)
			stayAlive()
		}
		f, err = os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT, 0)
		if err != nil {
			logf("open direct %s: %v", path, err)
			stayAlive()
		}
		runPerf(f)
		if err := f.Close(); err != nil {
			logf("close direct device: %v", err)
			stayAlive()
		}
		f, err = os.OpenFile(path, flags, 0)
		if err != nil {
			logf("reopen %s: %v", path, err)
			stayAlive()
		}
	}
	success := []byte("WHP_NVME_LINUX_OK\n")
	if _, err := f.WriteAt(success, 1024); err != nil {
		logf("write success: %v", err)
		stayAlive()
	}
	_ = f.Sync()
	logf("WHP_NVME_LINUX_OK")
	stayAlive()
}

func runPerf(f *os.File) {
	const (
		offset = 4 * 1024 * 1024
		total = 256 * 1024 * 1024
	)
	chunk := readPerfChunk(64 * 1024 * 1024)
	buf, err := directBuffer(chunk)
	if err != nil {
		logf("direct buffer: %v", err)
		stayAlive()
	}
	for i := range buf {
		buf[i] = byte(i)
	}
	warmPerfRange(f, buf)
	start := time.Now()
	for written := 0; written < total; written += len(buf) {
		if _, err := f.WriteAt(buf, int64(offset+written)); err != nil {
			logf("perf write: %v", err)
			stayAlive()
		}
	}
	writeElapsed := time.Since(start)
	if err := f.Sync(); err != nil {
		logf("perf sync: %v", err)
		stayAlive()
	}
	_ = os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0)
	clear(buf)
	start = time.Now()
	var checksum uint64
	for read := 0; read < total; read += len(buf) {
		if _, err := f.ReadAt(buf, int64(offset+read)); err != nil {
			logf("perf read: %v", err)
			stayAlive()
		}
		if len(buf) > 0 {
			checksum += uint64(buf[0])
			checksum += uint64(buf[len(buf)-1])
		}
	}
	readElapsed := time.Since(start)
	logf("BLOCK_PERF bytes=%d write_ms=%d write_mib_s=%.2f read_ms=%d read_mib_s=%.2f chunk=%d checksum=%d",
		total,
		writeElapsed.Milliseconds(), mibPerSecond(total, writeElapsed),
		readElapsed.Milliseconds(), mibPerSecond(total, readElapsed),
		chunk,
		checksum,
	)
	logf("WHP_NVME_LINUX_PERF_OK")
}

func warmPerfRange(f *os.File, buf []byte) {
	const (
		offset = 4 * 1024 * 1024
		total = 256 * 1024 * 1024
	)
	for written := 0; written < total; written += len(buf) {
		if _, err := f.WriteAt(buf, int64(offset+written)); err != nil {
			logf("warm write: %v", err)
			stayAlive()
		}
	}
	if err := f.Sync(); err != nil {
		logf("warm sync: %v", err)
		stayAlive()
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func tuneBlockQueue(dev string) {
	dir := filepath.Join("/sys/block", dev, "queue")
	for _, name := range []string{"max_hw_sectors_kb", "max_sectors_kb", "logical_block_size", "physical_block_size"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			logf("%s=%s", name, strings.TrimSpace(string(data)))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "max_sectors_kb"), []byte("8192\n"), 0); err != nil {
		logf("set max_sectors_kb: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "max_sectors_kb")); err == nil {
		logf("max_sectors_kb_after=%s", strings.TrimSpace(string(data)))
	}
}

func readPerfChunk(defaultSize int) int {
	data, err := os.ReadFile("/perf-chunk")
	if err != nil {
		return defaultSize
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value <= 0 {
		return defaultSize
	}
	return value
}

func directBuffer(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
}

func mibPerSecond(bytes int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(bytes) / 1024 / 1024 / elapsed.Seconds()
}

func loadModules() {
	data, err := os.ReadFile("/modules/load-order")
	if err != nil {
		logf("read load-order: %v", err)
		return
	}
	for _, name := range strings.Fields(string(data)) {
		mod, err := os.ReadFile("/modules/" + name)
		if err != nil {
			logf("read module %s: %v", name, err)
			continue
		}
		if err := initModule(mod); err != nil && err != syscall.EEXIST {
			logf("load module %s: %v", name, err)
		}
	}
}

func initModule(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	params := []byte{0}
	_, _, errno := syscall.Syscall(syscall.SYS_INIT_MODULE, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&params[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

func dup2(oldfd, newfd int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_DUP3, uintptr(oldfd), uintptr(newfd), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func waitBlock() (string, string, error) {
	prefix := "nvme"
	if data, err := os.ReadFile("/block-prefix"); err == nil {
		if value := strings.TrimSpace(string(data)); value != "" {
			prefix = value
		}
	}
	for i := 0; i < 400; i++ {
		entries, err := os.ReadDir("/sys/block")
		if err == nil {
			for _, entry := range entries {
				name := entry.Name()
				if strings.HasPrefix(name, prefix) {
					return name, prefix, nil
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return "", prefix, fmt.Errorf("not found")
}

func makeNode(name string) error {
	data, err := os.ReadFile(filepath.Join("/sys/block", name, "dev"))
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(string(data)), ":")
	if len(parts) != 2 {
		return fmt.Errorf("bad dev file %q", string(data))
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return err
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return err
	}
	_ = os.Remove("/dev/" + name)
	return syscall.Mknod("/dev/"+name, syscall.S_IFBLK|0600, int(mkdev(uint32(major), uint32(minor))))
}

func mkdev(major, minor uint32) uint64 {
	return (uint64(major&0xfff) << 8) | uint64(minor&0xff) | (uint64(minor&^0xff) << 12) | (uint64(major&^0xfff) << 32)
}

func stayAlive() {
	for {
		time.Sleep(time.Hour)
	}
}
`
