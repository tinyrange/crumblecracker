package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func headlessGuest(env []string) bool {
	for _, entry := range env {
		if entry == "CCX3_HEADLESS=1" {
			return true
		}
	}
	return false
}

// Runtime-only units defer the image's graphical default target. /run is a
// transient guest filesystem, so the image and persistent home stay untouched.
func installHeadlessSystemd(root string, env []string) error {
	write := func(name, body string) error {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0600)
	}
	if err := write("run/systemd/system/ccx3-headless.target", `[Unit]
Description=CrumbleCracker headless guest
Requires=basic.target ccx3-stage2.service
After=basic.target ccx3-stage2.service
AllowIsolate=yes
`); err != nil {
		return err
	}
	var assignments []string
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "CCX3_HEADLESS" {
			continue
		}
		if strings.ContainsRune(entry, 0) {
			return fmt.Errorf("invalid guest environment")
		}
		// systemd unit values use C escapes and percent specifiers, not a shell.
		assignments = append(assignments, strconv.Quote(strings.ReplaceAll(entry, "%", "%%")))
	}
	environment := "Environment=" + strings.Join(assignments, " ") + "\n"
	for _, service := range []string{"neurodesktop-glass.service", "neurodesktop-jupyter.service", "neurodesktop-virgl.service"} {
		if err := write("run/systemd/system/"+service+".d/90-headless-environment.conf", "[Unit]\nConflicts=shutdown.target\nBefore=shutdown.target\n[Service]\n"+environment); err != nil {
			return err
		}
	}
	return nil
}
