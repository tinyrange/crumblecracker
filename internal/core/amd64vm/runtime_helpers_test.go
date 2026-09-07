package amd64vm

import "testing"

func TestBuildShareMountUsesCanonicalGuestPath(t *testing.T) {
	mount, err := BuildShareMount(0, DirectoryShare{Source: t.TempDir(), Mount: " /data/../data/ "})
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := mount.Backend.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	if mount.GuestPath != "/data" {
		t.Fatalf("guest path = %q, want /data", mount.GuestPath)
	}
	if _, err := BuildShareMount(1, DirectoryShare{Source: t.TempDir(), Mount: "/"}); err == nil {
		t.Fatal("root share unexpectedly accepted")
	}
}
