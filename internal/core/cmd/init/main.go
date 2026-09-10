//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/tinyrange/crumblecracker/internal/core/managed/capturerelay"
	"github.com/tinyrange/crumblecracker/internal/core/managed/guestagent"
	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
	"golang.org/x/sys/unix"
)

const configPath = "/etc/ccx3-init.json"
const guestQEMUPath = "/run/ccx3/qemu-x86_64"
const guestQEMUBinfmtPath = "/run/ccx3-qemu-x86_64"
const initDurationMarker = "__CCX3_INIT_MS__:"
const defaultControlMarkerPref = "__CCX3_CTL__:"
const fatalBootMarker = "ccx3-init-fatal: "
const execPivotMode = "--ccx3-exec-pivot"
const archiveMode = "--ccx3-fs-archive"
const extractMode = "--ccx3-fs-extract"
const captureRelayMode = "--ccx3-capture-relay"

var credentialSetupMu sync.Mutex

const (
	managedExecTimingRecv            = "recv"
	managedExecTimingGuestReadyBegin = "guest_ready_begin"
	managedExecTimingGuestReadyDone  = "guest_ready_done"
	managedExecTimingStartBegin      = "start_begin"
	managedExecTimingFirstStdout     = "first_stdout"
	managedExecTimingFirstStderr     = "first_stderr"
	managedExecTimingStartCall       = "start_call"
	managedExecTimingStarted         = "started"
	managedExecTimingWaitBegin       = "wait_begin"
	managedExecTimingWaitDone        = "wait_done"
	managedExecTimingStreamsDone     = "streams_done"
	managedExecTimingExitSent        = "exit_sent"
)
const stage2Mode = "--ccx3-stage2"
const stage2Path = "/etc/ccx3/stage2"
const stage2ConfigPath = "/etc/ccx3/config.json"
const snapshotStage2Path = "/var/tmp/.ccx3-snapshot-stage2"
const snapshotStage2ConfigPath = "/var/tmp/.ccx3-snapshot-config.json"
const snapshotModuleDir = "/var/tmp/.ccx3-snapshot-modules"
const initSystemSystemd = "systemd"
const systemdReadyTimeout = 30 * time.Second

// Exec output is base64-framed by the managed protocol. Small fragments create
// thousands of sidecar frames for ordinary file transfers and can overflow a
// bounded transport queue even while its consumer is making progress.
const execProtocolChunkSize = 32 << 10
const defaultVsockConnectTimeout = 5 * time.Second
const stage2VsockConnectAttemptTimeout = 500 * time.Millisecond
const managedExecGroupTerminateGrace = 500 * time.Millisecond
const managedExecGroupKillWait = 2 * time.Second

var consoleFD = 2
var kmsgFD = -1
var protocolMu sync.Mutex
var setTimeOfDay = unix.Settimeofday
var managedChildren = struct {
	sync.Mutex
	pids map[int]struct{}
}{pids: make(map[int]struct{})}

type config struct {
	Command            []string `json:"command"`
	Env                []string `json:"env"`
	WorkDir            string   `json:"workdir"`
	User               string   `json:"user,omitempty"`
	InitSystem         string   `json:"init,omitempty"`
	Hostname           string   `json:"hostname,omitempty"`
	Modules            []string `json:"modules"`
	EmulatorTag        string   `json:"emulator_tag,omitempty"`
	RootFSTag          string   `json:"rootfs_tag"`
	RootFSImagePath    string   `json:"rootfs_image_path,omitempty"`
	RootFSImageType    string   `json:"rootfs_image_type,omitempty"`
	Shares             []share  `json:"shares,omitempty"`
	VsockPort          uint32   `json:"vsock_port,omitempty"`
	ReadyMarker        string   `json:"ready_marker"`
	BeginMarker        string   `json:"begin_marker"`
	OutputMarkerPref   string   `json:"output_marker_prefix"`
	ErrorMarkerPref    string   `json:"error_marker_prefix"`
	ControlMarkerPref  string   `json:"control_marker_prefix"`
	UsageMarkerPref    string   `json:"usage_marker_prefix"`
	ExitMarkerPrefix   string   `json:"exit_marker_prefix"`
	PrecopyAMD64Root   bool     `json:"precopy_amd64_root,omitempty"`
	DisableCgroupMount bool     `json:"disable_cgroup_mount,omitempty"`
	Network            *network `json:"network,omitempty"`
	SnapshotMMIOBase   uint64   `json:"snapshot_mmio_base,omitempty"`
	SnapshotModules    []string `json:"snapshot_modules,omitempty"`
	UnixTime           int64    `json:"unix_time,omitempty"`
	KernelRelease      string   `json:"kernel_release,omitempty"`
	ModuleSymversPath  string   `json:"module_symvers_path,omitempty"`
}

type network struct {
	Interface string `json:"interface,omitempty"`
	Address   string `json:"address,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	DNS       string `json:"dns,omitempty"`
}

func installModuleSymvers(root, release string, data []byte) error {
	release = strings.TrimSpace(release)
	if release == "" || release == "." || release == ".." || filepath.Base(release) != release {
		return fmt.Errorf("invalid kernel release %q", release)
	}
	if len(data) == 0 {
		return fmt.Errorf("Module.symvers is empty")
	}

	headersDir := filepath.Join(root, "usr", "src", "linux-headers-"+release)
	if err := os.MkdirAll(headersDir, 0o755); err != nil {
		return fmt.Errorf("create kernel metadata directory: %w", err)
	}
	symversPath := filepath.Join(headersDir, "Module.symvers")
	tmp, err := os.CreateTemp(headersDir, ".Module.symvers-*")
	if err != nil {
		return fmt.Errorf("create temporary Module.symvers: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("set Module.symvers permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write Module.symvers: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close Module.symvers: %w", err)
	}
	if err := os.Rename(tmpPath, symversPath); err != nil {
		return fmt.Errorf("install Module.symvers: %w", err)
	}
	ok = true

	modulesDir := filepath.Join(root, "lib", "modules", release)
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		return fmt.Errorf("create kernel module directory: %w", err)
	}
	buildPath := filepath.Join(modulesDir, "build")
	if _, err := os.Lstat(buildPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect kernel build link: %w", err)
	}
	linkTarget := filepath.Join("..", "..", "..", "usr", "src", "linux-headers-"+release)
	if err := os.Symlink(linkTarget, buildPath); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("install kernel build link: %w", err)
	}
	return nil
}

type share struct {
	Tag      string `json:"tag"`
	Mount    string `json:"mount"`
	Writable bool   `json:"writable,omitempty"`
}

type execRequest = protocol.ManagedExecRequest

type preparedExecRequest struct {
	ID        string
	Command   []string
	Env       []string
	RootDir   string
	WorkDir   string
	User      string
	Stdin     []byte
	TTY       bool
	ControlFD bool
	Cols      int
	Rows      int
}

type execRequestValidation struct {
	Message  string
	ExitCode int
}

type systemdCommandGate struct {
	enabled bool
	ready   bool
	wait    func(time.Duration) error
	mu      sync.Mutex
}

type managedExec struct {
	stdinMu sync.Mutex
	stdin   io.WriteCloser
	pty     *os.File
	process *os.Process
	family  *guestagent.ProcessFamily
	group   bool
	start   time.Time
}

func (m *managedExec) writeStdin(data []byte) error {
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	if m.stdin == nil {
		return fmt.Errorf("stdin is closed")
	}
	_, err := m.stdin.Write(data)
	return err
}

func (m *managedExec) closeStdin() error {
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	if m.stdin == nil {
		return nil
	}
	err := m.stdin.Close()
	m.stdin = nil
	return err
}

func (m *managedExec) setProcess(proc *os.Process, group bool, family *guestagent.ProcessFamily) {
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	m.process = proc
	m.family = family
	m.group = group
}

func (m *managedExec) setPTY(pty *os.File) {
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	m.pty = pty
}

func (m *managedExec) signal(name string) error {
	sig, err := guestagent.ParseSignal(name)
	if err != nil {
		return err
	}
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	if m.process == nil {
		return fmt.Errorf("process is not started")
	}
	process := m.process
	family := m.family
	if family != nil {
		_ = family.Signal(sig)
	}
	if m.group && process.Pid > 0 {
		err := syscall.Kill(-process.Pid, sig)
		if err == nil {
			if family != nil {
				return family.Signal(sig)
			}
			return nil
		}
		if errors.Is(err, syscall.ESRCH) {
			if family != nil {
				return family.Signal(sig)
			}
			return nil
		}
	}
	err = process.Signal(sig)
	if family != nil {
		if familyErr := family.Signal(sig); err == nil {
			err = familyErr
		}
	}
	return err
}

func (m *managedExec) resize(cols, rows int) error {
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	if m.pty == nil {
		return fmt.Errorf("exec has no tty")
	}
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid tty size %dx%d", cols, rows)
	}
	return unix.IoctlSetWinsize(int(m.pty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Col: uint16(cols),
		Row: uint16(rows),
	})
}

func (m *managedExec) WriteStdin(data []byte) error {
	return m.writeStdin(data)
}

func (m *managedExec) CloseStdin() error {
	return m.closeStdin()
}

func (m *managedExec) Signal(name string) error {
	return m.signal(name)
}

func (m *managedExec) Resize(cols, rows int) error {
	return m.resize(cols, rows)
}

func validateExecRequest(req execRequest) execRequestValidation {
	if len(req.Command) == 0 {
		return execRequestValidation{Message: "exec request missing command", ExitCode: 125}
	}
	if req.ID == "" {
		return execRequestValidation{Message: "exec request missing id"}
	}
	return execRequestValidation{}
}

func prepareExecRequest(cfg config, req execRequest) preparedExecRequest {
	workDir := req.WorkDir
	if workDir == "" {
		workDir = cfg.WorkDir
	}
	env := req.Env
	if len(env) == 0 {
		env = cfg.Env
	}
	user := req.User
	if user == "" {
		user = cfg.User
	}
	return preparedExecRequest{
		ID:        req.ID,
		Command:   append([]string(nil), req.Command...),
		Env:       append([]string(nil), env...),
		RootDir:   req.RootDir,
		WorkDir:   workDir,
		User:      user,
		Stdin:     append([]byte(nil), req.Stdin...),
		TTY:       req.TTY,
		ControlFD: req.ControlFD,
		Cols:      req.Cols,
		Rows:      req.Rows,
	}
}

func startPreparedExec(cfg config, control io.Writer, active *guestagent.ActiveExecSet, req preparedExecRequest, waitReady func() error) {
	// Use a kernel pipe for command input. An io.Pipe requires a guest Go
	// goroutine to rendezvous with every write; immediately after snapshot
	// restore that rendezvous could stall even though the child was already
	// running. The kernel pipe also preserves natural backpressure without an
	// arbitrary in-memory input limit.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		writeExecStderr(cfg, control, req.ID, "ccx3-init: open command stdin: "+err.Error()+"\n")
		reporterForConfig(cfg, control, req.ID, time.Now()).Exit(126)
		return
	}
	managed := &managedExec{stdin: stdinW, start: time.Now()}
	closeStdin := active.Add(req.ID, managed)
	// The input sink is fully usable once it is registered: the kernel pipe can
	// queue data before the command worker runs. Advertise that state without a
	// goroutine rendezvous, which may not be scheduled promptly after restore.
	writeExecTiming(control, req.ID, "input_ready", managed.start)
	go func() {
		runManagedExec(cfg, control, req.ID, req.Command, req.Env, req.RootDir, req.WorkDir, req.User, stdinR, managed, req.TTY, req.ControlFD, req.Cols, req.Rows, waitReady, func() {
			_ = managed.closeStdin()
			active.Delete(req.ID)
		})
	}()
	if closeStdin {
		if err := managed.closeStdin(); err != nil {
			writeKernel("ccx3-init: close pending stdin: " + err.Error())
		}
	}
	if len(req.Stdin) == 0 {
		return
	}
	initialStdin := append([]byte(nil), req.Stdin...)
	go func(id string, managed *managedExec) {
		if err := managed.writeStdin(initialStdin); err != nil {
			writeKernel("ccx3-init: write initial stdin: " + err.Error())
		}
		if err := managed.closeStdin(); err != nil {
			writeKernel("ccx3-init: close initial stdin: " + err.Error())
		}
	}(req.ID, managed)
}

func runPreparedExecInline(cfg config, control io.Writer, req preparedExecRequest, waitReady func() error) {
	managed := &managedExec{start: time.Now()}
	stdin, cleanup, err := inlineExecStdin(req.Stdin)
	if err != nil {
		writeKernel("ccx3-init: prepare inline stdin: " + err.Error())
		writeExecStderr(cfg, control, req.ID, "ccx3-init: prepare inline stdin: "+err.Error()+"\n")
		reporterForConfig(cfg, control, req.ID, time.Now()).Exit(126)
		return
	}
	runManagedExec(cfg, control, req.ID, req.Command, req.Env, req.RootDir, req.WorkDir, req.User, stdin, managed, req.TTY, req.ControlFD, req.Cols, req.Rows, waitReady, cleanup)
}

func inlineExecStdin(data []byte) (io.ReadCloser, func(), error) {
	if len(data) == 0 {
		return nil, func() {}, nil
	}
	file, err := os.CreateTemp("", "ccx3-stdin-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, err
	}
	return file, cleanup, nil
}

func newSystemdCommandGate(initSystem string) *systemdCommandGate {
	return &systemdCommandGate{
		enabled: strings.TrimSpace(initSystem) == initSystemSystemd,
		ready:   strings.TrimSpace(initSystem) != initSystemSystemd,
		wait:    waitForSystemdCommandReady,
	}
}

func (g *systemdCommandGate) WaitForCommand(argv []string) func() error {
	if g == nil || !g.enabled || !commandNeedsSystemdReady(argv) {
		return nil
	}
	return func() error {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.ready {
			return nil
		}
		wait := g.wait
		if wait == nil {
			wait = waitForSystemdCommandReady
		}
		if err := wait(systemdReadyTimeout); err != nil {
			return err
		}
		g.ready = true
		return nil
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == captureRelayMode {
		if err := capturerelay.Run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ccx3-init: capture relay:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == stage2Mode {
		if err := runStage2(); err != nil {
			writeKernel("ccx3-init: stage2: " + err.Error())
			fmt.Fprintf(os.Stderr, "ccx3-init: stage2: %v\n", err)
			os.Exit(126)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == execPivotMode {
		if err := runExecPivot(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ccx3-init: exec pivot: %v\n", err)
			os.Exit(126)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == archiveMode {
		if err := runArchiveHelper(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ccx3-init: fs archive: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == extractMode {
		if err := runExtractHelper(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ccx3-init: fs extract: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		writeKernel(fatalBootMarker + err.Error())
		writeConsole(fatalBootMarker + err.Error() + "\n")
		for {
			syscall.Pause()
		}
	}
}

func run() error {
	fd, err := syscall.Open("/dev/console", syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err == nil {
		_ = syscall.SetNonblock(fd, false)
		consoleFD = fd
		for _, target := range []int{0, 1, 2} {
			_ = syscall.Dup3(fd, target, 0)
		}
	}
	openKernelLog()

	var cfg config
	buf, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(buf, &cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/"
	}
	if cfg.Hostname == "" || cfg.Hostname == "(none)" {
		cfg.Hostname = "ccx3"
	}
	if err := configureClock(cfg.UnixTime); err != nil {
		return err
	}
	bootStart := time.Now()

	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	_ = syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	if !cfg.DisableCgroupMount {
		mountCgroupFS("/sys/fs/cgroup")
	}
	configureMemoryOvercommit("/proc")

	modules := cfg.Modules
	var moduleSymvers []byte
	if cfg.ModuleSymversPath != "" {
		moduleSymvers, err = os.ReadFile(cfg.ModuleSymversPath)
		if err != nil {
			return fmt.Errorf("read staged Module.symvers: %w", err)
		}
		if len(moduleSymvers) == 0 {
			return fmt.Errorf("staged Module.symvers is empty")
		}
	}
	var deferredModulePaths []string
	var deferredModules []modulePayload
	if cfg.SnapshotMMIOBase != 0 {
		modules, deferredModulePaths = splitSnapshotDeferredModules(cfg.Modules)
		deferredModules, err = readModulePayloads(deferredModulePaths)
		if err != nil {
			return err
		}
	}
	writeStage(bootStart, "loading modules")
	if err := loadModules(modules); err != nil {
		return err
	}
	writeStage(bootStart, "modules loaded")
	if cfg.RootFSTag != "" || cfg.RootFSImagePath != "" {
		writeStage(bootStart, "mounting rootfs")
		if err := mountConfiguredRootFS(cfg); err != nil {
			return err
		}
		writeStage(bootStart, "rootfs mounted")
		if len(moduleSymvers) > 0 {
			writeStage(bootStart, "installing kernel symbol versions")
			if err := installModuleSymvers("/", cfg.KernelRelease, moduleSymvers); err != nil {
				return fmt.Errorf("install kernel symbol versions: %w", err)
			}
			writeStage(bootStart, "kernel symbol versions installed")
		}
		writeStage(bootStart, "configuring hostname")
		if err := configureHostname(cfg.Hostname); err != nil {
			return fmt.Errorf("configure hostname: %w", err)
		}
		writeStage(bootStart, "hostname configured")
		writeStage(bootStart, "configuring runtime filesystem")
		if err := configureRuntimeFilesystem(); err != nil {
			return fmt.Errorf("configure runtime filesystem: %w", err)
		}
		writeStage(bootStart, "runtime filesystem configured")
		writeStage(bootStart, "configuring package managers")
		if err := configurePackageManagers(""); err != nil {
			return fmt.Errorf("configure package managers: %w", err)
		}
		writeStage(bootStart, "package managers configured")
		if cfg.PrecopyAMD64Root {
			writeStage(bootStart, "precopying amd64 root")
			if err := precopyAMD64Root(); err != nil {
				return fmt.Errorf("precopy amd64 root: %w", err)
			}
			writeStage(bootStart, "amd64 root precopied")
		}
		writeStage(bootStart, "configuring binfmt")
		if configured, err := configureBinfmt(); err != nil {
			return fmt.Errorf("configure binfmt: %w", err)
		} else if configured {
			writeStage(bootStart, "binfmt configured")
		} else {
			writeStage(bootStart, "binfmt unavailable")
		}
	}
	if cfg.Network != nil {
		writeStage(bootStart, "configuring network")
		if err := configureNetwork(cfg.Network); err != nil {
			return fmt.Errorf("configure network: %w", err)
		}
		writeStage(bootStart, "network configured")
	}
	writeStage(bootStart, "changing workdir")
	if err := os.Chdir(cfg.WorkDir); err != nil {
		return fmt.Errorf("chdir %s: %w", cfg.WorkDir, err)
	}

	if len(cfg.Command) == 0 {
		if cfg.VsockPort != 0 {
			if strings.TrimSpace(cfg.InitSystem) != "" {
				return bootInitSystem(cfg, bootStart)
			}
			if len(deferredModules) > 0 {
				writeStage(bootStart, "staging deferred modules")
				cfg.SnapshotModules, err = stageSnapshotModules(deferredModules)
				if err != nil {
					return err
				}
				writeStage(bootStart, "deferred modules staged")
			}
			if cfg.SnapshotMMIOBase != 0 {
				writeStage(bootStart, "preparing snapshot stage2")
				if err := prepareSnapshotStage2(cfg); err != nil {
					return fmt.Errorf("prepare snapshot stage2: %w", err)
				}
				writeStage(bootStart, "snapshot stage2 prepared")
			}
			if err := triggerSnapshotMMIO(cfg.SnapshotMMIOBase); err != nil {
				return fmt.Errorf("trigger snapshot: %w", err)
			}
			if cfg.SnapshotMMIOBase != 0 {
				writeStage(bootStart, "seeding entropy")
				if err := seedEntropyFromHostRNG(64); err != nil {
					return fmt.Errorf("seed entropy after snapshot restore: %w", err)
				}
				writeStage(bootStart, "entropy seeded")
				return execSnapshotStage2()
			}
			writeStage(bootStart, "connecting vsock control")
			control, err := connectVsock(cfg.VsockPort)
			if err != nil {
				return fmt.Errorf("connect vsock control: %w", err)
			}
			defer control.Close()
			if cfg.ReadyMarker != "" {
				writeProtocolLineTo(control, initDurationMarker+itoa(int(time.Since(bootStart).Milliseconds())))
				writeStage(bootStart, "sending ready marker")
				writeProtocolLineTo(control, cfg.ReadyMarker)
			}
			return commandLoop(cfg, control)
		}
		if cfg.ReadyMarker != "" {
			writeKernel(cfg.ReadyMarker)
			if kmsgFD >= 0 {
				writeConsole(cfg.ReadyMarker + "\n")
			}
		}
		return commandLoop(cfg, os.Stdin)
	}

	if cfg.BeginMarker != "" {
		writeKernel(cfg.BeginMarker)
	}

	writeKernel("ccx3-init: exec " + strings.Join(cfg.Command, " "))
	if err := execCommand(cfg); err != nil {
		return err
	}
	return fmt.Errorf("exec command returned unexpectedly")
}

func runStage2() error {
	openKernelLog()
	writeKernel("ccx3-init: stage2 starting")
	snapshotRestore := os.Getenv("CCX3_STAGE2_CONFIG") == snapshotStage2ConfigPath
	var cfg config
	buf, err := readStage2Config()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(buf, &cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	if snapshotRestore {
		if err := loadModules(cfg.SnapshotModules); err != nil {
			return fmt.Errorf("load snapshot modules: %w", err)
		}
		_ = os.RemoveAll(snapshotModuleDir)
		_ = os.Remove(snapshotStage2ConfigPath)
		_ = os.Remove(snapshotStage2Path)
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/"
	}
	if cfg.VsockPort == 0 {
		return fmt.Errorf("vsock control port is not configured")
	}
	writeKernel("ccx3-init: stage2 config loaded")
	if err := os.Chdir(cfg.WorkDir); err != nil {
		return fmt.Errorf("chdir %s: %w", cfg.WorkDir, err)
	}
	control, err := connectStage2Control(cfg.VsockPort)
	if err != nil {
		return fmt.Errorf("connect vsock control: %w", err)
	}
	defer control.Close()
	if cfg.ReadyMarker != "" {
		writeProtocolLineTo(control, initDurationMarker+"0")
		writeProtocolLineTo(control, cfg.ReadyMarker)
	}
	return commandLoop(cfg, control)
}

func prepareSnapshotStage2(cfg config) error {
	if err := copyCurrentExecutable(snapshotStage2Path); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode snapshot stage2 config: %w", err)
	}
	if err := os.WriteFile(snapshotStage2ConfigPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write snapshot stage2 config: %w", err)
	}
	if err := os.Chmod(snapshotStage2ConfigPath, 0o600); err != nil {
		return fmt.Errorf("chmod snapshot stage2 config: %w", err)
	}
	return nil
}

func stageSnapshotModules(modules []modulePayload) ([]string, error) {
	if err := os.RemoveAll(snapshotModuleDir); err != nil {
		return nil, fmt.Errorf("clear snapshot module directory: %w", err)
	}
	if err := os.MkdirAll(snapshotModuleDir, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot module directory: %w", err)
	}
	paths := make([]string, 0, len(modules))
	for i, module := range modules {
		path := filepath.Join(snapshotModuleDir, fmt.Sprintf("%03d-%s", i, filepath.Base(module.path)))
		if err := os.WriteFile(path, module.data, 0o600); err != nil {
			return nil, fmt.Errorf("stage snapshot module %s: %w", module.path, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func execSnapshotStage2() error {
	env := append(os.Environ(), "CCX3_STAGE2_CONFIG="+snapshotStage2ConfigPath)
	if err := syscall.Exec(snapshotStage2Path, []string{snapshotStage2Path, stage2Mode}, env); err != nil {
		return fmt.Errorf("exec snapshot stage2: %w", err)
	}
	return fmt.Errorf("snapshot stage2 returned unexpectedly")
}

func waitForSystemdControlSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := socketExists("/run/systemd/private")
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd control socket did not appear within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func socketExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return false, fmt.Errorf("%s exists but is not a socket", path)
		}
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

func systemdSystemBusExpected() bool {
	for _, candidate := range []string{
		"/usr/bin/dbus-daemon",
		"/bin/dbus-daemon",
		"/usr/bin/dbus-broker-launch",
		"/lib/systemd/system/dbus.service",
		"/usr/lib/systemd/system/dbus.service",
	} {
		if pathExists(candidate) {
			return true
		}
	}
	return false
}

func readStage2Config() ([]byte, error) {
	if path := strings.TrimSpace(os.Getenv("CCX3_STAGE2_CONFIG")); path != "" {
		return os.ReadFile(path)
	}
	buf, err := os.ReadFile(stage2ConfigPath)
	if err == nil {
		return buf, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return os.ReadFile(configPath)
}

func connectStage2Control(port uint32) (*os.File, error) {
	start := time.Now()
	lastLog := start
	for attempt := 0; ; attempt++ {
		control, err := connectVsockWithTimeout(port, stage2VsockConnectAttemptTimeout)
		if err == nil {
			if attempt > 0 || time.Since(start) > stage2VsockConnectAttemptTimeout {
				writeKernel(fmt.Sprintf("ccx3-init: stage2 vsock connected after %s (%d retries)", time.Since(start).Round(time.Millisecond), attempt))
			}
			return control, nil
		}
		if attempt == 0 {
			writeKernel("ccx3-init: stage2 waiting for vsock control: " + err.Error())
			lastLog = time.Now()
		} else if time.Since(lastLog) >= 5*time.Second {
			writeKernel(fmt.Sprintf("ccx3-init: stage2 still waiting for vsock control after %s: %v", time.Since(start).Round(time.Millisecond), err))
			lastLog = time.Now()
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func bootInitSystem(cfg config, bootStart time.Time) error {
	initSystem := strings.TrimSpace(cfg.InitSystem)
	switch initSystem {
	case "", "ccx3":
		return fmt.Errorf("init system is not configured")
	case initSystemSystemd:
		return bootSystemd(cfg, bootStart)
	default:
		return fmt.Errorf("unsupported init system %q", initSystem)
	}
}

func bootSystemd(cfg config, bootStart time.Time) error {
	systemdPath, err := findSystemd()
	if err != nil {
		return err
	}
	writeStage(bootStart, "installing ccx3 stage2")
	if err := installSystemdStage2(cfg); err != nil {
		return err
	}
	writeStage(bootStart, "exec systemd")
	args := []string{systemdPath}
	if headlessGuest(cfg.Env) {
		if err := installHeadlessSystemd("/", cfg.Env); err != nil {
			return err
		}
		args = append(args, "--unit=ccx3-headless.target")
	}
	return syscall.Exec(systemdPath, args, os.Environ())
}

func findSystemd() (string, error) {
	for _, candidate := range []string{"/lib/systemd/systemd", "/usr/lib/systemd/systemd"} {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		return candidate, nil
	}
	if target, err := filepath.EvalSymlinks("/sbin/init"); err == nil && strings.Contains(target, "systemd") {
		return "/sbin/init", nil
	}
	return "", fmt.Errorf("systemd init requested but no systemd binary was found")
}

func installSystemdStage2(cfg config) error {
	if err := copyCurrentExecutable(stage2Path); err != nil {
		return err
	}
	configData, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode stage2 config: %w", err)
	}
	if err := os.WriteFile(stage2ConfigPath, append(configData, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", stage2ConfigPath, err)
	}
	if err := os.MkdirAll("/etc/systemd/system/sysinit.target.wants", 0o755); err != nil {
		return fmt.Errorf("mkdir systemd runtime units: %w", err)
	}
	unitPath := "/etc/systemd/system/ccx3-stage2.service"
	unit := `[Unit]
Description=ccx3 guest control stage2
DefaultDependencies=no
Before=sysinit.target
StartLimitIntervalSec=0

[Service]
Type=simple
Environment=CCX3_STAGE2_CONFIG=/etc/ccx3/config.json
ExecStart=/etc/ccx3/stage2 --ccx3-stage2
Restart=on-failure
RestartSec=250ms
StandardInput=null
StandardOutput=null
StandardError=null

[Install]
WantedBy=sysinit.target
`
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}
	if err := ensureSymlink(unitPath, "/etc/systemd/system/sysinit.target.wants/ccx3-stage2.service"); err != nil {
		return fmt.Errorf("enable ccx3 stage2: %w", err)
	}
	for _, unit := range []string{
		"systemd-networkd-wait-online.service",
		"NetworkManager-wait-online.service",
		"systemd-remount-fs.service",
	} {
		if err := ensureSymlink("/dev/null", filepath.Join("/run/systemd/system", unit)); err != nil {
			return fmt.Errorf("mask %s: %w", unit, err)
		}
	}
	return nil
}

func copyCurrentExecutable(dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	src, err := os.Open("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("open current executable: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy current executable to %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}

func runExecPivot(args []string) error {
	req, err := parseExecPivotArgs(args)
	if err != nil {
		return err
	}
	if req.rootDir != "" {
		if err := pivotExecRoot(req.rootDir); err != nil {
			return err
		}
	}
	if req.workDir != "" {
		if err := os.Chdir(req.workDir); err != nil {
			return fmt.Errorf("chdir %s: %w", req.workDir, err)
		}
	}
	if req.uid != "" || req.gid != "" || req.groups != "" {
		if err := applyExecCredential(req.uid, req.gid, req.groups); err != nil {
			return err
		}
	}
	argv0, err := lookPathForExec(req.argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(argv0, req.argv, os.Environ())
}

type execPivotRequest struct {
	rootDir string
	workDir string
	uid     string
	gid     string
	groups  string
	argv    []string
}

func parseExecPivotArgs(args []string) (execPivotRequest, error) {
	if len(args) < 7 {
		return execPivotRequest{}, fmt.Errorf("invalid exec pivot argument count")
	}
	if args[5] != "--" {
		return execPivotRequest{}, fmt.Errorf("invalid exec pivot separator")
	}
	argv := args[6:]
	if len(argv) == 0 {
		return execPivotRequest{}, fmt.Errorf("missing command")
	}
	return execPivotRequest{
		rootDir: args[0],
		workDir: args[1],
		uid:     args[2],
		gid:     args[3],
		groups:  args[4],
		argv:    append([]string(nil), argv...),
	}, nil
}

func pivotExecRoot(rootDir string) error {
	return pivotExecRootWithOps(rootDir, execPivotRootOps{
		mount:    syscall.Mount,
		mkdirAll: os.MkdirAll,
		pivot:    syscall.PivotRoot,
		chdir:    os.Chdir,
		unmount:  syscall.Unmount,
		remove:   os.Remove,
	})
}

type execPivotRootOps struct {
	mount    func(source, target, fstype string, flags uintptr, data string) error
	mkdirAll func(path string, perm os.FileMode) error
	pivot    func(newroot, putold string) error
	chdir    func(dir string) error
	unmount  func(target string, flags int) error
	remove   func(name string) error
}

func pivotExecRootWithOps(rootDir string, ops execPivotRootOps) error {
	if rootDir == "" {
		return fmt.Errorf("missing root_dir")
	}
	if err := ops.mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private: %w", err)
	}
	putOld := filepath.Join(rootDir, ".ccx3-old-root")
	if err := ops.mkdirAll(putOld, 0o700); err != nil {
		return fmt.Errorf("mkdir put_old: %w", err)
	}
	if err := ops.pivot(rootDir, putOld); err != nil {
		return fmt.Errorf("pivot_root %s: %w", rootDir, err)
	}
	if err := ops.chdir("/"); err != nil {
		return fmt.Errorf("chdir / after pivot_root: %w", err)
	}
	if err := ops.unmount("/.ccx3-old-root", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := ops.remove("/.ccx3-old-root"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old root: %w", err)
	}
	return nil
}

func applyExecCredential(uidText, gidText, groupsText string) error {
	return applyExecCredentialWithOps(uidText, gidText, groupsText, execCredentialOps{
		setgroups: syscall.Setgroups,
		setgid:    syscall.Setgid,
		setuid:    syscall.Setuid,
	})
}

type execCredentialOps struct {
	setgroups func([]int) error
	setgid    func(int) error
	setuid    func(int) error
}

func applyExecCredentialWithOps(uidText, gidText, groupsText string, ops execCredentialOps) error {
	uid, err := parseUint32(uidText)
	if err != nil {
		return fmt.Errorf("invalid uid %q: %w", uidText, err)
	}
	gid, err := parseUint32(gidText)
	if err != nil {
		return fmt.Errorf("invalid gid %q: %w", gidText, err)
	}
	var groups []int
	if groupsText != "" {
		for _, part := range strings.Split(groupsText, ",") {
			group, err := parseUint32(part)
			if err != nil {
				return fmt.Errorf("invalid group %q: %w", part, err)
			}
			groups = append(groups, int(group))
		}
	}
	if err := ops.setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := ops.setgid(int(gid)); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := ops.setuid(int(uid)); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	return nil
}

func parseUint32(value string) (uint32, error) {
	n := uint64(0)
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		n = n*10 + uint64(ch-'0')
		if n > uint64(^uint32(0)) {
			return 0, fmt.Errorf("out of range")
		}
	}
	return uint32(n), nil
}

func lookPathForExec(file string) (string, error) {
	return lookPathForExecEnv(file, os.Environ())
}

func lookPathForExecEnv(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	pathEnv := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			pathEnv = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable file not found in PATH: %s", file)
}

func writeStage(start time.Time, stage string) {
	line := fmt.Sprintf("ccx3-init: +%dms %s", time.Since(start).Milliseconds(), stage)
	writeKernel(line)
	writeConsole(line + "\n")
}

func configureClock(unixTime int64) error {
	if unixTime <= 0 {
		return nil
	}
	tv := unix.NsecToTimeval(unixTime * int64(time.Second))
	if err := setTimeOfDay(&tv); err != nil {
		return fmt.Errorf("set guest clock: %w", err)
	}
	return nil
}

func mountConfiguredRootFS(cfg config) error {
	if strings.TrimSpace(cfg.RootFSImagePath) != "" {
		return mountImageRootFS(cfg.RootFSImagePath, cfg.RootFSImageType, cfg.EmulatorTag, cfg.DisableCgroupMount)
	}
	return mountVirtioRootFS(cfg.RootFSTag, cfg.EmulatorTag, cfg.DisableCgroupMount)
}

func mountVirtioRootFS(tag, emulatorTag string, disableCgroup bool) error {
	if err := os.MkdirAll("/mnt", 0o755); err != nil {
		return fmt.Errorf("mkdir /mnt: %w", err)
	}
	if err := syscall.Mount(tag, "/mnt", "virtiofs", 0, ""); err != nil {
		return fmt.Errorf("mount virtiofs %s: %w", tag, err)
	}
	return finishRootFS("/mnt", emulatorTag, disableCgroup)
}

func mountImageRootFS(imagePath, imageType, emulatorTag string, disableCgroup bool) error {
	imageType = strings.TrimSpace(imageType)
	if imageType == "" {
		imageType = "ext4"
	}
	if err := os.MkdirAll("/mnt", 0o755); err != nil {
		return fmt.Errorf("mkdir /mnt: %w", err)
	}
	if err := attachLoop("/dev/loop0", imagePath); err != nil {
		return err
	}
	if err := syscall.Mount("/dev/loop0", "/mnt", imageType, 0, ""); err != nil {
		_ = clearLoop("/dev/loop0")
		return fmt.Errorf("mount %s rootfs image %s: %w", imageType, imagePath, err)
	}
	return finishRootFS("/mnt", emulatorTag, disableCgroup)
}

func attachLoop(loopPath, imagePath string) error {
	image, err := syscall.Open(imagePath, syscall.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open rootfs image %s: %w", imagePath, err)
	}
	defer syscall.Close(image)
	loop, err := syscall.Open(loopPath, syscall.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open loop device %s: %w", loopPath, err)
	}
	defer syscall.Close(loop)
	if err := unix.IoctlSetInt(loop, unix.LOOP_SET_FD, image); err != nil {
		return fmt.Errorf("attach %s to %s: %w", imagePath, loopPath, err)
	}
	return nil
}

func clearLoop(loopPath string) error {
	loop, err := syscall.Open(loopPath, syscall.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(loop)
	return unix.IoctlSetInt(loop, unix.LOOP_CLR_FD, 0)
}

func finishRootFS(newRoot, emulatorTag string, disableCgroup bool) error {
	if err := switchRootFS(newRoot); err != nil {
		return err
	}

	for _, dir := range []string{"/proc", "/sys", "/dev", "/tmp", "/run"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	_ = syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	if !disableCgroup {
		mountCgroupFS("/sys/fs/cgroup")
	}
	configureMemoryOvercommit("/proc")
	_ = syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	_ = syscall.Mount("tmpfs", "/tmp", "tmpfs", 0, "mode=1777")
	_ = syscall.Mount("tmpfs", "/run", "tmpfs", 0, "mode=755")
	for _, dir := range []string{"/dev/pts", "/dev/shm"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	_ = syscall.Mount("devpts", "/dev/pts", "devpts", 0, "gid=5,mode=620,ptmxmode=666")
	_ = syscall.Mount("tmpfs", "/dev/shm", "tmpfs", 0, "mode=1777")
	configureDevicePermissions("/dev")
	if emulatorTag != "" {
		if err := os.MkdirAll("/run/ccx3", 0o755); err != nil {
			return fmt.Errorf("mkdir /run/ccx3: %w", err)
		}
		if err := syscall.Mount(emulatorTag, "/run/ccx3", "virtiofs", 0, ""); err != nil {
			return fmt.Errorf("mount emulator virtiofs %s: %w", emulatorTag, err)
		}
	}
	configureDeviceLinks()
	return nil
}

func switchRootFS(newRoot string) error {
	if err := os.Chdir(newRoot); err != nil {
		return fmt.Errorf("chdir %s before switch_root: %w", newRoot, err)
	}
	// pivot_root cannot move away from the initramfs rootfs. Use the
	// switch_root pattern so later exec roots are not nested under a chroot,
	// which would prevent user-namespace sandboxes such as bubblewrap.
	for _, mount := range []string{"/sys/fs/cgroup", "/sys", "/proc"} {
		_ = syscall.Unmount(mount, syscall.MNT_DETACH)
	}
	if err := emptyRootFS(newRoot); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move root %s to /: %w", newRoot, err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot after switch_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir / after switch_root: %w", err)
	}
	return nil
}

func emptyRootFS(keep string) error {
	keep = "/" + strings.Trim(strings.TrimSpace(keep), "/")
	entries, err := os.ReadDir("/")
	if err != nil {
		return fmt.Errorf("read old root: %w", err)
	}
	for _, entry := range entries {
		name := "/" + entry.Name()
		if name == keep {
			continue
		}
		if err := removeRootFSEntry(name); err != nil {
			return err
		}
	}
	return nil
}

func triggerSnapshotMMIO(base uint64) error {
	if base == 0 {
		return nil
	}
	const snapshotMagic = 0x43535833534e4150
	pageSize := unix.Getpagesize()
	pageBase := base & ^uint64(pageSize-1)
	pageOff := int(base - pageBase)
	if err := ensureDeviceNode("/dev/mem", unix.S_IFCHR|0o600, int(unix.Mkdev(1, 1))); err != nil {
		writeConsole("__CCX3_SNAPSHOT__\n")
		return fmt.Errorf("create /dev/mem: %w", err)
	}
	mem, err := os.OpenFile("/dev/mem", os.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		writeConsole("__CCX3_SNAPSHOT__\n")
		return fmt.Errorf("open /dev/mem: %w", err)
	}
	defer mem.Close()
	mapped, err := unix.Mmap(int(mem.Fd()), int64(pageBase), pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		writeConsole("__CCX3_SNAPSHOT__\n")
		return fmt.Errorf("map snapshot MMIO: %w", err)
	}
	defer unix.Munmap(mapped)
	if pageOff+8 > len(mapped) {
		return fmt.Errorf("snapshot mmio offset %#x outside mapped page", pageOff)
	}
	trigger := (*uint64)(unsafe.Pointer(&mapped[pageOff]))
	deadline := time.Now().Add(30 * time.Second)
	for atomic.LoadUint64(trigger) != snapshotMagic {
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for snapshot readiness: timed out")
		}
		time.Sleep(time.Millisecond)
	}
	atomic.StoreUint64(trigger, snapshotMagic)
	writeConsole("__CCX3_SNAPSHOT__\n")
	return nil
}

func ensureDeviceNode(path string, mode uint32, dev int) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return unix.Mknod(path, mode, dev)
}

func seedEntropyFromHostRNG(size int) error {
	if size <= 0 {
		return nil
	}
	seed := make([]byte, size)
	rng, err := os.Open("/dev/hwrng")
	if err != nil {
		return fmt.Errorf("open /dev/hwrng: %w", err)
	}
	if _, err := io.ReadFull(rng, seed); err != nil {
		_ = rng.Close()
		return fmt.Errorf("read /dev/hwrng: %w", err)
	}
	if err := rng.Close(); err != nil {
		return fmt.Errorf("close /dev/hwrng: %w", err)
	}
	urandom, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/urandom: %w", err)
	}
	if _, err := urandom.Write(seed); err != nil {
		_ = urandom.Close()
		return fmt.Errorf("write /dev/urandom: %w", err)
	}
	if err := urandom.Close(); err != nil {
		return fmt.Errorf("close /dev/urandom: %w", err)
	}
	return nil
}

func removeRootFSEntry(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat old root entry %s: %w", name, err)
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		entries, err := os.ReadDir(name)
		if err != nil {
			return fmt.Errorf("read old root dir %s: %w", name, err)
		}
		for _, entry := range entries {
			if err := removeRootFSEntry(filepath.Join(name, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old root entry %s: %w", name, err)
	}
	return nil
}

func mountCgroupFS(path string) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		writeKernel("ccx3-init: mkdir cgroup mountpoint: " + err.Error())
		return
	}
	if err := syscall.Mount("none", path, "cgroup2", 0, ""); err == nil || errors.Is(err, syscall.EBUSY) {
		return
	}
	// Older or custom kernels may only expose cgroup v1. Mounting cgroup2 is
	// best-effort so ordinary containers continue to boot on those kernels.
	if err := syscall.Mount("cgroup", path, "cgroup", 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
		writeKernel("ccx3-init: mount cgroup filesystem: " + err.Error())
	}
}

func configureRuntimeFilesystem() error {
	if err := mountRuntimeTmpFS("/var/tmp", "mode=1777"); err != nil {
		return err
	}
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{"/run/lock", 0o1777},
		{"/run/user", 0o755},
		{"/run/sshd", 0o755},
		{"/var/log", 0o755},
		{"/var/cache", 0o755},
		{"/var/lib/dbus", 0o755},
	} {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir.path, err)
		}
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", dir.path, err)
		}
	}
	if err := ensureMachineID(); err != nil {
		return err
	}
	if err := ensureLocaltime(); err != nil {
		return err
	}
	return nil
}

func mountRuntimeTmpFS(path, options string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	if err := syscall.Mount("tmpfs", path, "tmpfs", 0, options); err != nil && !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("mount tmpfs on %s: %w", path, err)
	}
	return nil
}

func configureDeviceLinks() {
	for target, link := range map[string]string{
		"/proc/self/fd":   "/dev/fd",
		"/proc/self/fd/0": "/dev/stdin",
		"/proc/self/fd/1": "/dev/stdout",
		"/proc/self/fd/2": "/dev/stderr",
	} {
		if err := ensureSymlink(target, link); err != nil {
			writeKernel("ccx3-init: link " + link + ": " + err.Error())
		}
	}
}

// configureDevicePermissions applies the standard permissions normally set
// by udev. Minimal guest targets may deliberately start without udev, but
// should still expose devices intended for unprivileged use.
func configureDevicePermissions(devRoot string) {
	for name, mode := range map[string]os.FileMode{
		"fuse": 0o666,
	} {
		path := filepath.Join(devRoot, name)
		if err := os.Chmod(path, mode); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeKernel("ccx3-init: chmod " + path + ": " + err.Error())
		}
	}
}

func ensureLocaltime() error {
	if pathExists("/etc/localtime") {
		return nil
	}
	if !pathExists("/usr/share/zoneinfo/Etc/UTC") {
		return nil
	}
	return ensureSymlink("/usr/share/zoneinfo/Etc/UTC", "/etc/localtime")
}

func ensureSymlink(target, link string) error {
	if existing, err := os.Readlink(link); err == nil {
		if existing == target {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	} else if os.IsNotExist(err) {
		// Create it below.
	} else if info, statErr := os.Lstat(link); statErr == nil {
		if info.IsDir() {
			return fmt.Errorf("%s is a directory", link)
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

func ensureMachineID() error {
	id, err := readMachineID("/etc/machine-id")
	if err != nil {
		return err
	}
	if id == "" {
		id, err = newMachineID()
		if err != nil {
			return err
		}
		if err := os.WriteFile("/etc/machine-id", []byte(id+"\n"), 0o444); err != nil {
			return fmt.Errorf("write /etc/machine-id: %w", err)
		}
	}
	dbusPath := "/var/lib/dbus/machine-id"
	if existing, err := readMachineID(dbusPath); err != nil {
		return err
	} else if existing == id {
		return nil
	}
	_ = os.Remove(dbusPath)
	if err := os.Symlink("/etc/machine-id", dbusPath); err == nil {
		return nil
	}
	if err := os.WriteFile(dbusPath, []byte(id+"\n"), 0o444); err != nil {
		return fmt.Errorf("write %s: %w", dbusPath, err)
	}
	return nil
}

func readMachineID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" || id == "uninitialized" {
		return "", nil
	}
	return id, nil
}

func newMachineID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate machine-id: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return hex.EncodeToString(buf[:]), nil
}

func precopyAMD64Root() error {
	for _, path := range []string{
		"/run/ccx3/qemu-x86_64",
		"/bin/busybox",
		"/lib/ld-musl-x86_64.so.1",
		"/lib/libc.musl-x86_64.so.1",
	} {
		if err := copyPath(path, filepath.Join("/run/ccx3-precopy", path)); err != nil {
			return err
		}
	}
	return nil
}

func configureNetwork(cfg *network) error {
	if cfg == nil {
		return nil
	}
	iface := strings.TrimSpace(cfg.Interface)
	if iface == "" {
		iface = "eth0"
	}
	address := strings.TrimSpace(cfg.Address)
	if address == "" {
		address = "10.42.0.2/24"
	}
	gateway := strings.TrimSpace(cfg.Gateway)
	if gateway == "" {
		gateway = "10.42.0.1"
	}
	dns := strings.TrimSpace(cfg.DNS)
	if dns == "" {
		dns = gateway
	}
	if err := configureNetworkLink("lo"); err != nil {
		writeKernel("ccx3-init: configure lo: " + err.Error())
	}
	if err := configureNetworkLink(iface); err != nil {
		return err
	}
	if err := configureNetworkAddress(iface, address); err != nil {
		return err
	}
	if err := configureNetworkDefaultRoute(iface, gateway); err != nil {
		return err
	}
	if err := writeResolverConfig(dns); err != nil {
		return err
	}
	return nil
}

func writeResolverConfig(dns string) error {
	data := []byte("nameserver " + dns + "\n")
	if err := writeFileReplacingMissingSymlink("/etc/resolv.conf", data, 0o644); err != nil {
		return fmt.Errorf("write /etc/resolv.conf: %w", err)
	}
	return nil
}

func writeFileReplacingMissingSymlink(name string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(name, data, perm); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Lstat(name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(name)
		return os.WriteFile(name, data, perm)
	}
	return os.WriteFile(name, data, perm)
}

func configureNetworkLink(name string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("find interface %s: %w", name, err)
	}
	msg := unix.IfInfomsg{
		Family: unix.AF_UNSPEC,
		Index:  int32(iface.Index),
		Flags:  unix.IFF_UP,
		Change: unix.IFF_UP,
	}
	if err := netlinkRequest(unix.RTM_NEWLINK, 0, structBytes(&msg), false); err != nil {
		return fmt.Errorf("set link %s up: %w", name, err)
	}
	return nil
}

func configureNetworkAddress(ifaceName, address string) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("find interface %s: %w", ifaceName, err)
	}
	ip, ipNet, err := net.ParseCIDR(address)
	if err != nil {
		return fmt.Errorf("parse address %s: %w", address, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("address %s is not IPv4", address)
	}
	ones, _ := ipNet.Mask.Size()
	msg := unix.IfAddrmsg{
		Family:    unix.AF_INET,
		Prefixlen: uint8(ones),
		Scope:     unix.RT_SCOPE_UNIVERSE,
		Index:     uint32(iface.Index),
	}
	payload := structBytes(&msg)
	payload = appendRtAttr(payload, unix.IFA_LOCAL, ip4)
	payload = appendRtAttr(payload, unix.IFA_ADDRESS, ip4)
	if err := netlinkRequest(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, payload, true); err != nil {
		return fmt.Errorf("add address %s to %s: %w", address, ifaceName, err)
	}
	return nil
}

func configureNetworkDefaultRoute(ifaceName, gateway string) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("find interface %s: %w", ifaceName, err)
	}
	gw := net.ParseIP(gateway).To4()
	if gw == nil {
		return fmt.Errorf("gateway %s is not IPv4", gateway)
	}
	msg := unix.RtMsg{
		Family:   unix.AF_INET,
		Table:    unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_STATIC,
		Scope:    unix.RT_SCOPE_UNIVERSE,
		Type:     unix.RTN_UNICAST,
	}
	payload := structBytes(&msg)
	payload = appendRtAttr(payload, unix.RTA_GATEWAY, gw)
	oif := uint32(iface.Index)
	payload = appendRtAttr(payload, unix.RTA_OIF, structBytes(&oif))
	if err := netlinkRequest(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, payload, true); err != nil {
		return fmt.Errorf("add default route via %s dev %s: %w", gateway, ifaceName, err)
	}
	return nil
}

func netlinkRequest(msgType uint16, flags uint16, payload []byte, ignoreExists bool) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	seq := uint32(time.Now().UnixNano())
	hdr := unix.NlMsghdr{
		Len:   uint32(unix.NLMSG_HDRLEN + len(payload)),
		Type:  msgType,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK | flags,
		Seq:   seq,
	}
	msg := append(structBytes(&hdr), payload...)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	return readNetlinkAck(fd, seq, ignoreExists)
}

func readNetlinkAck(fd int, seq uint32, ignoreExists bool) error {
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		for off := 0; off+unix.NLMSG_HDRLEN <= n; {
			hdr := (*unix.NlMsghdr)(unsafe.Pointer(&buf[off]))
			if hdr.Len < unix.NLMSG_HDRLEN || off+int(hdr.Len) > n {
				return fmt.Errorf("short netlink response")
			}
			next := off + nlmsgAlign(int(hdr.Len))
			if hdr.Seq != seq {
				off = next
				continue
			}
			switch hdr.Type {
			case unix.NLMSG_ERROR:
				if int(hdr.Len) < unix.NLMSG_HDRLEN+int(unsafe.Sizeof(unix.NlMsgerr{})) {
					return fmt.Errorf("short netlink error response")
				}
				msgErr := (*unix.NlMsgerr)(unsafe.Pointer(&buf[off+unix.NLMSG_HDRLEN]))
				if msgErr.Error == 0 {
					return nil
				}
				errno := syscall.Errno(-msgErr.Error)
				if ignoreExists && errno == syscall.EEXIST {
					return nil
				}
				return errno
			case unix.NLMSG_DONE:
				return nil
			}
			off = next
		}
	}
}

func appendRtAttr(buf []byte, attrType uint16, data []byte) []byte {
	attr := unix.RtAttr{
		Len:  uint16(unsafe.Sizeof(unix.RtAttr{}) + uintptr(len(data))),
		Type: attrType,
	}
	buf = append(buf, structBytes(&attr)...)
	buf = append(buf, data...)
	for len(buf)%unix.NLMSG_ALIGNTO != 0 {
		buf = append(buf, 0)
	}
	return buf
}

func nlmsgAlign(n int) int {
	return (n + unix.NLMSG_ALIGNTO - 1) & ^(unix.NLMSG_ALIGNTO - 1)
}

func structBytes[T any](value *T) []byte {
	size := int(unsafe.Sizeof(*value))
	data := unsafe.Slice((*byte)(unsafe.Pointer(value)), size)
	out := make([]byte, size)
	copy(out, data)
	return out
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return nil
}

func copyPath(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", src, err)
		}
		_ = os.Remove(dst)
		if err := os.Symlink(target, dst); err != nil {
			return fmt.Errorf("symlink %s: %w", dst, err)
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

func configureMemoryOvercommit(procRoot string) {
	path := filepath.Join(procRoot, "sys/vm/overcommit_memory")
	_ = os.WriteFile(path, []byte("1\n"), 0o644)
}

func configureBinfmt() (bool, error) {
	if _, err := os.Stat(guestQEMUPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := copyPath(guestQEMUPath, guestQEMUBinfmtPath); err != nil {
		return false, fmt.Errorf("copy qemu-x86_64 for binfmt: %w", err)
	}
	if err := os.Chmod(guestQEMUBinfmtPath, 0o755); err != nil {
		return false, fmt.Errorf("chmod %s: %w", guestQEMUBinfmtPath, err)
	}
	if err := os.MkdirAll("/proc/sys/fs/binfmt_misc", 0o755); err != nil {
		return false, fmt.Errorf("mkdir binfmt_misc: %w", err)
	}
	if err := syscall.Mount("binfmt_misc", "/proc/sys/fs/binfmt_misc", "binfmt_misc", 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
		if errors.Is(err, syscall.ENODEV) {
			writeKernel("ccx3-init: binfmt_misc unavailable: " + err.Error())
			return false, nil
		}
		return false, fmt.Errorf("mount binfmt_misc: %w", err)
	}
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/qemu-x86_64"); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat qemu-x86_64 registration: %w", err)
	}
	const qemuX8664Registration = ":qemu-x86_64:M::\\x7fELF\\x02\\x01\\x01\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x02\\x00\\x3e\\x00:\\xff\\xff\\xff\\xff\\xff\\xfe\\xfe\\x00\\xff\\xff\\xff\\xff\\xff\\xff\\xff\\xff\\xfe\\xff\\xff\\xff:" + guestQEMUBinfmtPath + ":F"
	if err := os.WriteFile("/proc/sys/fs/binfmt_misc/register", []byte(qemuX8664Registration), 0o644); err != nil {
		return false, fmt.Errorf("register qemu-x86_64: %w", err)
	}
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/qemu-x86_64"); err != nil {
		return false, fmt.Errorf("verify qemu-x86_64 registration: %w", err)
	}
	return true, nil
}

func writeString(fd int, value string) {
	for len(value) > 0 {
		n, err := syscall.Write(fd, []byte(value))
		if err != nil || n <= 0 {
			return
		}
		value = value[n:]
	}
}

func writeConsole(value string) {
	writeString(consoleFD, value)
}

func writeProtocolLine(value string) {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	writeConsole(strings.TrimRight(value, "\n") + "\n")
}

func writeProtocolLineTo(w io.Writer, value string) {
	guestagent.WriteProtocolLine(w, value)
}

func writeExecStderr(cfg config, control io.Writer, id, value string) {
	reporterForConfig(cfg, control, id, time.Time{}).Stderr([]byte(value))
}

func writeExecStdoutBytes(cfg config, control io.Writer, id string, data []byte) {
	reporterForConfig(cfg, control, id, time.Time{}).Stdout(data)
}

func writeExecControlBytes(cfg config, control io.Writer, id string, data []byte) {
	reporterForConfig(cfg, control, id, time.Time{}).ControlBytes(data)
}

func writeExecTiming(control io.Writer, id, phase string, start time.Time) {
	guestagent.NewExecReporter(guestagent.Protocol{TimingMarkerPrefix: guestagent.TimingMarkerPrefix}, control, id, start).Timing(phase)
}

func managedExecGuestReadyTimingPhases(waitReady func() error) (begin string, done string, enabled bool) {
	if waitReady == nil {
		return "", "", false
	}
	return managedExecTimingGuestReadyBegin, managedExecTimingGuestReadyDone, true
}

func managedExecFirstOutputTimingPhase(stderr bool) string {
	if stderr {
		return managedExecTimingFirstStderr
	}
	return managedExecTimingFirstStdout
}

func reporterForConfig(cfg config, control io.Writer, id string, start time.Time) guestagent.ExecReporter {
	return guestagent.NewExecReporter(protocolForConfig(cfg), control, id, start)
}

func protocolForConfig(cfg config) guestagent.Protocol {
	return guestagent.Protocol{
		BeginMarkerPrefix:   cfg.BeginMarker,
		OutputMarkerPrefix:  cfg.OutputMarkerPref,
		ErrorMarkerPrefix:   cfg.ErrorMarkerPref,
		ControlMarkerPrefix: cfg.ControlMarkerPref,
		UsageMarkerPrefix:   cfg.UsageMarkerPref,
		ExitMarkerPrefix:    cfg.ExitMarkerPrefix,
		TimingMarkerPrefix:  guestagent.TimingMarkerPrefix,
	}
}

func appendFileContents(buf *strings.Builder, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		buf.WriteString(path)
		buf.WriteString(": ")
		buf.WriteString(err.Error())
		buf.WriteString("\n")
		return
	}
	buf.WriteString(path)
	buf.WriteString(":\n")
	buf.Write(data)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		buf.WriteString("\n")
	}
}

func appendStat(buf *strings.Builder, label, path string) {
	info, err := os.Stat(path)
	if err != nil {
		buf.WriteString(label)
		buf.WriteString(": ")
		buf.WriteString(path)
		buf.WriteString(": ")
		buf.WriteString(err.Error())
		buf.WriteString("\n")
		return
	}
	buf.WriteString(label)
	buf.WriteString(": ")
	buf.WriteString(path)
	buf.WriteString(" mode=")
	buf.WriteString(fmt.Sprintf("%#o", info.Mode()&0o777))
	if info.IsDir() {
		buf.WriteString(" dir=true")
	}
	buf.WriteString("\n")
}

func managedExecStartFailure(err error, argv []string, rootDir, workDir string) (int, string) {
	command := "command"
	if len(argv) > 0 && strings.TrimSpace(argv[0]) != "" {
		command = argv[0]
	}
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return 127, command + ": executable not found\n"
	case errors.Is(err, os.ErrPermission):
		return 126, command + ": permission denied\n"
	default:
		return 126, "ccx3-init: exec error: " + err.Error() + "\n" + collectExecDiagnostics(rootDir, argv, workDir)
	}
}

func collectExecDiagnostics(rootDir string, argv []string, workDir string) string {
	var buf strings.Builder
	if rootDir != "" {
		appendStat(&buf, "root_dir", rootDir)
	}
	if len(argv) != 0 {
		appendStat(&buf, "argv0", argv[0])
		if rootDir != "" && strings.HasPrefix(argv[0], "/") {
			appendStat(&buf, "argv0@root", rootDir+argv[0])
		}
	}
	if workDir != "" {
		appendStat(&buf, "workdir", workDir)
		if rootDir != "" && strings.HasPrefix(workDir, "/") {
			appendStat(&buf, "workdir@root", rootDir+workDir)
		}
	}
	appendStat(&buf, "/run", "/run")
	appendStat(&buf, "/run/ccx3", "/run/ccx3")
	appendStat(&buf, guestQEMUBinfmtPath, guestQEMUBinfmtPath)
	if info, err := os.Stat(guestQEMUPath); err != nil {
		buf.WriteString(guestQEMUPath)
		buf.WriteString(": ")
		buf.WriteString(err.Error())
		buf.WriteString("\n")
	} else {
		buf.WriteString(guestQEMUPath)
		buf.WriteString(" mode: ")
		buf.WriteString(fmt.Sprintf("%#o", info.Mode()&0o777))
		buf.WriteString("\n")
	}
	appendFileContents(&buf, "/proc/sys/fs/binfmt_misc/status")
	appendFileContents(&buf, "/proc/sys/fs/binfmt_misc/qemu-x86_64")
	return buf.String()
}

func writeKernel(value string) {
	value = strings.TrimRight(value, "\n")
	if value == "" {
		return
	}
	if kmsgFD >= 0 {
		writeString(kmsgFD, "<6>"+value+"\n")
		return
	}
	writeConsole(value + "\n")
}

func openKernelLog() {
	if kmsgFD >= 0 {
		return
	}
	if fd, err := syscall.Open("/dev/kmsg", syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		kmsgFD = fd
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [32]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + (v % 10))
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func openPTY(cols, rows int) (*os.File, *os.File, error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(masterFD), "ptmx")
	if master == nil {
		_ = unix.Close(masterFD)
		return nil, nil, fmt.Errorf("open /dev/ptmx: no file handle")
	}

	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("unlock ptmx: %w", err)
	}
	ptn, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("query pty number: %w", err)
	}
	if cols > 0 && rows > 0 {
		if err := unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, &unix.Winsize{
			Col: uint16(cols),
			Row: uint16(rows),
		}); err != nil {
			_ = master.Close()
			return nil, nil, fmt.Errorf("set initial winsize: %w", err)
		}
	}
	slave, err := os.OpenFile("/dev/pts/"+itoa(ptn), os.O_RDWR, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("open slave pty: %w", err)
	}
	return master, slave, nil
}

func loadModules(modules []string) error {
	for _, path := range modules {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read module %s: %w", path, err)
		}
		if err := loadModuleData(path, data); err != nil {
			return err
		}
	}
	return nil
}

type modulePayload struct {
	path string
	data []byte
}

func readModulePayloads(modules []string) ([]modulePayload, error) {
	payloads := make([]modulePayload, 0, len(modules))
	for _, path := range modules {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read module %s: %w", path, err)
		}
		payloads = append(payloads, modulePayload{path: path, data: data})
	}
	return payloads, nil
}

func loadModuleData(path string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("module %s is empty", path)
	}
	params, err := syscall.BytePtrFromString("")
	if err != nil {
		return fmt.Errorf("init module params: %w", err)
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_INIT_MODULE, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(params)))
	if errno != 0 {
		return fmt.Errorf("load module %s: errno=%d", path, errno)
	}
	return nil
}

func splitSnapshotDeferredModules(modules []string) ([]string, []string) {
	var early []string
	var deferred []string
	for _, path := range modules {
		if snapshotDeferredModule(path) {
			deferred = append(deferred, path)
			continue
		}
		early = append(early, path)
	}
	return early, deferred
}

func snapshotDeferredModule(path string) bool {
	name := filepath.Base(path)
	return name == "vsock.ko" ||
		strings.HasPrefix(name, "vsock.ko.") ||
		strings.HasPrefix(name, "vmw_vsock_") ||
		strings.HasPrefix(name, "virtio_vsock")
}

func execCommand(cfg config) error {
	if info, err := os.Stat(cfg.Command[0]); err != nil {
		writeKernel("ccx3-init: stat failed for " + cfg.Command[0] + ": " + err.Error())
	} else {
		writeKernel("ccx3-init: stat mode for " + cfg.Command[0] + " is " + fmt.Sprintf("%#o", info.Mode()&0o777))
	}

	exitCode, usage, err := execCommandGo(cfg.Command, cfg.Env, cfg.WorkDir, cfg.User)
	if err != nil {
		return fmt.Errorf("run %s: %w", cfg.Command[0], err)
	}
	if cfg.UsageMarkerPref != "" && usage != nil {
		usageMarker := cfg.UsageMarkerPref + guestagent.EncodeExecUsage(usage)
		writeKernel(usageMarker)
		writeProtocolLine(usageMarker)
	}
	if cfg.ExitMarkerPrefix != "" {
		exitMarker := cfg.ExitMarkerPrefix + itoa(exitCode)
		writeKernel(exitMarker)
		writeProtocolLine(exitMarker)
	}
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	for {
		syscall.Pause()
	}
}

func execCommandGo(argv []string, env []string, workDir string, user string) (int, *guestagent.ExecUsage, error) {
	cred, err := guestagent.CredentialForUser(user)
	if err != nil {
		return 0, nil, err
	}
	if cred != nil {
		credentialSetupMu.Lock()
		err := guestagent.EnsureCredentialUser("", cred)
		credentialSetupMu.Unlock()
		if err != nil {
			return 0, nil, err
		}
		env = guestagent.EnvironmentForCredential("", env, cred)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	start := time.Now()
	cmd.Env = env
	if workDir != "" {
		cmd.Dir = workDir
	}
	console := os.NewFile(uintptr(consoleFD), "/dev/console")
	cmd.Stdin = console
	cmd.Stdout = console
	cmd.Stderr = console
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}

	err = cmd.Run()
	usage := guestagent.UsageFromProcessState(cmd.ProcessState, time.Since(start))
	if err == nil {
		return 0, usage, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return guestagent.ProcessExitCode(cmd.ProcessState, exitErr.ExitCode()), usage, nil
	}
	return 0, usage, err
}

func handleInitControlRequest(cfg config, control io.Writer, active *guestagent.ActiveExecSet, req execRequest) bool {
	switch req.Kind {
	case "exec":
		return false
	case "sync":
		if req.ID == "" {
			writeKernel("ccx3-init: sync request missing id")
			return true
		}
		go runSyncRequest(cfg, control, req.ID)
	case "fs_archive":
		if req.ID == "" {
			writeKernel("ccx3-init: fs_archive request missing id")
			return true
		}
		go runFSArchiveRequest(cfg, control, req.ID, req.RootDir, req.Path, firstNonEmpty(req.User, cfg.User))
	case "fs_mkdir":
		if req.ID == "" {
			writeKernel("ccx3-init: fs_mkdir request missing id")
			return true
		}
		go runFSMkdirRequest(cfg, control, req.ID, req.RootDir, req.Path, firstNonEmpty(req.User, cfg.User))
	case "fs_write":
		if req.ID == "" {
			writeKernel("ccx3-init: fs_write request missing id")
			return true
		}
		go runFSWriteRequest(cfg, control, req.ID, req.RootDir, req.Path, firstNonEmpty(req.User, cfg.User), io.NopCloser(bytes.NewReader(req.Stdin)))
	case "fs_extract":
		if req.ID == "" {
			writeKernel("ccx3-init: fs_extract request missing id")
			return true
		}
		if len(req.Stdin) > 0 {
			go runFSExtractRequest(cfg, control, req.ID, req.RootDir, req.Path, req.Directory, req.User, req.ArchiveLimits, io.NopCloser(bytes.NewReader(req.Stdin)), func() {})
			return true
		}
		stdinR, stdinW := io.Pipe()
		managed := &managedExec{stdin: stdinW, start: time.Now()}
		active.Add(req.ID, managed)
		go runFSExtractRequest(cfg, control, req.ID, req.RootDir, req.Path, req.Directory, req.User, req.ArchiveLimits, stdinR, func() {
			_ = managed.closeStdin()
			active.Delete(req.ID)
		})
	case "stdin":
		result := guestagent.HandleActiveControl(active, guestagent.ActiveControlRequest{
			Kind:  req.Kind,
			ID:    req.ID,
			Stdin: req.Stdin,
		})
		if !result.Found {
			writeKernel("ccx3-init: stdin for unknown exec id " + req.ID)
			return true
		}
		if result.Err != nil {
			writeKernel("ccx3-init: write stdin: " + result.Err.Error())
		}
	case "stdin_close":
		result := guestagent.HandleActiveControl(active, guestagent.ActiveControlRequest{
			Kind:                      req.Kind,
			ID:                        req.ID,
			RememberPendingStdinClose: true,
		})
		if !result.Found {
			return true
		}
		if result.Err != nil {
			writeKernel("ccx3-init: close stdin: " + result.Err.Error())
		}
	case "signal":
		result := guestagent.HandleActiveControl(active, guestagent.ActiveControlRequest{
			Kind:   req.Kind,
			ID:     req.ID,
			Signal: req.Signal,
		})
		if !result.Found {
			writeKernel("ccx3-init: signal for unknown exec id " + req.ID)
			return true
		}
		if result.Err != nil {
			writeKernel("ccx3-init: signal " + req.Signal + ": " + result.Err.Error())
		}
	case "resize":
		result := guestagent.HandleActiveControl(active, guestagent.ActiveControlRequest{
			Kind: req.Kind,
			ID:   req.ID,
			Cols: req.Cols,
			Rows: req.Rows,
		})
		if !result.Found {
			return true
		}
		if result.Err != nil {
			writeKernel("ccx3-init: resize " + itoa(req.Cols) + "x" + itoa(req.Rows) + ": " + result.Err.Error())
		}
	default:
		writeKernel("ccx3-init: unsupported control kind " + req.Kind)
	}
	return true
}

func commandLoop(cfg config, control io.ReadWriter) error {
	if cfg.ControlMarkerPref == "" {
		cfg.ControlMarkerPref = defaultControlMarkerPref
	}
	reader := bufio.NewReader(control)
	active := guestagent.NewActiveExecSet()
	systemdGate := newSystemdCommandGate(cfg.InitSystem)
	startOrphanReaper()
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("read exec request: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var req execRequest
		if err := json.Unmarshal(line, &req); err != nil {
			writeKernel("ccx3-init: decode exec request: " + err.Error())
			if cfg.ExitMarkerPrefix != "" {
				writeKernel(cfg.ExitMarkerPrefix + "125")
			}
			continue
		}
		if req.Kind == "" {
			req.Kind = "exec"
		}
		inlineExec := req.Kind == "exec_inline"
		if inlineExec {
			req.Kind = "exec"
		}
		if handleInitControlRequest(cfg, control, active, req) {
			continue
		}
		if validation := validateExecRequest(req); validation.Message != "" {
			writeKernel("ccx3-init: " + validation.Message)
			if validation.ExitCode != 0 && cfg.ExitMarkerPrefix != "" {
				writeKernel(cfg.ExitMarkerPrefix + itoa(validation.ExitCode))
			}
			continue
		}

		execReq := prepareExecRequest(cfg, req)
		if inlineExec {
			runPreparedExecInline(cfg, control, execReq, systemdGate.WaitForCommand(execReq.Command))
			continue
		}
		startPreparedExec(cfg, control, active, execReq, systemdGate.WaitForCommand(execReq.Command))
	}
}

func commandNeedsSystemdReady(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	return commandTextNeedsSystemdReady(argv[0], argv[1:])
}

func commandTextNeedsSystemdReady(cmd string, args []string) bool {
	switch filepath.Base(cmd) {
	case "systemctl", "journalctl", "loginctl", "busctl", "hostnamectl", "timedatectl", "localectl", "networkctl", "resolvectl":
		return true
	case "service":
		return len(args) == 0 || args[0] != "--help"
	case "sudo", "doas", "env", "command":
		next, rest, ok := unwrapCommandPrefix(args)
		if !ok {
			return false
		}
		return commandTextNeedsSystemdReady(next, rest)
	case "sh", "bash", "dash", "zsh":
		for i, arg := range args {
			if shellArgRunsCommand(arg) && i+1 < len(args) {
				return shellScriptNeedsSystemdReady(args[i+1])
			}
		}
	}
	return false
}

func shellArgRunsCommand(arg string) bool {
	if arg == "-c" {
		return true
	}
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	return strings.Contains(arg[1:], "c")
}

func unwrapCommandPrefix(args []string) (string, []string, bool) {
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			args = args[1:]
			continue
		}
		if strings.Contains(arg, "=") && !strings.HasPrefix(arg, "-") {
			args = args[1:]
			continue
		}
		if strings.HasPrefix(arg, "-") {
			args = args[1:]
			if arg == "-u" || arg == "-g" || arg == "-C" || arg == "-S" || arg == "--user" || arg == "--group" || arg == "--chdir" || arg == "--shell" {
				if len(args) > 0 {
					args = args[1:]
				}
			}
			continue
		}
		return arg, args[1:], true
	}
	return "", nil, false
}

func shellScriptNeedsSystemdReady(script string) bool {
	fields := strings.FieldsFunc(script, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ';', '&', '|', '(', ')', '<', '>', '`', '$', '"', '\'', '\\':
			return true
		default:
			return false
		}
	})
	for _, field := range fields {
		if commandTextNeedsSystemdReady(field, nil) {
			return true
		}
	}
	return false
}

func waitForSystemdCommandReady(timeout time.Duration) error {
	if ok, err := socketExists("/run/systemd/private"); err != nil {
		return err
	} else if !ok {
		if err := waitForSystemdControlSocket(timeout); err != nil {
			return err
		}
	}
	if !systemdSystemBusExpected() {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		ok, err := socketExists("/run/dbus/system_bus_socket")
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd system bus socket did not appear within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func runSyncRequest(cfg config, control io.Writer, id string) {
	proto := protocolForConfig(cfg)
	proto.WriteBegin(control, id)
	syscall.Sync()
	proto.WriteExit(control, id, 0)
}

func runFSArchiveRequest(cfg config, control io.Writer, id, rootDir, src, user string) {
	proto := protocolForConfig(cfg)
	proto.WriteBegin(control, id)
	exitCode := 0
	if err := archivePathToControl(cfg, control, id, rootDir, src, user); err != nil {
		exitCode = 1
		writeExecStderr(cfg, control, id, "ccx3-init: fs archive: "+err.Error()+"\n")
	}
	proto.WriteExit(control, id, exitCode)
}

func archivePathToControl(cfg config, control io.Writer, id, rootDir, src, user string) error {
	if strings.TrimSpace(src) == "" {
		return fmt.Errorf("source path is required")
	}
	credential, err := guestagent.CredentialForUserInRoot(rootDir, user)
	if err != nil {
		return err
	}
	if credential != nil {
		cmd := exec.Command("/proc/self/exe", archiveMode, rootDir, src)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
		cmd.Stdout = execOutputWriter{write: func(data []byte) { writeExecStdoutBytes(cfg, control, id, data) }}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := runArchiveHelperCommand(cmd); err != nil {
			return archiveHelperError(err, stderr.String())
		}
		return nil
	}
	src = rootPath(rootDir, filepath.Clean(src))
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		err := guestagent.WritePathTar(pw, src, filepath.Base(src), info)
		_ = pw.CloseWithError(err)
	}()
	var buf [execProtocolChunkSize]byte
	for {
		n, err := pr.Read(buf[:])
		if n > 0 {
			writeExecStdoutBytes(cfg, control, id, buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

type execOutputWriter struct {
	write func([]byte)
}

func (w execOutputWriter) Write(data []byte) (int, error) {
	if len(data) > 0 && w.write != nil {
		w.write(data)
	}
	return len(data), nil
}

func runArchiveHelper(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("archive helper requires root and source paths")
	}
	src := rootPath(args[0], filepath.Clean(args[1]))
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	return guestagent.WritePathTar(os.Stdout, src, filepath.Base(src), info)
}

type extractHelperRequest struct {
	RootDir string                `json:"root_dir,omitempty"`
	Path    string                `json:"path"`
	IsDir   bool                  `json:"is_dir,omitempty"`
	Limits  *client.ArchiveLimits `json:"limits,omitempty"`
}

func runExtractHelper(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("extract helper requires one request")
	}
	var request extractHelperRequest
	if err := json.Unmarshal([]byte(args[0]), &request); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	ctx, cancel, err := guestagent.ArchiveContext(context.Background(), request.Limits)
	if err != nil {
		return err
	}
	defer cancel()
	return guestagent.ExtractTarToPathContext(ctx, os.Stdin, request.RootDir, request.Path, request.IsDir, request.Limits)
}

func archiveHelperError(runErr error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return errors.New(stderr)
	}
	return runErr
}

func runArchiveHelperCommand(cmd *exec.Cmd) error {
	managedChildren.Lock()
	err := cmd.Start()
	if err == nil {
		managedChildren.pids[cmd.Process.Pid] = struct{}{}
	}
	managedChildren.Unlock()
	if err != nil {
		return err
	}
	err = cmd.Wait()
	managedChildren.Lock()
	delete(managedChildren.pids, cmd.Process.Pid)
	managedChildren.Unlock()
	return err
}

func runFSExtractRequest(cfg config, control io.Writer, id, rootDir, dst string, dstDir bool, user string, limits *client.ArchiveLimits, stdin io.ReadCloser, cleanup func()) {
	defer cleanup()
	defer stdin.Close()
	proto := protocolForConfig(cfg)
	proto.WriteBegin(control, id)
	exitCode := 0
	ctx, cancel, err := guestagent.ArchiveContext(context.Background(), limits)
	if err == nil {
		defer cancel()
		credential, credentialErr := guestagent.CredentialForUserInRoot(rootDir, user)
		if credentialErr != nil {
			err = credentialErr
		} else if credential != nil {
			credentialSetupMu.Lock()
			userErr := guestagent.EnsureCredentialUser(rootDir, credential)
			if userErr == nil {
				userErr = guestagent.EnsureCredentialArchiveHome(rootDir, dst, credential)
			}
			credentialSetupMu.Unlock()
			if userErr != nil {
				err = userErr
			}
		}
		if err == nil && credential != nil {
			request, marshalErr := json.Marshal(extractHelperRequest{RootDir: rootDir, Path: dst, IsDir: dstDir, Limits: limits})
			if marshalErr != nil {
				err = marshalErr
			} else {
				cmd := exec.Command("/proc/self/exe", extractMode, string(request))
				cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
				cmd.Stdin = stdin
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if runErr := runArchiveHelperCommand(cmd); runErr != nil {
					err = archiveHelperError(runErr, stderr.String())
				}
			}
		}
		if err == nil && credential == nil {
			err = guestagent.ExtractTarToPathContext(ctx, stdin, rootDir, dst, dstDir, limits)
		}
	}
	if err != nil {
		exitCode = 1
		writeExecStderr(cfg, control, id, "ccx3-init: fs extract: "+err.Error()+"\n")
	}
	proto.WriteExit(control, id, exitCode)
}

func runFSMkdirRequest(cfg config, control io.Writer, id, rootDir, dir, user string) {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	proto := protocolForConfig(cfg)
	exitCode := 0
	proto.WriteBegin(control, id)
	target := rootPath(rootDir, filepath.Clean(dir))
	if err := os.MkdirAll(target, 0o755); err != nil {
		exitCode = 1
		writeExecStderr(cfg, control, id, "ccx3-init: fs mkdir: "+err.Error()+"\n")
	} else if err := guestagent.ChownPathForUser(rootDir, target, user); err != nil {
		exitCode = 1
		writeExecStderr(cfg, control, id, "ccx3-init: fs mkdir: "+err.Error()+"\n")
	}
	proto.WriteExit(control, id, exitCode)
}

func runFSWriteRequest(cfg config, control io.Writer, id, rootDir, dst, user string, stdin io.ReadCloser) {
	defer stdin.Close()
	proto := protocolForConfig(cfg)
	exitCode := 0
	proto.WriteBegin(control, id)
	if strings.TrimSpace(dst) == "" {
		writeExecStderr(cfg, control, id, "ccx3-init: fs write: destination path is required\n")
		exitCode = 1
		proto.WriteExit(control, id, exitCode)
		return
	}
	target := rootPath(rootDir, filepath.Clean(dst))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		writeExecStderr(cfg, control, id, "ccx3-init: fs write: "+err.Error()+"\n")
		exitCode = 1
	} else if err := writeStreamToPath(stdin, target, 0o644); err != nil {
		writeExecStderr(cfg, control, id, "ccx3-init: fs write: "+err.Error()+"\n")
		exitCode = 1
	} else if err := guestagent.ChownPathForUser(rootDir, target, user); err != nil {
		writeExecStderr(cfg, control, id, "ccx3-init: fs write: "+err.Error()+"\n")
		exitCode = 1
	}
	proto.WriteExit(control, id, exitCode)
}

func writeStreamToPath(r io.Reader, target string, mode os.FileMode) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, r)
	closeErr := file.Close()
	return errors.Join(copyErr, closeErr)
}

func execPivotArgs(rootDir, workDir string, cred *syscall.Credential, argv []string) []string {
	uid, gid, groups := execPivotCredentialArgs(cred)
	args := []string{rootDir, workDir, uid, gid, groups, "--"}
	return append(args, argv...)
}

func execPivotCredentialArgs(cred *syscall.Credential) (uid, gid, groups string) {
	if cred == nil {
		return "", "", ""
	}
	uid = fmt.Sprint(cred.Uid)
	gid = fmt.Sprint(cred.Gid)
	if len(cred.Groups) == 0 {
		return uid, gid, ""
	}
	parts := make([]string, 0, len(cred.Groups))
	for _, group := range cred.Groups {
		parts = append(parts, fmt.Sprint(group))
	}
	return uid, gid, strings.Join(parts, ",")
}

func managedExecCommand(argv []string, env []string, rootDir string, workDir string, cred *syscall.Credential, tty bool) (*exec.Cmd, bool) {
	useExecPivot := rootDir != "" || (tty && cred != nil)
	var cmd *exec.Cmd
	if useExecPivot {
		cmd = exec.Command("/proc/self/exe", append([]string{execPivotMode}, execPivotArgs(rootDir, workDir, cred, argv)...)...)
	} else {
		executable, err := lookPathForExecEnv(argv[0], env)
		if err != nil {
			executable = argv[0]
		}
		cmd = exec.Command(executable, argv[1:]...)
		cmd.Args[0] = argv[0]
		if workDir != "" {
			cmd.Dir = workDir
		}
	}
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	return cmd, useExecPivot
}

func configureManagedExecProcessAttrs(cmd *exec.Cmd, rootDir string, cred *syscall.Credential, useExecPivot bool) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if rootDir != "" {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWNS
		return
	}
	if cred != nil && !useExecPivot {
		cmd.SysProcAttr.Credential = cred
	}
}

func managedExecExitCode(waitErr error, state *os.ProcessState) (int, error) {
	if waitErr != nil && errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil
	}
	if state != nil {
		status, ok := state.Sys().(syscall.WaitStatus)
		if !ok {
			return 126, fmt.Errorf("process state did not include wait status")
		}
		if status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		if status.Exited() {
			return status.ExitStatus(), nil
		}
		return 126, fmt.Errorf("process did not exit normally")
	}
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 126, waitErr
}

func startManagedExecProcess(cmd *exec.Cmd, controlW **os.File) error {
	// Keep registration atomic with Start so the PID 1 orphan reaper cannot
	// consume a short-lived managed command before os.Process.Wait observes it.
	managedChildren.Lock()
	err := cmd.Start()
	if err == nil {
		managedChildren.pids[cmd.Process.Pid] = struct{}{}
	}
	managedChildren.Unlock()
	if controlW != nil && *controlW != nil {
		_ = (*controlW).Close()
		*controlW = nil
	}
	return err
}

type managedExecWaitResult struct {
	WaitErr      error
	ProcessState *os.ProcessState
	Usage        string
	ExitCode     int
	ExitErr      error
}

func waitManagedExecProcess(cmd *exec.Cmd, execStart time.Time) managedExecWaitResult {
	state, waitErr := cmd.Process.Wait()
	managedChildren.Lock()
	delete(managedChildren.pids, cmd.Process.Pid)
	managedChildren.Unlock()
	cmd.ProcessState = state
	usage := guestagent.UsageFromProcessState(state, time.Since(execStart))
	exitCode, exitErr := managedExecExitCode(waitErr, state)
	var usagePayload string
	if usage != nil {
		usagePayload = guestagent.EncodeExecUsage(usage)
	}
	return managedExecWaitResult{
		WaitErr:      waitErr,
		ProcessState: state,
		Usage:        usagePayload,
		ExitCode:     exitCode,
		ExitErr:      exitErr,
	}
}

func startOrphanReaper() {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			reapOrphanedChildren(os.Getpid())
		}
	}()
}

// reapOrphanedChildren prevents descendants left behind by canceled command
// trees from accumulating as zombies under ccx3-init. Managed direct children
// remain owned by os.Process.Wait so their structured exit status is preserved.
func reapOrphanedChildren(parentPID int) {
	managedChildren.Lock()
	defer managedChildren.Unlock()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid64, err := strconv.ParseInt(entry.Name(), 10, 32)
		if err != nil {
			continue
		}
		pid := int(pid64)
		if _, tracked := managedChildren.pids[pid]; tracked {
			continue
		}
		state, ppid, ok := procProcessState(filepath.Join("/proc", entry.Name(), "stat"))
		if !ok || state != 'Z' || ppid != parentPID {
			continue
		}
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	}
}

func procProcessState(path string) (byte, int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	// The comm field is parenthesized and may itself contain spaces. State and
	// parent PID are the first two fields after its final closing parenthesis.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := bytes.Fields(data[end+1:])
	if len(fields) < 2 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], ppid, true
}

func managedExecStreamCount(tty bool, hasStdin bool, hasControl bool) int {
	streams := 2
	if tty {
		streams = 1
		if hasStdin {
			streams++
		}
	}
	if hasControl {
		streams++
	}
	return streams
}

func managedExecDoneStreamCount(tty bool, hasStdin bool, hasControl bool) int {
	if !tty {
		hasStdin = false
	}
	return managedExecStreamCount(tty, hasStdin, hasControl)
}

func waitManagedExecStreams(done <-chan struct{}, count int) {
	for i := 0; i < count; i++ {
		<-done
	}
}

func attachManagedExecControlPipe(cmd *exec.Cmd, enabled bool) (*os.File, *os.File, error) {
	if !enabled {
		return nil, nil, nil
	}
	controlR, controlW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, controlW)
	return controlR, controlW, nil
}

func copyManagedExecReader(done chan<- struct{}, r io.Reader, closeReader func() error, onFirst func(), emit func([]byte)) {
	defer func() { done <- struct{}{} }()
	if closeReader != nil {
		defer closeReader()
	}
	var buf [execProtocolChunkSize]byte
	first := true
	for {
		n, err := r.Read(buf[:])
		if n > 0 && emit != nil {
			if first {
				if onFirst != nil {
					onFirst()
				}
				first = false
			}
			emit(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func copyManagedExecStdinToPTY(done chan<- struct{}, stdin io.ReadCloser, pty io.Writer) {
	defer func() { done <- struct{}{} }()
	defer stdin.Close()
	var buf [execProtocolChunkSize]byte
	for {
		n, err := stdin.Read(buf[:])
		if n > 0 {
			if _, writeErr := pty.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				_, _ = pty.Write([]byte{4})
			}
			return
		}
	}
}

func copyManagedExecStdinToPipe(done chan<- struct{}, stdin io.ReadCloser, childStdin io.WriteCloser) {
	defer func() { done <- struct{}{} }()
	defer stdin.Close()
	defer childStdin.Close()
	_, _ = io.Copy(childStdin, stdin)
}

func closeManagedExecStdinPipe(stdin io.ReadCloser, childStdin io.WriteCloser, done <-chan struct{}) {
	if childStdin != nil {
		_ = childStdin.Close()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if done != nil {
		<-done
	}
}

func runManagedExec(cfg config, control io.Writer, id string, argv []string, env []string, rootDir string, workDir string, user string, stdin io.ReadCloser, managed *managedExec, tty bool, controlFD bool, cols int, rows int, waitReady func() error, cleanup func()) {
	defer cleanup()
	execStart := time.Now()
	reporter := reporterForConfig(cfg, control, id, execStart)
	reporter.Timing(managedExecTimingRecv)
	reporter.Begin()
	writeKernel("ccx3-init: exec " + strings.Join(argv, " "))
	if readyBegin, readyDone, ok := managedExecGuestReadyTimingPhases(waitReady); ok {
		reporter.Timing(readyBegin)
		if err := waitReady(); err != nil {
			writeKernel("ccx3-init: guest not ready for exec: " + err.Error())
			writeExecStderr(cfg, control, id, "ccx3-init: guest not ready for exec: "+err.Error()+"\n")
			reporter.Exit(126)
			return
		}
		reporter.Timing(readyDone)
	}
	reporter.Timing(managedExecTimingStartBegin)

	signalGroup := true
	var rootMounts []string
	if rootDir != "" {
		preparedRoot, mounts, err := prepareExecRoot(rootDir)
		if err != nil {
			writeKernel("ccx3-init: prepare exec root: " + err.Error())
			writeExecStderr(cfg, control, id, "ccx3-init: prepare exec root: "+err.Error()+"\n")
			reporter.Exit(126)
			return
		}
		rootDir = preparedRoot
		rootMounts = mounts
		defer teardownExecRoot(rootMounts)
	}
	execCred, err := guestagent.CredentialForUserInRoot(rootDir, user)
	if err != nil {
		writeKernel("ccx3-init: resolve user: " + err.Error())
		writeExecStderr(cfg, control, id, "ccx3-init: resolve user: "+err.Error()+"\n")
		reporter.Exit(126)
		return
	}
	if execCred != nil {
		credentialSetupMu.Lock()
		err := guestagent.EnsureCredentialUser(rootDir, execCred)
		credentialSetupMu.Unlock()
		if err != nil {
			writeKernel("ccx3-init: ensure user: " + err.Error())
			writeExecStderr(cfg, control, id, "ccx3-init: ensure user: "+err.Error()+"\n")
			reporter.Exit(126)
			return
		}
		env = guestagent.EnvironmentForCredential(rootDir, env, execCred)
	}
	if err := guestagent.EnsureCredentialWorkDir(rootDir, workDir, execCred); err != nil {
		writeKernel("ccx3-init: ensure workdir: " + err.Error())
		writeExecStderr(cfg, control, id, "ccx3-init: ensure workdir: "+err.Error()+"\n")
		reporter.Exit(126)
		return
	}
	if err := validateManagedExecWorkDir(rootDir, workDir); err != nil {
		writeKernel("ccx3-init: validate workdir: " + err.Error())
		writeExecStderr(cfg, control, id, "ccx3-init: invalid workdir: "+err.Error()+"\n")
		reporter.Exit(126)
		return
	}
	cmd, useExecPivot := managedExecCommand(argv, env, rootDir, workDir, execCred, tty)
	var (
		done       chan struct{}
		stdoutR    io.ReadCloser
		stderrR    io.ReadCloser
		stdinW     io.WriteCloser
		stdinDone  chan struct{}
		controlR   *os.File
		controlW   *os.File
		ptyMaster  *os.File
		ptySlave   *os.File
		startError error
	)
	if controlFD {
		controlR, controlW, startError = attachManagedExecControlPipe(cmd, true)
		if startError != nil {
			writeKernel("ccx3-init: open control fd: " + startError.Error())
			writeExecStderr(cfg, control, id, "ccx3-init: open control fd: "+startError.Error()+"\n")
			reporter.Exit(126)
			return
		}
		defer func() {
			if controlR != nil {
				_ = controlR.Close()
			}
		}()
		defer func() {
			if controlW != nil {
				_ = controlW.Close()
			}
		}()
	}

	if tty {
		ptyMaster, ptySlave, startError = openPTY(cols, rows)
		if startError != nil {
			writeKernel("ccx3-init: open pty: " + startError.Error())
			reporter.Exit(126)
			return
		}
		defer func() {
			if ptyMaster != nil {
				_ = ptyMaster.Close()
			}
		}()
		defer func() {
			if ptySlave != nil {
				_ = ptySlave.Close()
			}
		}()
		managed.setPTY(ptyMaster)
		cmd.Stdin = ptySlave
		cmd.Stdout = ptySlave
		cmd.Stderr = ptySlave
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0,
		}
		var ptyEmit func([]byte)
		if cfg.OutputMarkerPref != "" {
			ptyEmit = reporter.Stdout
		}
		done = make(chan struct{}, managedExecDoneStreamCount(true, stdin != nil, controlR != nil))
		go copyManagedExecReader(done, ptyMaster, nil, func() { reporter.Timing(managedExecFirstOutputTimingPhase(false)) }, ptyEmit)
		if stdin != nil {
			go copyManagedExecStdinToPTY(done, stdin, ptyMaster)
		}
	} else {
		if stdin != nil {
			if file, ok := stdin.(*os.File); ok {
				cmd.Stdin = file
			} else {
				var err error
				stdinW, err = cmd.StdinPipe()
				if err != nil {
					closeManagedExecStdinPipe(stdin, nil, nil)
					writeKernel("ccx3-init: open stdin pipe: " + err.Error())
					writeExecStderr(cfg, control, id, "ccx3-init: open stdin pipe: "+err.Error()+"\n")
					reporter.Exit(126)
					return
				}
			}
		} else {
			devNull, err := os.Open("/dev/null")
			if err == nil {
				defer devNull.Close()
				cmd.Stdin = devNull
			}
		}

		var err error
		stdoutR, err = cmd.StdoutPipe()
		if err != nil {
			closeManagedExecStdinPipe(stdin, stdinW, nil)
			writeKernel("ccx3-init: open stdout pipe: " + err.Error())
			writeExecStderr(cfg, control, id, "ccx3-init: open stdout pipe: "+err.Error()+"\n")
			reporter.Exit(126)
			return
		}
		stderrR, err = cmd.StderrPipe()
		if err != nil {
			_ = stdoutR.Close()
			closeManagedExecStdinPipe(stdin, stdinW, nil)
			writeKernel("ccx3-init: open stderr pipe: " + err.Error())
			writeExecStderr(cfg, control, id, "ccx3-init: open stderr pipe: "+err.Error()+"\n")
			reporter.Exit(126)
			return
		}

		var stdoutEmit func([]byte)
		if cfg.OutputMarkerPref != "" {
			stdoutEmit = reporter.Stdout
		}
		done = make(chan struct{}, managedExecDoneStreamCount(false, stdin != nil, controlR != nil))
		go copyManagedExecReader(done, stdoutR, stdoutR.Close, func() { reporter.Timing(managedExecFirstOutputTimingPhase(false)) }, stdoutEmit)
		var stderrEmit func([]byte)
		if cfg.ErrorMarkerPref != "" {
			stderrEmit = func(data []byte) { writeExecStderr(cfg, control, id, string(data)) }
		}
		go copyManagedExecReader(done, stderrR, stderrR.Close, func() { reporter.Timing(managedExecFirstOutputTimingPhase(true)) }, stderrEmit)
	}
	if controlR != nil {
		go copyManagedExecReader(done, controlR, nil, nil, reporter.ControlBytes)
	}
	configureManagedExecProcessAttrs(cmd, rootDir, execCred, useExecPivot)
	preparedFamily, prepareFamilyErr := guestagent.PrepareProcessFamily(cmd, id)
	if prepareFamilyErr != nil {
		closeManagedExecStdinPipe(stdin, stdinW, stdinDone)
		writeKernel("ccx3-init: prepare command containment: " + prepareFamilyErr.Error())
		writeExecStderr(cfg, control, id, "ccx3-init: prepare command containment: "+prepareFamilyErr.Error()+"\n")
		reporter.Exit(126)
		return
	}

	reporter.Timing(managedExecTimingStartCall)
	startErr := startManagedExecProcess(cmd, &controlW)
	if startErr != nil {
		preparedFamily.Abort()
		_ = managed.closeStdin()
		closeManagedExecStdinPipe(stdin, stdinW, stdinDone)
		if ptySlave != nil {
			_ = ptySlave.Close()
			ptySlave = nil
		}
		if ptyMaster != nil {
			_ = ptyMaster.Close()
			ptyMaster = nil
		}
		if stdoutR != nil {
			_ = stdoutR.Close()
		}
		if stderrR != nil {
			_ = stderrR.Close()
		}
		waitManagedExecStreams(done, cap(done))
		exitCode, message := managedExecStartFailure(startErr, argv, rootDir, workDir)
		writeKernel("ccx3-init: exec error: " + startErr.Error())
		writeExecStderr(cfg, control, id, message)
		reporter.Exit(exitCode)
		return
	}
	reporter.Timing(managedExecTimingStarted)
	if stdinW != nil {
		stdinDone = make(chan struct{}, 1)
		go copyManagedExecStdinToPipe(stdinDone, stdin, stdinW)
	}
	startedFamily := preparedFamily.Start(cmd.Process.Pid)
	managed.setProcess(cmd.Process, signalGroup, startedFamily)
	if ptySlave != nil {
		_ = ptySlave.Close()
		ptySlave = nil
	}

	reporter.Timing(managedExecTimingWaitBegin)
	waitResult := waitManagedExecProcess(cmd, execStart)
	terminateManagedExecProcessGroup(cmd.Process.Pid)
	managed.stdinMu.Lock()
	family := managed.family
	managed.stdinMu.Unlock()
	if family != nil {
		family.Terminate()
	}
	reporter.Timing(managedExecTimingWaitDone)
	if tty {
		_ = managed.closeStdin()
	}
	if !tty {
		closeManagedExecStdinPipe(stdin, stdinW, stdinDone)
	}
	waitManagedExecStreams(done, cap(done))
	reporter.Timing(managedExecTimingStreamsDone)

	if waitResult.ExitErr != nil {
		writeKernel("ccx3-init: exec error: " + waitResult.ExitErr.Error())
		writeExecStderr(cfg, control, id, "ccx3-init: exec error: "+waitResult.ExitErr.Error()+"\n")
	}
	if waitResult.Usage != "" {
		reporter.Usage(waitResult.Usage)
	}
	if reporter.HasExitMarker() {
		reporter.Timing(managedExecTimingExitSent)
		reporter.Exit(waitResult.ExitCode)
	}
}

func terminateManagedExecProcessGroup(pgid int) {
	if pgid <= 0 || !managedExecProcessGroupExists(pgid) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if waitManagedExecProcessGroup(pgid, managedExecGroupTerminateGrace) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = waitManagedExecProcessGroup(pgid, managedExecGroupKillWait)
}

func waitManagedExecProcessGroup(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		reapOrphanedChildren(os.Getpid())
		if !managedExecProcessGroupExists(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func managedExecProcessGroupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func validateManagedExecWorkDir(rootDir, workDir string) error {
	if strings.TrimSpace(workDir) == "" {
		return nil
	}
	target := rootPath(rootDir, filepath.Clean(workDir))
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", workDir)
	}
	return nil
}

func configureHostname(hostname string) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" || hostname == "(none)" {
		hostname = "ccx3"
	}
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		return fmt.Errorf("set hostname %q: %w", hostname, err)
	}
	if err := os.MkdirAll("/etc", 0o755); err != nil {
		return fmt.Errorf("mkdir /etc: %w", err)
	}
	_ = os.WriteFile("/etc/hostname", []byte(hostname+"\n"), 0o644)
	hosts := "127.0.0.1\tlocalhost " + hostname + "\n::1\tlocalhost ip6-localhost ip6-loopback " + hostname + "\n"
	_ = os.WriteFile("/etc/hosts", []byte(hosts), 0o644)
	return nil
}

func configurePackageManagers(rootDir string) error {
	aptDir := rootPath(rootDir, "/etc/apt/apt.conf.d")
	if !pathExists(rootPath(rootDir, "/usr/bin/apt")) && !pathExists(rootPath(rootDir, "/bin/apt")) {
		return nil
	}
	if err := os.MkdirAll(aptDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", aptDir, err)
	}
	// Queue all HTTP work through one access-method worker so archive and
	// security downloads cannot leave each other's connections idle. Do not
	// disable apt's default HTTP pipelining: the old Pipeline-Depth=0 override
	// made full Ubuntu updates slow enough to hit the archive's five-second
	// keep-alive timeout, after which Ubuntu 24.04's apt worker crashed.
	conf := strings.Join([]string{
		`Acquire::Queue-Mode "access";`,
		`Acquire::Languages "none";`,
		`Acquire::IndexTargets::deb::DEP-11::DefaultEnabled "false";`,
		`Acquire::IndexTargets::deb::CNF::DefaultEnabled "false";`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(aptDir, "99ccvm-netstack"), []byte(conf), 0o644); err != nil {
		return fmt.Errorf("write apt netstack config: %w", err)
	}
	return nil
}

func pathExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

func rootPath(rootDir, name string) string {
	if rootDir == "" {
		return name
	}
	return filepath.Join(rootDir, strings.TrimPrefix(name, "/"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func prepareExecRoot(rootDir string) (string, []string, error) {
	cleaned := strings.TrimSpace(rootDir)
	if cleaned == "" {
		return "", nil, nil
	}
	if !strings.HasPrefix(cleaned, "/") {
		return "", nil, fmt.Errorf("root_dir must be absolute")
	}
	mounts := make([]string, 0, 6)
	if err := syscall.Mount(cleaned, cleaned, "", syscall.MS_BIND, ""); err != nil {
		return "", nil, fmt.Errorf("bind mount exec root %s: %w", cleaned, err)
	}
	mounts = append(mounts, cleaned)
	for _, dir := range []string{"/proc", "/sys", "/dev", "/run", "/tmp"} {
		target := cleaned + dir
		if err := os.MkdirAll(target, 0o755); err != nil {
			teardownExecRoot(mounts)
			return "", nil, fmt.Errorf("mkdir %s: %w", target, err)
		}
		flags := uintptr(syscall.MS_BIND)
		if dir == "/dev" {
			flags |= syscall.MS_REC
		}
		if err := syscall.Mount(dir, target, "", flags, ""); err != nil {
			teardownExecRoot(mounts)
			return "", nil, fmt.Errorf("bind mount %s -> %s: %w", dir, target, err)
		}
		mounts = append(mounts, target)
	}
	if err := copyExecRootNetworkFiles(cleaned); err != nil {
		teardownExecRoot(mounts)
		return "", nil, err
	}
	return cleaned, mounts, nil
}

func copyExecRootNetworkFiles(rootDir string) error {
	for _, name := range []string{"resolv.conf", "hosts", "hostname"} {
		src := filepath.Join("/etc", name)
		data, err := os.ReadFile(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read %s: %w", src, err)
		}
		dst := filepath.Join(rootDir, "etc", name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := writeFileReplacingMissingSymlink(dst, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}

func teardownExecRoot(mounts []string) {
	for i := len(mounts) - 1; i >= 0; i-- {
		_ = syscall.Unmount(mounts[i], 0)
	}
}

type sockaddrVM struct {
	Family   uint16
	Reserved uint16
	Port     uint32
	CID      uint32
	Zero     [4]byte
}

func connectVsock(port uint32) (*os.File, error) {
	return connectVsockWithTimeout(port, defaultVsockConnectTimeout)
}

func connectVsockWithTimeout(port uint32, timeout time.Duration) (*os.File, error) {
	fd, err := syscall.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = syscall.Close(fd)
		}
	}()
	if timeout > 0 {
		if err := syscall.SetNonblock(fd, true); err != nil {
			return nil, err
		}
	}
	addr := sockaddrVM{
		Family: unix.AF_VSOCK,
		Port:   port,
		CID:    2,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno == 0 {
		if timeout > 0 {
			if err := syscall.SetNonblock(fd, false); err != nil {
				return nil, err
			}
		}
		closeFD = false
		return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port)), nil
	}
	if timeout <= 0 || (errno != unix.EINPROGRESS && errno != unix.EALREADY && errno != unix.EWOULDBLOCK) {
		return nil, errno
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("vsock:%d connect timed out after %s", port, timeout)
		}
		timeoutMS := int(remaining / time.Millisecond)
		if timeoutMS < 1 {
			timeoutMS = 1
		}
		pollFD := []unix.PollFd{{
			Fd:     int32(fd),
			Events: unix.POLLOUT,
		}}
		n, err := unix.Poll(pollFD, timeoutMS)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, err
		}
		if n == 0 {
			continue
		}
		socketErr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return nil, err
		}
		if socketErr != 0 {
			return nil, syscall.Errno(socketErr)
		}
		if err := syscall.SetNonblock(fd, false); err != nil {
			return nil, err
		}
		closeFD = false
		return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port)), nil
	}
}
