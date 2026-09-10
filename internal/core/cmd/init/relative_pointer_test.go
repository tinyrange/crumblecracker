package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelativePointerBootHandshake(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		root := t.TempDir()
		var env []string
		if enabled {
			env = []string{"CCX3_RELATIVE_POINTER=1"}
		}
		if err := installRelativePointerMode(root, env); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(filepath.Join(root, "run/ccx3-relative-pointer"))
		if enabled && err != nil || !enabled && !os.IsNotExist(err) {
			t.Fatalf("enabled=%v: %v", enabled, err)
		}
	}
}
