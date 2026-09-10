package desktopapp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

var openSharedFolder = revealSharedFolder

func sharedFolderCommand(path, platform string) (*exec.Cmd, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	switch platform {
	case "darwin":
		return exec.Command("open", "--", path), nil
	case "windows":
		return exec.Command("explorer.exe", path), nil
	default:
		return exec.Command("xdg-open", path), nil
	}
}

func revealSharedFolder(path string) error {
	if path == "" {
		return fmt.Errorf("no shared folder is configured")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("shared folder is not a directory: %s", path)
	}
	command, err := sharedFolderCommand(path, runtime.GOOS)
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}
