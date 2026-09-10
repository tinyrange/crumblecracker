package desktopapp

import (
	"path/filepath"
	"testing"
)

func TestSharedFolderOpenerPassesPathAsOneArgument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared space & 日本語")
	for _, platform := range []string{"darwin", "windows", "linux"} {
		command, err := sharedFolderCommand(path, platform)
		if err != nil {
			t.Fatal(err)
		}
		if command.Args[len(command.Args)-1] != path {
			t.Fatalf("%s opener changed path: %v", platform, command.Args)
		}
		if platform == "darwin" && len(command.Args) != 3 || platform != "darwin" && len(command.Args) != 2 {
			t.Fatalf("%s split path: %v", platform, command.Args)
		}
	}
}
