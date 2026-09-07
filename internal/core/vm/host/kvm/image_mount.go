//go:build linux

package kvm

import (
	"path/filepath"
	"strings"
)

func ImageMountPath(image string) string {
	replacer := strings.NewReplacer("/", "_", ":", "_", "@", "_", " ", "_")
	return filepath.Join("/.ccx3", "images", replacer.Replace(image))
}
