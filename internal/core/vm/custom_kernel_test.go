package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCustomKernelProviderReadsFileKernel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmlinuz")
	if err := os.WriteFile(path, []byte("kernel-bytes"), 0o600); err != nil {
		t.Fatalf("write kernel: %v", err)
	}

	provider := customKernelProvider{path: path}
	got, err := provider.ReadKernel()
	if err != nil {
		t.Fatalf("read custom kernel: %v", err)
	}
	if string(got) != "kernel-bytes" {
		t.Fatalf("kernel bytes = %q, want fixture bytes", got)
	}
	modules, err := provider.PlanModuleLoad([]string{"CONFIG_VIRTIO_FS"}, nil)
	if err != nil {
		t.Fatalf("plan modules for custom kernel: %v", err)
	}
	if len(modules) != 0 {
		t.Fatalf("modules = %d, want none for custom kernel", len(modules))
	}
}

func TestCustomKernelPathRequiresFilePrefix(t *testing.T) {
	if _, ok := customKernelPath("ubuntu"); ok {
		t.Fatalf("ubuntu detected as custom kernel")
	}
	path, ok := customKernelPath("file:/tmp/vmlinuz")
	if !ok || path != "/tmp/vmlinuz" {
		t.Fatalf("customKernelPath = %q, %v; want /tmp/vmlinuz, true", path, ok)
	}
}
