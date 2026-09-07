//go:build !windows

package capturerelay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRetirementReleasesSpoolAndContinuesDrainingWriter(t *testing.T) {
	dir := t.TempDir()
	controlPath := filepath.Join(dir, "relay-control")
	shellPath := filepath.Join(dir, "shell-control")
	if err := syscall.Mkfifo(shellPath, 0o600); err != nil {
		t.Fatal(err)
	}
	shell, err := os.OpenFile(shellPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()

	relayDone := make(chan error, 1)
	go func() { relayDone <- Run([]string{controlPath, shellPath, "65536"}) }()
	waitForFile(t, controlPath+".ready", true)
	control, err := os.OpenFile(controlPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	output := filepath.Join(dir, "output")
	paths := []string{output, output + ".closed", output + ".overflow", output + ".retired", output + ".registered", output + ".finished"}
	for _, path := range paths {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fifo := output + ".fifo"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	account := "split-output"
	fmt.Fprintf(control, "register\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", account, output, fifo, output+".closed", output+".overflow", output+".retired", output+".registered", output+".finished")
	waitForFile(t, output+".registered", true)

	writerReady := make(chan struct{})
	continueWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			writerDone <- err
			return
		}
		if _, err := writer.Write([]byte(strings.Repeat("a", 50000))); err != nil {
			writerDone <- err
			return
		}
		if _, err := fmt.Fprintf(writer, "%s%s%s", captureFinishPrefix, account, captureFinishSuffix); err != nil {
			writerDone <- err
			return
		}
		close(writerReady)
		<-continueWriter
		_, err = writer.Write([]byte(strings.Repeat("b", 50000)))
		writerDone <- errors.Join(err, writer.Close())
	}()
	<-writerReady
	waitForFile(t, output+".finished", true)
	fmt.Fprintf(control, "retire\t%s\n", account)
	waitForFile(t, output+".retired", true)

	before, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != 50000 {
		t.Fatalf("finish acknowledgement published with %d of 50000 foreground bytes stored", before.Size())
	}
	close(continueWriter)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	accounting := make(chan string, 1)
	go func() {
		buf := make([]byte, 128)
		n, err := shell.Read(buf)
		if err != nil {
			accounting <- "read error: " + err.Error()
			return
		}
		accounting <- string(buf[:n])
	}()
	var got string
	select {
	case got = <-accounting:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for capture accounting")
	}
	if want := "\x1dvmsh-capture:split-output:100000\x1f\n"; got != want {
		t.Fatalf("accounting = %q, want %q", got, want)
	}
	after, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || after.Size() > 50000 {
		t.Fatalf("retired spool grew from %d to %d", before.Size(), after.Size())
	}
	if data, err := os.ReadFile(output + ".closed"); err != nil || len(data) != 0 {
		t.Fatalf("retired capture recreated completion marker: %q, %v", data, err)
	}
	fmt.Fprintln(control, "stop")
	if err := <-relayDone; err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationAcknowledgementFailureRelinquishesCapture(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := output + ".fifo"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	registered := filepath.Join(dir, "registered-directory")
	if err := os.Mkdir(registered, 0o700); err != nil {
		t.Fatal(err)
	}
	relay := newRelay(filepath.Join(dir, "control"), filepath.Join(dir, "shell"), 65536)
	err := relay.register([]string{
		"account", output, fifo, output + ".closed", output + ".overflow",
		output + ".retired", registered, output + ".finished",
	})
	if err == nil {
		t.Fatal("capture registration succeeded without a deliverable acknowledgement")
	}
	if len(relay.captures) != 0 {
		t.Fatal("failed capture registration remained owned by the relay")
	}
	writer, openErr := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if openErr == nil {
		_ = writer.Close()
		t.Fatal("failed capture registration left its FIFO reader open")
	}
}

func TestSpoolFailureSwitchesToDrainAndDiscard(t *testing.T) {
	spool, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("host does not provide /dev/full: %v", err)
	}
	overflow := filepath.Join(t.TempDir(), "overflow")
	if err := os.WriteFile(overflow, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &capture{owner: &relay{maxStored: 65536}, account: "full", spool: spool, overflow: overflow}
	capture.consumeData([]byte("first"))
	if capture.spool != nil {
		t.Fatal("failed spool remained open for repeated writes")
	}
	capture.consumeData([]byte("second"))
	if capture.total != int64(len("firstsecond")) {
		t.Fatalf("discard mode stopped draining byte accounting at %d", capture.total)
	}
	if data, err := os.ReadFile(overflow); err != nil || len(data) == 0 {
		t.Fatalf("spool failure was not reported as overflow: %q, %v", data, err)
	}
}

func waitForFile(t *testing.T, path string, nonempty bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && (!nonempty || info.Size() != 0) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
