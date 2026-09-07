package guest

import "testing"

func TestProfileMatch(t *testing.T) {
	profile := Profile{
		Canonical: "@openbsd",
		Aliases:   []string{"openbsd", "OpenBSD:7.9"},
	}
	tests := []struct {
		image string
		want  bool
	}{
		{"@openbsd", true},
		{" @OPENBSD ", true},
		{"openbsd", true},
		{"openbsd:7.9", true},
		{"freebsd", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := profile.Match(tc.image); got != tc.want {
			t.Fatalf("Profile.Match(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}
}

func TestLinuxProfileCapabilities(t *testing.T) {
	caps := LinuxProfile.Caps
	if LinuxProfile.Name != "Linux" {
		t.Fatalf("Linux profile name = %q", LinuxProfile.Name)
	}
	if !caps.PersistentExec || !caps.StreamingExec || !caps.TTY || !caps.ResizeTTY || !caps.Signals {
		t.Fatalf("Linux profile missing managed exec capability: %+v", caps)
	}
	if !caps.CopyIn || !caps.CopyOut || !caps.ArchiveExtract || !caps.DynamicShares || !caps.PortForward || !caps.AlternateImageExec || !caps.RootSnapshot || !caps.ImageSnapshot {
		t.Fatalf("Linux profile missing integration capability: %+v", caps)
	}
}
