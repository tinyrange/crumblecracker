package vm

import (
	"path/filepath"
	"strings"
)

func sameRuntimeImage(target, running string) bool {
	target = strings.TrimSpace(target)
	return target == "" || target == strings.TrimSpace(running)
}

func rootDirWithinMount(mountPath, rootDir string) string {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" || rootDir == "/" {
		return mountPath
	}
	return filepath.Join(mountPath, strings.TrimPrefix(rootDir, "/"))
}
