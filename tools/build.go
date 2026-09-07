// Build the two desktop products and their embedded Linux guest payloads.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	output := flag.String("out", "build", "output directory")
	flag.Parse()
	if err := build(*output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func build(output string) error {
	if _, err := os.Stat("internal/core/cmd/build-guestinit/main.go"); err != nil {
		return fmt.Errorf("run the build from the repository root: %w", err)
	}
	if err := run(nil, "go", "run", "./internal/core/cmd/build-guestinit"); err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0755); err != nil {
		return err
	}
	for _, app := range []struct{ name, pkg string }{{"SquadVM", "squadvm"}, {"NeurodeskAppX", "ndappx"}} {
		name := app.name
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		target := filepath.Join(output, name)
		args := []string{"build", "-trimpath", "-o", target}
		if runtime.GOOS == "windows" {
			args = append(args, "-ldflags=-H=windowsgui")
		}
		args = append(args, "./cmd/"+app.pkg)
		if err := run([]string{"CGO_ENABLED=0"}, "go", args...); err != nil {
			return err
		}
		if runtime.GOOS == "darwin" {
			if err := run(nil, "codesign", "--force", "--sign", "-", "--entitlements", "tools/entitlements.xml", target); err != nil {
				return err
			}
		}
		fmt.Println(target)
	}
	return nil
}

func run(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
