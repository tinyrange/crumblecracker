package vm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestRuntimeSequentialMultiOSSoak(t *testing.T) {
	if os.Getenv("CC_TEST_VM_SOAK") == "" {
		t.Skip("set CC_TEST_VM_SOAK=1 to run the sequential multi-OS VM soak")
	}
	if testing.Short() {
		t.Skip("sequential multi-OS VM soak is not a short test")
	}
	if err := Supports(); err != nil {
		t.Skipf("VM runtime unsupported on this host: %v", err)
	}

	boots := soakPositiveInt(t, "CC_TEST_VM_SOAK_BOOTS", 100)
	commands := soakPositiveInt(t, "CC_TEST_VM_SOAK_COMMANDS", 20)
	selectedOS := soakSelectedOS()
	linux := newRuntimeBootEnv(t)
	bsd := NewRuntimeBackend(nil, nil, runtimeBootCacheRoot(t)+"/guestinit")

	cases := []struct {
		name     string
		image    string
		guestOS  string
		memoryMB uint64
		backend  Backend
		network  *client.NetworkConfig
	}{
		{name: "linux", image: linux.imageName, guestOS: "Linux", memoryMB: linux.memoryMB, backend: linux.backend, network: &client.NetworkConfig{Enabled: false}},
		{name: "netbsd", image: "@netbsd", guestOS: "NetBSD", memoryMB: 1024, backend: bsd},
		{name: "freebsd", image: "@freebsd", guestOS: "FreeBSD", memoryMB: 1024, backend: bsd},
		{name: "openbsd", image: "@openbsd", guestOS: "OpenBSD", memoryMB: 768, backend: bsd},
	}

	for _, tc := range cases {
		if len(selectedOS) != 0 && !selectedOS[tc.name] {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManagerWithBackend(tc.backend)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = manager.ShutdownAll(ctx)
			})

			startGoroutines := runtime.NumGoroutine()
			started := time.Now()
			for boot := 0; boot < boots; boot++ {
				runSequentialSoakBoot(t, manager, tc.name, tc.image, tc.guestOS, tc.memoryMB, tc.network, boot, commands)
				if (boot+1)%10 == 0 || boot+1 == boots {
					runtime.GC()
					var mem runtime.MemStats
					runtime.ReadMemStats(&mem)
					t.Logf("progress boots=%d/%d commands=%d elapsed=%s goroutines=%d heap=%dMiB open_fds=%d",
						boot+1, boots, soakCommandCount(boot+1, commands), time.Since(started).Round(time.Millisecond),
						runtime.NumGoroutine(), mem.HeapAlloc>>20, soakOpenFDs())
				}
			}
			t.Logf("completed boots=%d commands=%d elapsed=%s goroutine_delta=%d",
				boots, soakCommandCount(boots, commands), time.Since(started).Round(time.Millisecond), runtime.NumGoroutine()-startGoroutines)
		})
	}
}

func soakCommandCount(boots, commandsPerBoot int) int {
	return boots*(commandsPerBoot+3) + 2*((boots+9)/10)
}

func runSequentialSoakBoot(t *testing.T, manager *Manager, osName, image, guestOS string, memoryMB uint64, network *client.NetworkConfig, boot, commands int) {
	t.Helper()
	const id = "sequential-soak"
	marker := fmt.Sprintf("%s-%06d", osName, boot)
	started := false
	defer func() {
		if !started {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := manager.ShutdownInstance(ctx, id); err != nil {
			t.Errorf("%s boot %d cleanup shutdown: %v", osName, boot, err)
		}
	}()

	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBoot()
	bootEvents := 0
	state, err := manager.StartStream(bootCtx, client.CreateInstanceRequest{
		ID:       id,
		Image:    image,
		MemoryMB: memoryMB,
		CPUs:     1,
		Network:  network,
	}, func(client.BootEvent) error {
		bootEvents++
		return nil
	})
	if err != nil {
		t.Fatalf("%s boot %d start: %v", osName, boot, err)
	}
	started = true
	if state.Status != "running" || manager.StatusOf(id).Status != "running" {
		t.Fatalf("%s boot %d state after start = %+v / %+v", osName, boot, state, manager.StatusOf(id))
	}
	if bootEvents == 0 {
		t.Fatalf("%s boot %d emitted no boot events", osName, boot)
	}

	for command := 0; command < commands; command++ {
		commandMarker := fmt.Sprintf("%s-c%03d", marker, command)
		script := `set -eu
marker=$1
want_os=$2
mode=$3
test "$(uname -s)" = "$want_os"
case "$mode" in
  0)
    path="/tmp/cc-soak-$marker"
    printf '%s' "$marker" >"$path"
    test "$(cat "$path")" = "$marker"
    rm -f "$path"
    ;;
  1)
    test "$PWD" = /tmp
    test "$CC_SOAK_MARKER" = "$marker"
    ;;
  2)
    value=0
    while test "$value" -lt 32; do value=$((value + 1)); done
    test "$value" -eq 32
    ;;
  3)
    (printf '%s' "$marker") | while IFS= read -r value; do test "$value" = "$marker"; done
    ;;
esac
printf '%s\n' "$marker"`
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := manager.RunIn(ctx, id, client.RunRequest{
			Command: []string{"sh", "-c", script, "soak", commandMarker, guestOS, strconv.Itoa(command % 4)},
			Env:     []string{"CC_SOAK_MARKER=" + commandMarker},
			WorkDir: "/tmp",
		})
		cancel()
		if err != nil {
			t.Fatalf("%s boot %d command %d: %v", osName, boot, command, err)
		}
		if resp.ExitCode != 0 || strings.TrimSpace(resp.Output) != commandMarker {
			t.Fatalf("%s boot %d command %d response = code %d output %q", osName, boot, command, resp.ExitCode, resp.Output)
		}
	}

	runSoakStreamedInput(t, manager, id, osName, boot, marker)
	runSoakControlFD(t, manager, id, osName, boot, marker)
	if boot%10 == 0 {
		runSoakSignal(t, manager, id, osName, boot, marker)
		runSoakTTYResize(t, manager, id, osName, boot, marker)
	}

	execCtx, cancelExec := context.WithTimeout(context.Background(), 15*time.Second)
	failed, err := manager.RunIn(execCtx, id, client.RunRequest{
		Command: []string{"sh", "-c", "printf '%s\\n' \"$1\"; exit 37", "soak", marker + "-exit"},
	})
	cancelExec()
	if err != nil {
		t.Fatalf("%s boot %d non-zero command: %v", osName, boot, err)
	}
	if failed.ExitCode != 37 || strings.TrimSpace(failed.Output) != marker+"-exit" {
		t.Fatalf("%s boot %d non-zero response = code %d output %q", osName, boot, failed.ExitCode, failed.Output)
	}

	apiCtx, cancelAPI := context.WithTimeout(context.Background(), 15*time.Second)
	if err := manager.FlushInstance(apiCtx, id); err != nil {
		cancelAPI()
		t.Fatalf("%s boot %d flush: %v", osName, boot, err)
	}
	if _, err := manager.ConsoleHistory(apiCtx, id); err != nil {
		cancelAPI()
		t.Fatalf("%s boot %d console history: %v", osName, boot, err)
	}
	cancelAPI()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	err = manager.ShutdownInstance(shutdownCtx, id)
	cancelShutdown()
	if err != nil {
		t.Fatalf("%s boot %d shutdown: %v", osName, boot, err)
	}
	started = false
	if state := manager.StatusOf(id); state.Status != "stopped" || state.ExitReason != "clean shutdown" {
		t.Fatalf("%s boot %d state after shutdown = %+v", osName, boot, state)
	}
}

func runSoakStreamedInput(t *testing.T, manager *Manager, id, osName string, boot int, marker string) {
	t.Helper()
	inputs := make(chan client.ExecInput, 3)
	inputs <- client.ExecInput{Kind: "stdin", Data: []byte("first-")}
	inputs <- client.ExecInput{Kind: "stdin", Data: []byte(marker + "\nsecond-" + marker + "\n")}
	close(inputs)
	var output bytes.Buffer
	var exit *int
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := manager.StreamIn(ctx, id, client.ExecRequest{
		Command: []string{"sh", "-c", "while IFS= read -r line; do printf '%s\\n' \"$line\"; done"},
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "stderr", "output":
			writeExecEventOutput(&output, event)
		case "exit":
			code := event.ExitCode
			exit = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s boot %d streamed input: %v", osName, boot, err)
	}
	want := "first-" + marker + "\nsecond-" + marker
	if exit == nil || *exit != 0 || strings.TrimSpace(output.String()) != want {
		t.Fatalf("%s boot %d streamed input exit=%v output=%q, want %q", osName, boot, exit, output.String(), want)
	}
}

func runSoakControlFD(t *testing.T, manager *Manager, id, osName string, boot int, marker string) {
	t.Helper()
	var control bytes.Buffer
	var exit *int
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := manager.StreamIn(ctx, id, client.ExecRequest{
		Command:   []string{"sh", "-c", "printf '%s\\n' \"$1\" >&3", "soak", marker + "-control"},
		ControlFD: true,
	}, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "control":
			control.WriteString(event.Output)
		case "exit":
			code := event.ExitCode
			exit = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s boot %d control fd: %v", osName, boot, err)
	}
	if exit == nil || *exit != 0 || strings.TrimSpace(control.String()) != marker+"-control" {
		t.Fatalf("%s boot %d control fd exit=%v output=%q", osName, boot, exit, control.String())
	}
}

func runSoakSignal(t *testing.T, manager *Manager, id, osName string, boot int, marker string) {
	t.Helper()
	inputs := make(chan client.ExecInput, 1)
	var control bytes.Buffer
	var exit *int
	signaled := false
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := manager.StreamIn(ctx, id, client.ExecRequest{
		Command:   []string{"sh", "-c", "trap 'printf \\\"%s\\\\n\\\" \\\"$1-term\\\" >&3; exit 7' TERM; printf '%s\\n' \"$1-ready\" >&3; while :; do sleep 1; done", "soak", marker},
		ControlFD: true,
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "control":
			control.WriteString(event.Output)
			if !signaled && strings.Contains(control.String(), marker+"-ready") {
				signaled = true
				inputs <- client.ExecInput{Kind: "signal", Signal: "TERM"}
				close(inputs)
			}
		case "exit":
			code := event.ExitCode
			exit = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s boot %d signal: %v", osName, boot, err)
	}
	if !signaled || exit == nil || *exit != 7 || !strings.Contains(control.String(), marker+"-term") {
		t.Fatalf("%s boot %d signal state signaled=%t exit=%v control=%q", osName, boot, signaled, exit, control.String())
	}
}

func runSoakTTYResize(t *testing.T, manager *Manager, id, osName string, boot int, marker string) {
	t.Helper()
	inputs := make(chan client.ExecInput, 2)
	var control bytes.Buffer
	var output bytes.Buffer
	var exit *int
	sent := false
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := manager.StreamIn(ctx, id, client.ExecRequest{
		Command:   []string{"sh", "-c", "printf '%s\\n' \"$1-ready\" >&3; IFS= read -r command; eval \"$command\"", "soak", marker},
		TTY:       true,
		ControlFD: true,
		Cols:      80,
		Rows:      24,
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "stderr", "output":
			writeExecEventOutput(&output, event)
		case "control":
			control.WriteString(event.Output)
			if !sent && strings.Contains(control.String(), marker+"-ready") {
				sent = true
				inputs <- client.ExecInput{Kind: "resize", Cols: 91, Rows: 37}
				inputs <- client.ExecInput{Kind: "stdin", Data: []byte("stty size >&3\n")}
				close(inputs)
			}
		case "exit":
			code := event.ExitCode
			exit = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s boot %d TTY resize: %v", osName, boot, err)
	}
	if !sent || exit == nil || *exit != 0 || !strings.Contains(control.String(), "37 91") {
		exitCode := -1
		if exit != nil {
			exitCode = *exit
		}
		t.Fatalf("%s boot %d TTY resize state sent=%t exit=%d control=%q output=%q", osName, boot, sent, exitCode, control.String(), output.String())
	}
}

func soakPositiveInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return value
}

func soakSelectedOS() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("CC_TEST_VM_SOAK_OS"))
	if raw == "" {
		return nil
	}
	selected := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(strings.ToLower(name)); name != "" {
			selected[name] = true
		}
	}
	return selected
}

func soakOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}
