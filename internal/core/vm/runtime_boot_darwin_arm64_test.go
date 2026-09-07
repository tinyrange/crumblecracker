//go:build darwin && arm64

package vm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const runtimeBootCodesignedEnv = "CCX3_VM_TEST_CODESIGNED"

func TestMain(m *testing.M) {
	if os.Getenv(runtimeBootCodesignedEnv) == "1" {
		os.Exit(m.Run())
	}
	code, err := runCodesignedRuntimeTestBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runCodesignedRuntimeTestBinary() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("locate test binary: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "cc-runtime-vm-test-*")
	if err != nil {
		return 1, fmt.Errorf("create codesigned test temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	signedExe := filepath.Join(tmpDir, filepath.Base(exe))
	if err := copyRuntimeTestFile(exe, signedExe); err != nil {
		return 1, fmt.Errorf("copy test binary for codesigning: %w", err)
	}
	if err := os.Chmod(signedExe, 0o755); err != nil {
		return 1, fmt.Errorf("chmod copied test binary: %w", err)
	}

	entitlements := filepath.Join(runtimeBootRepoRoot(), "tools", "entitlements.xml")
	cmd := exec.Command("codesign", "-f", "-s", "-", "--entitlements", entitlements, signedExe)
	if output, err := cmd.CombinedOutput(); err != nil {
		return 1, fmt.Errorf("codesign VM test binary: %w\n%s", err, output)
	}

	child := exec.Command(signedExe, os.Args[1:]...)
	child.Env = append(os.Environ(), runtimeBootCodesignedEnv+"=1")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Stdin = os.Stdin
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("run codesigned VM test binary: %w", err)
	}
	return 0, nil
}

func copyRuntimeTestFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
