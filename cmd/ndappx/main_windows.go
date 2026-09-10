//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
)

const attachParentProcess = ^uintptr(0)

var (
	kernel32DLL      = syscall.NewLazyDLL("kernel32.dll")
	attachConsoleAPI = kernel32DLL.NewProc("AttachConsole")
)

func platformArguments(args []string) []string {
	// Preserve Electron's inherited pipes in headless mode; attaching the
	// parent's console replaces stdout and breaks the readiness handshake.
	for _, arg := range args {
		if strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-") == "headless" {
			return args
		}
	}
	attachParentConsole()
	if len(args) != 0 {
		return args
	}
	return []string{defaultNeurodesktopImage}
}

func platformDefaultCPUs() int {
	return 4
}

func platformDefaultEphemeralHome() bool {
	return true
}

func attachParentConsole() bool {
	attached, _, _ := attachConsoleAPI.Call(attachParentProcess)
	if attached == 0 {
		return false
	}
	if stdin, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = stdin
	}
	if stdout, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = stdout
	}
	if stderr, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stderr = stderr
	}
	return true
}
