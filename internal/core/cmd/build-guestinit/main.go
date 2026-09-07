package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	arch := flag.String("arch", runtime.GOARCH, "target architecture for BSD guest init payloads")
	flag.Parse()
	if *arch != "amd64" && *arch != "arm64" {
		fmt.Fprintf(os.Stderr, "build-guestinit: unsupported architecture %q\n", *arch)
		os.Exit(2)
	}

	builds := []struct {
		goos, goarch, pkg, output string
	}{
		{"linux", "arm64", "./internal/core/cmd/init", "internal/core/guestinit/guest-init-linux-arm64"},
		{"linux", "amd64", "./internal/core/cmd/init", "internal/core/guestinit/guest-init-linux-amd64"},
	}
	for _, build := range builds {
		if err := buildPayload(build.goos, build.goarch, build.pkg, build.output); err != nil {
			fmt.Fprintln(os.Stderr, "build-guestinit:", err)
			os.Exit(1)
		}
	}
}

func buildPayload(goos, goarch, pkg, output string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	tmp := output + ".tmp"
	_ = os.Remove(tmp)
	cmd := exec.Command("go", "build", "-trimpath", "-o", tmp, pkg)
	cmd.Env = append(withoutGoTarget(os.Environ()), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("build %s/%s %s: %w", goos, goarch, pkg, err)
	}
	if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, output); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func withoutGoTarget(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if key == "GOOS" || key == "GOARCH" || key == "CGO_ENABLED" {
			continue
		}
		out = append(out, item)
	}
	return out
}
