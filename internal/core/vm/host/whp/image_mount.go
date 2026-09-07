//go:build windows && amd64

package whp

import (
	"path"
	"strings"
)

func ImageMountPath(image string) string {
	replacer := strings.NewReplacer("/", "_", ":", "_", "@", "_", " ", "_")
	return path.Join("/.ccx3", "images", replacer.Replace(image))
}
