//go:build darwin && arm64

package hvf

import (
	"path/filepath"
	"strings"
)

func ImageMountPath(image string) string {
	replacer := strings.NewReplacer("/", "_", ":", "_", "@", "_", " ", "_")
	return filepath.Join("/.ccx3", "images", replacer.Replace(image))
}
