//go:build linux && amd64

package kvm

import (
	"context"
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

func TestLinuxBootsWithNVMeBlock(t *testing.T) {
	if os.Getenv("CC_TEST_LINUX_KVM_NVME") == "" {
		t.Skip("set CC_TEST_LINUX_KVM_NVME=1 to run Linux NVMe block KVM smoke test")
	}
	if hostCPUHasFlag("hypervisor") {
		t.Skip("NVMe block smoke test requires bare-metal KVM")
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
	modules, err := kernelManager.PlanModuleLoad(
		[]string{"CONFIG_BLK_DEV_NVME"},
		map[string]string{"CONFIG_BLK_DEV_NVME": "kernel/drivers/nvme/host/nvme.ko.gz"},
	)
	if err != nil {
		t.Fatalf("plan NVMe block modules: %v", err)
	}

	initBin := buildNVMeBlockInit(t)
	files := []initramfs.File{
		{Path: "/dev", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/proc", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/sys", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/modules", Mode: 0o755, Type: initramfs.TypeDirectory},
		{Path: "/dev/console", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 5, DevMinor: 1},
		{Path: "/dev/kmsg", Mode: 0o600, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 11},
		{Path: "/dev/null", Mode: 0o666, Type: initramfs.TypeCharDevice, DevMajor: 1, DevMinor: 3},
		{Path: "/init", Mode: 0o755, Data: initBin, Type: initramfs.TypeRegular},
	}
	var loadOrder strings.Builder
	for _, mod := range modules {
		files = append(files, initramfs.File{
			Path: "/modules/" + mod.Name + ".ko",
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
	initrd, err := initramfs.Build(files)
	if err != nil {
		t.Fatalf("build initramfs: %v", err)
	}

	disk := newLinuxNVMeTestDisk(16 * 1024 * 1024)
	ctrl := nvme.NewController(disk)
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bootCancel()
	success := "NVME_BLOCK_OK\n"
	serial, err := BootInitramfsToMarkerWithNVMeBlock(bootCtx, kernel, initrd, 512, true, "NVME_BLOCK_OK", ctrl)
	if err != nil {
		t.Fatalf("boot Linux with NVMe block: %v\nserial:\n%s", err, serial)
	}
	if !disk.hasAt(1024, success) {
		t.Fatalf("disk missing success marker; serial:\n%s", serial)
	}
	if got := disk.stringAt(512, len("cc-nvme-block\n")); got != "cc-nvme-block\n" {
		t.Fatalf("disk write = %q", got)
	}
}

type linuxNVMeTestDisk struct {
	mu   sync.Mutex
	data []byte
}

func newLinuxNVMeTestDisk(size int) *linuxNVMeTestDisk {
	return &linuxNVMeTestDisk{data: make([]byte, size)}
}

func (d *linuxNVMeTestDisk) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off >= int64(len(d.data)) {
		return 0, nil
	}
	return copy(p, d.data[off:]), nil
}

func (d *linuxNVMeTestDisk) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off >= int64(len(d.data)) {
		return 0, nil
	}
	return copy(d.data[off:], p), nil
}

func (d *linuxNVMeTestDisk) Size() int64 {
	return int64(len(d.data))
}

func (d *linuxNVMeTestDisk) hasAt(off int64, value string) bool {
	return d.stringAt(off, len(value)) == value
}

func (d *linuxNVMeTestDisk) stringAt(off int64, size int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off+int64(size) > int64(len(d.data)) {
		return ""
	}
	return string(d.data[off : off+int64(size)])
}

func buildNVMeBlockInit(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "init.c")
	if err := os.WriteFile(src, []byte(nvmeBlockInitSource), 0o644); err != nil {
		t.Fatalf("write init source: %v", err)
	}
	out := filepath.Join(dir, "init")
	cmd := exec.Command("musl-gcc", "-static", "-Os", "-Wall", "-Wextra", "-o", out, src)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build init: %v\n%s", err, data)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	return data
}

const nvmeBlockInitSource = `#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/sysmacros.h>
#include <unistd.h>

#ifndef BLKFLSBUF
#define BLKFLSBUF 0x1261
#endif

static void stay_alive(void) {
	for (;;) {
		pause();
	}
}

static void logmsg(const char *fmt, ...) {
	char buf[512];
	va_list ap;
	va_start(ap, fmt);
	int n = vsnprintf(buf, sizeof(buf) - 2, fmt, ap);
	va_end(ap);
	if (n < 0) {
		return;
	}
	if (n > (int)sizeof(buf) - 2) {
		n = (int)sizeof(buf) - 2;
	}
	buf[n++] = '\n';
	write(1, buf, (size_t)n);
	int fd = open("/dev/kmsg", O_WRONLY | O_NONBLOCK);
	if (fd >= 0) {
		write(fd, buf, (size_t)n);
		close(fd);
	}
}

static int read_file(const char *path, void **out, size_t *out_len) {
	int fd = open(path, O_RDONLY);
	if (fd < 0) {
		return -1;
	}
	struct stat st;
	if (fstat(fd, &st) != 0 || st.st_size <= 0) {
		close(fd);
		return -1;
	}
	void *buf = malloc((size_t)st.st_size);
	if (buf == NULL) {
		close(fd);
		return -1;
	}
	size_t done = 0;
	while (done < (size_t)st.st_size) {
		ssize_t n = read(fd, (char *)buf + done, (size_t)st.st_size - done);
		if (n <= 0) {
			free(buf);
			close(fd);
			return -1;
		}
		done += (size_t)n;
	}
	close(fd);
	*out = buf;
	*out_len = done;
	return 0;
}

static void load_one_module(const char *name) {
	char path[256];
	snprintf(path, sizeof(path), "/modules/%s", name);
	void *data = NULL;
	size_t data_len = 0;
	if (read_file(path, &data, &data_len) != 0) {
		logmsg("read module %s failed: errno=%d", path, errno);
		return;
	}
	long rc = syscall(SYS_init_module, data, data_len, "");
	if (rc != 0 && errno != EEXIST) {
		logmsg("load module %s failed: errno=%d", path, errno);
	}
	free(data);
}

static void load_modules(void) {
	void *data = NULL;
	size_t data_len = 0;
	if (read_file("/modules/load-order", &data, &data_len) != 0) {
		logmsg("read module load-order failed: errno=%d", errno);
		return;
	}
	char *text = data;
	size_t start = 0;
	for (size_t i = 0; i <= data_len; i++) {
		if (i != data_len && text[i] != '\n') {
			continue;
		}
		text[i] = 0;
		if (i > start) {
			load_one_module(text + start);
		}
		start = i + 1;
	}
	free(data);
}

static int wait_block(char *name, size_t name_len) {
	for (int i = 0; i < 200; i++) {
		DIR *dir = opendir("/sys/block");
		if (dir != NULL) {
			struct dirent *entry;
			while ((entry = readdir(dir)) != NULL) {
				if (strncmp(entry->d_name, "nvme", 4) == 0) {
					snprintf(name, name_len, "%s", entry->d_name);
					closedir(dir);
					return 0;
				}
			}
			closedir(dir);
		}
		usleep(25000);
	}
	return -1;
}

static int make_node(const char *name) {
	char path[256];
	snprintf(path, sizeof(path), "/sys/block/%s/dev", name);
	int fd = open(path, O_RDONLY);
	if (fd < 0) {
		return -1;
	}
	char buf[64];
	ssize_t n = read(fd, buf, sizeof(buf) - 1);
	close(fd);
	if (n <= 0) {
		return -1;
	}
	buf[n] = 0;
	unsigned major = 0;
	unsigned minor = 0;
	if (sscanf(buf, "%u:%u", &major, &minor) != 2) {
		return -1;
	}
	snprintf(path, sizeof(path), "/dev/%s", name);
	unlink(path);
	return mknod(path, S_IFBLK | 0600, makedev(major, minor));
}

int main(void) {
	logmsg("init starting");
	mount("proc", "/proc", "proc", 0, "");
	mount("sysfs", "/sys", "sysfs", 0, "");
	load_modules();

	char dev[32];
	if (wait_block(dev, sizeof(dev)) != 0) {
		logmsg("NVMe block device not found");
		stay_alive();
	}
	logmsg("found %s", dev);
	if (make_node(dev) != 0) {
		logmsg("mknod %s failed: errno=%d", dev, errno);
		stay_alive();
	}

	char path[64];
	snprintf(path, sizeof(path), "/dev/%s", dev);
	int fd = open(path, O_RDWR | O_SYNC);
	if (fd < 0) {
		logmsg("open %s failed: errno=%d", path, errno);
		stay_alive();
	}
	const char payload[] = "cc-nvme-block\n";
	if (pwrite(fd, payload, sizeof(payload) - 1, 512) != (ssize_t)(sizeof(payload) - 1)) {
		logmsg("write block failed: errno=%d", errno);
		stay_alive();
	}
	fsync(fd);
	ioctl(fd, BLKFLSBUF);
	char buf[sizeof(payload) - 1];
	memset(buf, 0, sizeof(buf));
	if (pread(fd, buf, sizeof(buf), 512) != (ssize_t)sizeof(buf)) {
		logmsg("read block failed: errno=%d", errno);
		stay_alive();
	}
	if (memcmp(buf, payload, sizeof(buf)) != 0) {
		logmsg("readback mismatch");
		stay_alive();
	}
	const char success[] = "NVME_BLOCK_OK\n";
	if (pwrite(fd, success, sizeof(success) - 1, 1024) != (ssize_t)(sizeof(success) - 1)) {
		logmsg("write success marker failed: errno=%d", errno);
		stay_alive();
	}
	fsync(fd);
	logmsg("NVME_BLOCK_OK");
	stay_alive();
}
`
