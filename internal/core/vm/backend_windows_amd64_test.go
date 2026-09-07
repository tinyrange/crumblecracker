//go:build windows && amd64

package vm

import "testing"

func TestWindowsGuestPreservesRequestedInitSystem(t *testing.T) {
	cfg := windowsGuestInitConfig(nil, true, " systemd ")
	if cfg.InitSystem != "systemd" {
		t.Fatalf("init system = %q, want systemd", cfg.InitSystem)
	}
	if cfg.DisableCgroupMount {
		t.Fatal("cgroup filesystem disabled for a managed Windows guest")
	}
}
