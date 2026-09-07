//go:build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/managed/guestagent"
)

func TestConfigurePackageManagersDoesNotDisableAptPipelining(t *testing.T) {
	root := t.TempDir()
	aptPath := filepath.Join(root, "usr", "bin", "apt")
	configPath := filepath.Join(root, "etc", "apt", "apt.conf.d", "99ccvm-netstack")
	if err := os.MkdirAll(filepath.Dir(aptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aptPath, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`Acquire::http::Pipeline-Depth "0";`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := configurePackageManagers(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		config[fields[0]] = strings.Trim(fields[1], `";`)
	}
	if _, ok := config["Acquire::http::Pipeline-Depth"]; ok {
		t.Fatalf("HTTP pipelining was overridden: %q", config["Acquire::http::Pipeline-Depth"])
	}
	if _, ok := config["Acquire::https::Pipeline-Depth"]; ok {
		t.Fatalf("HTTPS pipelining was overridden: %q", config["Acquire::https::Pipeline-Depth"])
	}
	if config["Acquire::Queue-Mode"] != "access" {
		t.Fatalf("queue mode = %q, want access", config["Acquire::Queue-Mode"])
	}
}

func TestConfigureDevicePermissionsMakesFuseAvailableToGuestUsers(t *testing.T) {
	devRoot := t.TempDir()
	fusePath := filepath.Join(devRoot, "fuse")
	if err := os.WriteFile(fusePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	configureDevicePermissions(devRoot)

	info, err := os.Stat(fusePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o666 {
		t.Fatalf("/dev/fuse permissions = %#o, want 0666", got)
	}
}

func TestInstallModuleSymversUsesKernelBuildPath(t *testing.T) {
	root := t.TempDir()
	release := "6.18.39-0-virt"
	want := []byte("0x12345678\tmodule_layout\tvmlinux\tEXPORT_SYMBOL\n")
	if err := installModuleSymvers(root, release, want); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "lib", "modules", release, "build", "Module.symvers")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Module.symvers = %q, want %q", got, want)
	}

	replacement := []byte("0x87654321\tmodule_layout\tvmlinux\tEXPORT_SYMBOL\n")
	if err := installModuleSymvers(root, release, replacement); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replaced Module.symvers = %q, want %q", got, replacement)
	}
}

func TestInstallModuleSymversRejectsInvalidRelease(t *testing.T) {
	root := t.TempDir()
	if err := installModuleSymvers(root, "../escape", []byte("data")); err == nil {
		t.Fatal("invalid kernel release unexpectedly accepted")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid install wrote %d root entries", len(entries))
	}
}

func TestCommandNeedsSystemdReady(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "empty", argv: nil, want: false},
		{name: "ordinary command", argv: []string{"nproc"}, want: false},
		{name: "direct systemctl", argv: []string{"systemctl", "status"}, want: true},
		{name: "path systemctl", argv: []string{"/usr/bin/systemctl", "status"}, want: true},
		{name: "journalctl", argv: []string{"journalctl", "-b"}, want: true},
		{name: "service", argv: []string{"service", "ssh", "status"}, want: true},
		{name: "service help", argv: []string{"service", "--help"}, want: false},
		{name: "shell ordinary", argv: []string{"sh", "-lc", "printf ok"}, want: false},
		{name: "shell systemctl", argv: []string{"sh", "-lc", "systemctl status"}, want: true},
		{name: "shell path systemctl", argv: []string{"bash", "-c", "/bin/systemctl status"}, want: true},
		{name: "sudo systemctl", argv: []string{"sudo", "-u", "root", "systemctl", "status"}, want: true},
		{name: "env systemctl", argv: []string{"env", "LC_ALL=C", "systemctl", "status"}, want: true},
		{name: "env ordinary", argv: []string{"env", "LC_ALL=C", "nproc"}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := commandNeedsSystemdReady(test.argv)
			if got != test.want {
				t.Fatalf("commandNeedsSystemdReady(%q) = %v, want %v", test.argv, got, test.want)
			}
		})
	}
}

func TestReapOrphanedChildrenPreservesManagedExitStatus(t *testing.T) {
	managed := exec.Command("sh", "-c", "exit 7")
	if err := startManagedExecProcess(managed, nil); err != nil {
		t.Fatal(err)
	}
	orphan := exec.Command("sh", "-c", "exit 0")
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}

	waitForZombie := func(pid int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			state, ppid, ok := procProcessState(filepath.Join("/proc", itoa(pid), "stat"))
			if ok && state == 'Z' && ppid == os.Getpid() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("process %d did not become a child zombie", pid)
	}
	waitForZombie(managed.Process.Pid)
	waitForZombie(orphan.Process.Pid)

	reapOrphanedChildren(os.Getpid())
	result := waitManagedExecProcess(managed, time.Now())
	if result.ExitErr != nil || result.ExitCode != 7 {
		t.Fatalf("managed wait = exit %d, err %v", result.ExitCode, result.ExitErr)
	}
	if _, err := orphan.Process.Wait(); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("orphan wait error = %v, want already reaped child", err)
	}
}

func TestSystemdCommandGate(t *testing.T) {
	nonSystemd := newSystemdCommandGate("")
	if wait := nonSystemd.WaitForCommand([]string{"systemctl", "status"}); wait != nil {
		t.Fatalf("non-systemd gate returned wait function")
	}

	systemd := newSystemdCommandGate("systemd")
	if wait := systemd.WaitForCommand([]string{"nproc"}); wait != nil {
		t.Fatalf("ordinary command returned wait function")
	}

	calls := 0
	systemd.wait = func(timeout time.Duration) error {
		if timeout != systemdReadyTimeout {
			t.Fatalf("timeout = %s, want %s", timeout, systemdReadyTimeout)
		}
		calls++
		return nil
	}
	wait := systemd.WaitForCommand([]string{"systemctl", "status"})
	if wait == nil {
		t.Fatalf("systemctl did not return wait function")
	}
	if err := wait(); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("cached wait: %v", err)
	}
	if calls != 1 {
		t.Fatalf("wait calls = %d, want 1", calls)
	}
}

func TestSystemdCommandGateRetriesAfterError(t *testing.T) {
	systemd := newSystemdCommandGate("systemd")
	wantErr := errors.New("not ready")
	calls := 0
	systemd.wait = func(time.Duration) error {
		calls++
		if calls == 1 {
			return wantErr
		}
		return nil
	}
	wait := systemd.WaitForCommand([]string{"journalctl", "-b"})
	if wait == nil {
		t.Fatalf("journalctl did not return wait function")
	}
	if err := wait(); !errors.Is(err, wantErr) {
		t.Fatalf("first wait error = %v, want %v", err, wantErr)
	}
	if err := wait(); err != nil {
		t.Fatalf("retry wait: %v", err)
	}
	if calls != 2 {
		t.Fatalf("wait calls = %d, want 2", calls)
	}
	if err := wait(); err != nil {
		t.Fatalf("cached retry wait: %v", err)
	}
	if calls != 2 {
		t.Fatalf("cached wait calls = %d, want 2", calls)
	}
}

func TestValidateExecRequest(t *testing.T) {
	if got := validateExecRequest(execRequest{ID: "7"}); got.Message != "exec request missing command" || got.ExitCode != 125 {
		t.Fatalf("missing command validation = %+v", got)
	}
	if got := validateExecRequest(execRequest{Command: []string{"true"}}); got.Message != "exec request missing id" || got.ExitCode != 0 {
		t.Fatalf("missing id validation = %+v", got)
	}
	if got := validateExecRequest(execRequest{ID: "7", Command: []string{"true"}}); got.Message != "" || got.ExitCode != 0 {
		t.Fatalf("valid request validation = %+v", got)
	}
}

func TestPrepareExecRequestAppliesDefaults(t *testing.T) {
	cfg := config{
		Env:     []string{"PATH=/bin", "BASE=1"},
		WorkDir: "/workspace",
		User:    "1000:1000",
	}
	req := execRequest{
		ID:        "7",
		Command:   []string{"tool"},
		RootDir:   "/rootfs",
		Stdin:     []byte("input"),
		TTY:       true,
		ControlFD: true,
		Cols:      80,
		Rows:      24,
	}
	got := prepareExecRequest(cfg, req)
	if got.ID != "7" || got.RootDir != "/rootfs" || got.WorkDir != "/workspace" || got.User != "1000:1000" {
		t.Fatalf("prepared request metadata = %+v", got)
	}
	if !reflect.DeepEqual(got.Command, []string{"tool"}) || !reflect.DeepEqual(got.Env, cfg.Env) || string(got.Stdin) != "input" {
		t.Fatalf("prepared request payload = %+v", got)
	}
	if !got.TTY || !got.ControlFD || got.Cols != 80 || got.Rows != 24 {
		t.Fatalf("prepared request tty/control = %+v", got)
	}
	got.Command[0] = "mutated"
	got.Env[0] = "MUTATED=1"
	got.Stdin[0] = 'x'
	if req.Command[0] != "tool" || cfg.Env[0] != "PATH=/bin" || string(req.Stdin) != "input" {
		t.Fatalf("prepareExecRequest did not copy slices")
	}
}

func TestManagedExecCommandDirect(t *testing.T) {
	env := []string{"PATH=/bin"}
	cmd, pivot := managedExecCommand([]string{"echo", "ok"}, env, "", "/work", nil, false)
	if pivot {
		t.Fatalf("direct command unexpectedly used pivot")
	}
	if cmd.Args[0] != "echo" {
		t.Fatalf("argv0 = %q, want echo", cmd.Args[0])
	}
	if !reflect.DeepEqual(cmd.Args, []string{"echo", "ok"}) {
		t.Fatalf("args = %#v", cmd.Args)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("dir = %q, want /work", cmd.Dir)
	}
	if !reflect.DeepEqual(cmd.Env, env) {
		t.Fatalf("env = %#v", cmd.Env)
	}
	if cmd.WaitDelay != 2*time.Second {
		t.Fatalf("wait delay = %s", cmd.WaitDelay)
	}
}

func TestManagedExecCommandUsesRequestPATH(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "installed-after-boot")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	cmd, pivot := managedExecCommand([]string{"installed-after-boot", "arg"}, []string{"PATH=" + dir}, "", "", nil, false)
	if pivot {
		t.Fatal("direct command unexpectedly used pivot")
	}
	if cmd.Path != executable {
		t.Fatalf("executable path = %q, want %q", cmd.Path, executable)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"installed-after-boot", "arg"}) {
		t.Fatalf("args = %#v", cmd.Args)
	}
}

func TestManagedExecCommandPivotsForRootDir(t *testing.T) {
	cmd, pivot := managedExecCommand([]string{"/bin/sh", "-c", "id"}, []string{"A=B"}, "/mnt/root", "/work", nil, false)
	if !pivot {
		t.Fatalf("root dir command did not use pivot")
	}
	req := parseManagedExecPivotRequest(t, cmd)
	if req.rootDir != "/mnt/root" || req.workDir != "/work" {
		t.Fatalf("pivot request root/work = %q/%q", req.rootDir, req.workDir)
	}
	if !reflect.DeepEqual(req.argv, []string{"/bin/sh", "-c", "id"}) {
		t.Fatalf("pivot argv = %#v", req.argv)
	}
}

func TestManagedExecCommandPivotsForTTYCredential(t *testing.T) {
	cred := &syscall.Credential{Uid: 1000, Gid: 1001, Groups: []uint32{10, 20}}
	cmd, pivot := managedExecCommand([]string{"whoami"}, nil, "", "/home/user", cred, true)
	if !pivot {
		t.Fatalf("tty credential command did not use pivot")
	}
	req := parseManagedExecPivotRequest(t, cmd)
	if req.rootDir != "" || req.workDir != "/home/user" {
		t.Fatalf("pivot request root/work = %q/%q", req.rootDir, req.workDir)
	}
	if req.uid != "1000" || req.gid != "1001" || req.groups != "10,20" {
		t.Fatalf("pivot credential = %q/%q/%q", req.uid, req.gid, req.groups)
	}
	if !reflect.DeepEqual(req.argv, []string{"whoami"}) {
		t.Fatalf("pivot argv = %#v", req.argv)
	}
}

func TestParseExecPivotArgs(t *testing.T) {
	args := execPivotArgs("/mnt/root", "/work", &syscall.Credential{Uid: 1000, Gid: 1001, Groups: []uint32{10, 20}}, []string{"/bin/sh", "-c", "id"})
	req, err := parseExecPivotArgs(args)
	if err != nil {
		t.Fatalf("parseExecPivotArgs: %v", err)
	}
	if req.rootDir != "/mnt/root" || req.workDir != "/work" || req.uid != "1000" || req.gid != "1001" || req.groups != "10,20" {
		t.Fatalf("request metadata = %+v", req)
	}
	if !reflect.DeepEqual(req.argv, []string{"/bin/sh", "-c", "id"}) {
		t.Fatalf("argv = %#v", req.argv)
	}

	nilCredArgs := execPivotArgs("", "", nil, []string{"true"})
	nilCredReq, err := parseExecPivotArgs(nilCredArgs)
	if err != nil {
		t.Fatalf("parse nil credential args: %v", err)
	}
	if nilCredReq.uid != "" || nilCredReq.gid != "" || nilCredReq.groups != "" {
		t.Fatalf("nil credential request = %+v", nilCredReq)
	}

	if _, err := parseExecPivotArgs([]string{"too", "short"}); err == nil {
		t.Fatalf("short args error = %v", err)
	}
	if _, err := parseExecPivotArgs([]string{"", "", "", "", "", "not-separator", "true"}); err == nil {
		t.Fatalf("separator error = %v", err)
	}
}

func TestPivotExecRootWithOps(t *testing.T) {
	state := execPivotRootState{}
	ops := execPivotRootOps{
		mount: func(source, target, fstype string, flags uintptr, data string) error {
			if source == "" && target == "/" && fstype == "" && flags == syscall.MS_REC|syscall.MS_PRIVATE && data == "" {
				state.privateMount = true
			}
			return nil
		},
		mkdirAll: func(path string, perm os.FileMode) error {
			if !state.privateMount {
				t.Fatalf("put_old created before mount namespace was made private")
			}
			if filepath.Dir(path) != "/mnt/root" || filepath.Base(path) == "" || perm != 0o700 {
				t.Fatalf("put_old path/perm = %q %#o", path, perm)
			}
			state.putOld = path
			return nil
		},
		pivot: func(newroot, putold string) error {
			if newroot != "/mnt/root" || putold != state.putOld {
				t.Fatalf("pivot root = %q old = %q", newroot, putold)
			}
			state.pivoted = true
			return nil
		},
		chdir: func(dir string) error {
			if !state.pivoted || dir != "/" {
				t.Fatalf("chdir before pivot or wrong dir: pivoted=%v dir=%q", state.pivoted, dir)
			}
			state.cwd = dir
			return nil
		},
		unmount: func(target string, flags int) error {
			if state.cwd != "/" || flags != syscall.MNT_DETACH {
				t.Fatalf("unmount before chdir or wrong flags: cwd=%q flags=%#x", state.cwd, flags)
			}
			if filepath.Base(target) != ".ccx3-old-root" {
				t.Fatalf("old root cleanup target = %q", target)
			}
			state.oldRootTarget = target
			state.unmountedOldRoot = true
			return nil
		},
		remove: func(name string) error {
			if !state.unmountedOldRoot || name != state.oldRootTarget {
				t.Fatalf("remove before unmount or wrong target: %q", name)
			}
			state.removedOldRoot = true
			return nil
		},
	}
	if err := pivotExecRootWithOps("/mnt/root", ops); err != nil {
		t.Fatalf("pivotExecRootWithOps: %v", err)
	}
	if !state.privateMount || !state.pivoted || !state.unmountedOldRoot || !state.removedOldRoot {
		t.Fatalf("pivot root state = %+v", state)
	}
}

func TestApplyExecCredentialWithOps(t *testing.T) {
	var groups []int
	var gid, uid int
	ops := execCredentialOps{
		setgroups: func(values []int) error {
			groups = append([]int(nil), values...)
			return nil
		},
		setgid: func(value int) error {
			gid = value
			return nil
		},
		setuid: func(value int) error {
			uid = value
			return nil
		},
	}
	if err := applyExecCredentialWithOps("1000", "1001", "10,20", ops); err != nil {
		t.Fatalf("applyExecCredentialWithOps: %v", err)
	}
	if !reflect.DeepEqual(groups, []int{10, 20}) || gid != 1001 || uid != 1000 {
		t.Fatalf("credential calls groups=%v gid=%d uid=%d", groups, gid, uid)
	}
	if err := applyExecCredentialWithOps("1000", "1001", "bad", ops); err == nil {
		t.Fatalf("invalid group error = %v", err)
	}
}

func TestConfigureManagedExecProcessAttrsSetsProcessGroup(t *testing.T) {
	cmd, _ := managedExecCommand([]string{"true"}, nil, "", "", nil, false)
	configureManagedExecProcessAttrs(cmd, "", nil, false)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %+v, want Setpgid", cmd.SysProcAttr)
	}
}

func TestValidateManagedExecWorkDirRejectsInvalidDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateManagedExecWorkDir(root, "/missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing workdir error = %v", err)
	}
	if err := validateManagedExecWorkDir(root, "/file"); err == nil {
		t.Fatal("file workdir was accepted")
	}
}

func TestTerminateManagedExecProcessGroupReapsDescendants(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 30 >/dev/null 2>&1 & echo $!")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := startManagedExecProcess(cmd, nil); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if result := waitManagedExecProcess(cmd, started); result.ExitCode != 0 {
		t.Fatalf("shell exit = %+v", result)
	}
	terminateManagedExecProcessGroup(cmd.Process.Pid)
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("descendant %d still exists: %v", childPID, err)
	}
}

func TestConfigureManagedExecProcessAttrsSetsRootNamespace(t *testing.T) {
	cmd, _ := managedExecCommand([]string{"true"}, nil, "/mnt/root", "", nil, false)
	configureManagedExecProcessAttrs(cmd, "/mnt/root", nil, true)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWNS == 0 {
		t.Fatalf("Cloneflags = %#x, want CLONE_NEWNS", cmd.SysProcAttr.Cloneflags)
	}
	if cmd.SysProcAttr.Credential != nil {
		t.Fatalf("root namespace command unexpectedly set credential")
	}
}

func TestConfigureManagedExecProcessAttrsSetsDirectCredential(t *testing.T) {
	cred := &syscall.Credential{Uid: 1000, Gid: 1001}
	cmd, _ := managedExecCommand([]string{"true"}, nil, "", "", cred, false)
	configureManagedExecProcessAttrs(cmd, "", cred, false)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential != cred {
		t.Fatalf("Credential = %+v, want test credential", cmd.SysProcAttr.Credential)
	}

	pivotCmd, _ := managedExecCommand([]string{"true"}, nil, "", "", cred, true)
	configureManagedExecProcessAttrs(pivotCmd, "", cred, true)
	if pivotCmd.SysProcAttr == nil || pivotCmd.SysProcAttr.Credential != nil {
		t.Fatalf("pivot credential = %+v, want nil", pivotCmd.SysProcAttr.Credential)
	}
}

func TestManagedExecExitCode(t *testing.T) {
	if code, err := managedExecExitCode(nil, nil); code != 0 || err != nil {
		t.Fatalf("nil wait error = %d, %v; want 0, nil", code, err)
	}
	if code, err := managedExecExitCode(exec.ErrWaitDelay, nil); code != 0 || err != nil {
		t.Fatalf("ErrWaitDelay = %d, %v; want 0, nil", code, err)
	}
	wantErr := errors.New("wait failed")
	if code, err := managedExecExitCode(wantErr, nil); code != 126 || !errors.Is(err, wantErr) {
		t.Fatalf("ordinary wait error = %d, %v; want 126, %v", code, err, wantErr)
	}
}

func TestManagedExecExitCodeUsesProcessExitCode(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 42")
	err := cmd.Run()
	if err == nil {
		t.Fatalf("exit 42 unexpectedly succeeded")
	}
	code, gotErr := managedExecExitCode(err, cmd.ProcessState)
	if gotErr != nil {
		t.Fatalf("managedExecExitCode returned error: %v", gotErr)
	}
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}

	signaled := exec.Command("sh", "-c", "kill -TERM $$")
	err = signaled.Run()
	if err == nil {
		t.Fatalf("signaled command unexpectedly succeeded")
	}
	code, gotErr = managedExecExitCode(err, signaled.ProcessState)
	if gotErr != nil {
		t.Fatalf("signaled managedExecExitCode returned error: %v", gotErr)
	}
	want := 128 + int(syscall.SIGTERM)
	if code != want {
		t.Fatalf("signal exit code = %d, want %d", code, want)
	}
}

func TestStartManagedExecProcessClosesControlWriter(t *testing.T) {
	controlR, controlW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer controlR.Close()
	cmd := exec.Command("true")
	if err := startManagedExecProcess(cmd, &controlW); err != nil {
		t.Fatalf("startManagedExecProcess: %v", err)
	}
	if controlW != nil {
		t.Fatalf("control writer was not cleared")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if _, err := controlR.Read(make([]byte, 1)); err == nil {
		t.Fatalf("control reader did not observe writer close")
	}
}

func TestWaitManagedExecProcess(t *testing.T) {
	ok := exec.Command("true")
	if err := ok.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}
	result := waitManagedExecProcess(ok, time.Now())
	if result.WaitErr != nil || result.ExitErr != nil || result.ExitCode != 0 || result.ProcessState == nil || len(result.Usage) == 0 {
		t.Fatalf("true result = %+v", result)
	}

	failed := exec.Command("sh", "-c", "exit 7")
	if err := failed.Start(); err != nil {
		t.Fatalf("start exit 7: %v", err)
	}
	result = waitManagedExecProcess(failed, time.Now())
	if result.WaitErr != nil || result.ExitErr != nil || result.ExitCode != 7 || result.ProcessState == nil || len(result.Usage) == 0 {
		t.Fatalf("exit 7 result = %+v", result)
	}
}

func TestManagedExecStreamCount(t *testing.T) {
	tests := []struct {
		name       string
		tty        bool
		hasStdin   bool
		hasControl bool
		want       int
	}{
		{name: "pipe stdio", want: 2},
		{name: "pipe stdio control", hasControl: true, want: 3},
		{name: "pipe stdio stdin", hasStdin: true, want: 2},
		{name: "tty output only", tty: true, want: 1},
		{name: "tty stdin", tty: true, hasStdin: true, want: 2},
		{name: "tty control", tty: true, hasControl: true, want: 2},
		{name: "tty stdin control", tty: true, hasStdin: true, hasControl: true, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := managedExecStreamCount(tc.tty, tc.hasStdin, tc.hasControl)
			if got != tc.want {
				t.Fatalf("managedExecStreamCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWaitManagedExecStreams(t *testing.T) {
	done := make(chan struct{}, 2)
	done <- struct{}{}
	done <- struct{}{}
	waitManagedExecStreams(done, 2)
	if len(done) != 0 {
		t.Fatalf("done channel len = %d, want 0", len(done))
	}
}

func TestAttachManagedExecControlPipe(t *testing.T) {
	cmd, _ := managedExecCommand([]string{"true"}, nil, "", "", nil, false)
	controlR, controlW, err := attachManagedExecControlPipe(cmd, false)
	if err != nil {
		t.Fatalf("disabled attach: %v", err)
	}
	if controlR != nil || controlW != nil || len(cmd.ExtraFiles) != 0 {
		t.Fatalf("disabled attach = (%v, %v, extra=%d), want nil nil 0", controlR, controlW, len(cmd.ExtraFiles))
	}

	controlR, controlW, err = attachManagedExecControlPipe(cmd, true)
	if err != nil {
		t.Fatalf("enabled attach: %v", err)
	}
	defer controlR.Close()
	defer controlW.Close()
	if controlR == nil || controlW == nil {
		t.Fatalf("enabled attach returned nil pipe")
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != controlW {
		t.Fatalf("ExtraFiles = %#v, want control writer", cmd.ExtraFiles)
	}
}

func TestCopyManagedExecReaderEmitsFirstAndCloses(t *testing.T) {
	done := make(chan struct{}, 1)
	firstCalls := 0
	closed := false
	var got bytes.Buffer
	copyManagedExecReader(done, bytes.NewBufferString("hello"), func() error {
		closed = true
		return nil
	}, func() {
		firstCalls++
	}, func(data []byte) {
		got.Write(data)
	})
	<-done
	if firstCalls != 1 {
		t.Fatalf("first calls = %d, want 1", firstCalls)
	}
	if got.String() != "hello" {
		t.Fatalf("output = %q", got.String())
	}
	if !closed {
		t.Fatalf("reader close callback was not called")
	}
}

func TestCopyManagedExecReaderDrainsWithoutEmitter(t *testing.T) {
	done := make(chan struct{}, 1)
	firstCalls := 0
	closed := false
	copyManagedExecReader(done, bytes.NewBufferString("ignored"), func() error {
		closed = true
		return nil
	}, func() {
		firstCalls++
	}, nil)
	<-done
	if firstCalls != 0 {
		t.Fatalf("first calls = %d, want 0", firstCalls)
	}
	if !closed {
		t.Fatalf("reader close callback was not called")
	}
}

func TestCopyManagedExecReaderEmitsWithoutFirstCallback(t *testing.T) {
	done := make(chan struct{}, 1)
	var got bytes.Buffer
	copyManagedExecReader(done, bytes.NewBufferString("control"), nil, nil, func(data []byte) {
		got.Write(data)
	})
	<-done
	if got.String() != "control" {
		t.Fatalf("output = %q", got.String())
	}
}

func TestCopyManagedExecStdinToPTYWritesEOTOnEOF(t *testing.T) {
	done := make(chan struct{}, 1)
	stdin := &trackingReadCloser{Buffer: bytes.NewBufferString("input")}
	var pty bytes.Buffer
	copyManagedExecStdinToPTY(done, stdin, &pty)
	<-done
	if got := pty.Bytes(); !reflect.DeepEqual(got, []byte{'i', 'n', 'p', 'u', 't', 4}) {
		t.Fatalf("pty bytes = %#v", got)
	}
	if !stdin.closed {
		t.Fatalf("stdin was not closed")
	}
}

func TestCopyManagedExecStdinToPipeClosesBothEnds(t *testing.T) {
	done := make(chan struct{}, 1)
	stdin := &trackingReadCloser{Buffer: bytes.NewBufferString("input")}
	childStdin := &trackingWriteCloser{}
	copyManagedExecStdinToPipe(done, stdin, childStdin)
	<-done
	if got := childStdin.String(); got != "input" {
		t.Fatalf("child stdin = %q, want input", got)
	}
	if !stdin.closed {
		t.Fatalf("stdin was not closed")
	}
	if !childStdin.closed {
		t.Fatalf("child stdin was not closed")
	}
}

func TestManagedExecTimingPhaseSelection(t *testing.T) {
	if begin, done, ok := managedExecGuestReadyTimingPhases(nil); ok || begin != "" || done != "" {
		t.Fatalf("nil wait ready phases = (%q, %q, %v), want disabled", begin, done, ok)
	}
	begin, done, ok := managedExecGuestReadyTimingPhases(func() error { return nil })
	if !ok || begin != managedExecTimingGuestReadyBegin || done != managedExecTimingGuestReadyDone {
		t.Fatalf("wait ready phases = (%q, %q, %v)", begin, done, ok)
	}
	if got := managedExecFirstOutputTimingPhase(false); got != managedExecTimingFirstStdout {
		t.Fatalf("stdout first phase = %q", got)
	}
	if got := managedExecFirstOutputTimingPhase(true); got != managedExecTimingFirstStderr {
		t.Fatalf("stderr first phase = %q", got)
	}
}

func TestManagedExecStartFailureUsesShellExitConventions(t *testing.T) {
	code, message := managedExecStartFailure(&os.PathError{Op: "fork/exec", Path: "missing", Err: syscall.ENOENT}, []string{"missing"}, "", "/")
	if code != 127 || message != "missing: executable not found\n" {
		t.Fatalf("missing executable result = (%d, %q)", code, message)
	}
	code, message = managedExecStartFailure(&os.PathError{Op: "fork/exec", Path: "/bin/private", Err: syscall.EACCES}, []string{"/bin/private"}, "", "/")
	if code != 126 || message != "/bin/private: permission denied\n" {
		t.Fatalf("permission result = (%d, %q)", code, message)
	}
}

func TestManagedExecDoneStreamCount(t *testing.T) {
	tests := []struct {
		name       string
		tty        bool
		hasStdin   bool
		hasControl bool
		want       int
	}{
		{name: "non_tty", want: 2},
		{name: "non_tty_stdin", hasStdin: true, want: 2},
		{name: "non_tty_control_stdin", hasStdin: true, hasControl: true, want: 3},
		{name: "tty_stdin", tty: true, hasStdin: true, want: 2},
		{name: "tty_control_stdin", tty: true, hasStdin: true, hasControl: true, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedExecDoneStreamCount(tc.tty, tc.hasStdin, tc.hasControl); got != tc.want {
				t.Fatalf("managedExecDoneStreamCount(%t, %t, %t) = %d, want %d", tc.tty, tc.hasStdin, tc.hasControl, got, tc.want)
			}
		})
	}
}

func TestHandleInitControlRequest(t *testing.T) {
	var control bytes.Buffer
	active := guestagent.NewActiveExecSet()
	if handled := handleInitControlRequest(config{}, &control, active, execRequest{Kind: "exec"}); handled {
		t.Fatalf("exec request handled as control")
	}

	fake := &fakeInitActiveExec{}
	active.Add("7", fake)
	if handled := handleInitControlRequest(config{}, &control, active, execRequest{Kind: "stdin", ID: "7", Stdin: []byte("input")}); !handled {
		t.Fatalf("stdin request was not handled")
	}
	if fake.stdin != "input" {
		t.Fatalf("stdin = %q", fake.stdin)
	}
	if handled := handleInitControlRequest(config{}, &control, active, execRequest{Kind: "resize", ID: "7", Cols: 100, Rows: 40}); !handled {
		t.Fatalf("resize request was not handled")
	}
	if fake.cols != 100 || fake.rows != 40 {
		t.Fatalf("resize = %dx%d", fake.cols, fake.rows)
	}
	if handled := handleInitControlRequest(config{}, &control, active, execRequest{Kind: "unknown"}); !handled {
		t.Fatalf("unknown request was not handled")
	}
}

type fakeInitActiveExec struct {
	stdin  string
	closed bool
	signal string
	cols   int
	rows   int
}

func (f *fakeInitActiveExec) WriteStdin(data []byte) error {
	f.stdin += string(data)
	return nil
}

func (f *fakeInitActiveExec) CloseStdin() error {
	f.closed = true
	return nil
}

func (f *fakeInitActiveExec) Signal(name string) error {
	f.signal = name
	return nil
}

func (f *fakeInitActiveExec) Resize(cols, rows int) error {
	f.cols = cols
	f.rows = rows
	return nil
}

func parseManagedExecPivotRequest(t *testing.T, cmd *exec.Cmd) execPivotRequest {
	t.Helper()
	if len(cmd.Args) < 2 || cmd.Args[1] != execPivotMode {
		t.Fatalf("command does not start an exec pivot: %#v", cmd.Args)
	}
	req, err := parseExecPivotArgs(cmd.Args[2:])
	if err != nil {
		t.Fatalf("parse exec pivot command: %v", err)
	}
	return req
}

type execPivotRootState struct {
	privateMount     bool
	putOld           string
	pivoted          bool
	cwd              string
	oldRootTarget    string
	unmountedOldRoot bool
	removedOldRoot   bool
}

type trackingReadCloser struct {
	*bytes.Buffer
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type trackingWriteCloser struct {
	bytes.Buffer
	closed bool
}

func (w *trackingWriteCloser) Close() error {
	w.closed = true
	return nil
}
