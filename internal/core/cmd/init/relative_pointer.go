package main

import (
	"os"
	"path/filepath"
	"slices"
)

// Images may opt into a single relative pointer at desktop startup. Keep the
// handshake in /run so it cannot persist across boots with a different frontend.
func installRelativePointerMode(root string, env []string) error {
	if !slices.Contains(env, "CCX3_RELATIVE_POINTER=1") {
		return nil
	}
	path := filepath.Join(root, "run/ccx3-relative-pointer")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("1\n"), 0644)
}
