package vmruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type SnapshotMemoryPathError struct {
	Path   string
	Reason string
}

func (e *SnapshotMemoryPathError) Error() string {
	return fmt.Sprintf("unsafe snapshot memory path %q: %s", e.Path, e.Reason)
}

func ResolveSnapshotMemoryPath(manifestPath, memoryFile string) (string, error) {
	if memoryFile == "" {
		return "", &SnapshotMemoryPathError{Path: memoryFile, Reason: "path is empty"}
	}
	if filepath.IsAbs(memoryFile) || filepath.VolumeName(memoryFile) != "" {
		return "", &SnapshotMemoryPathError{Path: memoryFile, Reason: "path is absolute"}
	}
	clean := filepath.Clean(memoryFile)
	if clean != memoryFile || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &SnapshotMemoryPathError{Path: memoryFile, Reason: "path is not a clean relative path"}
	}
	baseDir := filepath.Dir(manifestPath)
	resolvedBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve snapshot directory: %w", err)
	}
	candidate := filepath.Join(baseDir, clean)
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve snapshot memory path: %w", err)
	}
	rel, err := filepath.Rel(resolvedBase, resolvedCandidate)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &SnapshotMemoryPathError{Path: memoryFile, Reason: "resolved path escapes snapshot directory"}
	}
	info, err := os.Stat(resolvedCandidate)
	if err != nil {
		return "", fmt.Errorf("stat snapshot memory path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", &SnapshotMemoryPathError{Path: memoryFile, Reason: "path is not a regular file"}
	}
	return resolvedCandidate, nil
}
