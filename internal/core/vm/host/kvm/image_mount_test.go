//go:build linux

package kvm

import "testing"

func TestImageMountPathSanitizesImageName(t *testing.T) {
	got := ImageMountPath("@repo/name:tag with space")
	want := "/.ccx3/images/_repo_name_tag_with_space"
	if got != want {
		t.Fatalf("mount path = %q, want %q", got, want)
	}
}
