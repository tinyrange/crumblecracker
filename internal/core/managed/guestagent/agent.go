//go:build !windows

package guestagent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
	"golang.org/x/sys/unix"
)

const (
	ReadyMarker         = protocol.ReadyMarker
	BeginMarkerPrefix   = protocol.BeginMarkerPrefix
	OutputMarkerPrefix  = protocol.OutputMarkerPrefix
	ErrorMarkerPrefix   = protocol.ErrorMarkerPrefix
	ControlMarkerPrefix = protocol.ControlMarkerPrefix
	UsageMarkerPrefix   = protocol.UsageMarkerPrefix
	ExitMarkerPrefix    = protocol.ExitMarkerPrefix
	TimingMarkerPrefix  = protocol.TimingMarkerPrefix
)

type Options struct {
	Name         string
	DialAddr     string
	ConnectTries int
	PTY          PTY
	Context      context.Context
}

type PTY interface {
	Open(cols, rows int) (master, slave *os.File, err error)
	Resize(master *os.File, cols, rows int) error
}

type Protocol struct {
	BeginMarkerPrefix   string
	OutputMarkerPrefix  string
	ErrorMarkerPrefix   string
	ControlMarkerPrefix string
	UsageMarkerPrefix   string
	ExitMarkerPrefix    string
	TimingMarkerPrefix  string
}

type ExecReporter struct {
	Protocol Protocol
	Control  io.Writer
	ID       string
	Start    time.Time
}

func DefaultProtocol() Protocol {
	return Protocol{
		BeginMarkerPrefix:   BeginMarkerPrefix,
		OutputMarkerPrefix:  OutputMarkerPrefix,
		ErrorMarkerPrefix:   ErrorMarkerPrefix,
		ControlMarkerPrefix: ControlMarkerPrefix,
		UsageMarkerPrefix:   UsageMarkerPrefix,
		ExitMarkerPrefix:    ExitMarkerPrefix,
		TimingMarkerPrefix:  TimingMarkerPrefix,
	}
}

func NewExecReporter(proto Protocol, control io.Writer, id string, start time.Time) ExecReporter {
	return ExecReporter{
		Protocol: proto,
		Control:  control,
		ID:       id,
		Start:    start,
	}
}

func (r ExecReporter) Begin() {
	r.Protocol.WriteBegin(r.Control, r.ID)
}

func (r ExecReporter) Stdout(data []byte) {
	r.Protocol.WriteStdout(r.Control, r.ID, data)
}

func (r ExecReporter) Stderr(data []byte) {
	r.Protocol.WriteStderr(r.Control, r.ID, data)
}

func (r ExecReporter) ControlBytes(data []byte) {
	r.Protocol.WriteControl(r.Control, r.ID, data)
}

func (r ExecReporter) Usage(encodedUsage string) {
	r.Protocol.WriteUsage(r.Control, r.ID, encodedUsage)
}

func (r ExecReporter) Exit(code int) {
	r.Protocol.WriteExit(r.Control, r.ID, code)
}

func (r ExecReporter) Timing(phase string) {
	r.Protocol.WriteTiming(r.Control, r.ID, phase, r.Start)
}

func (r ExecReporter) HasExitMarker() bool {
	return r.Protocol.ExitMarkerPrefix != "" && r.ID != ""
}

func (p Protocol) WriteBegin(w io.Writer, id string) {
	if p.BeginMarkerPrefix == "" || id == "" {
		return
	}
	WriteProtocolLine(w, p.BeginMarkerPrefix+id)
}

func (p Protocol) WriteStdout(w io.Writer, id string, data []byte) {
	WriteProtocolBytes(w, p.OutputMarkerPrefix, id, data)
}

func (p Protocol) WriteStderr(w io.Writer, id string, data []byte) {
	WriteProtocolBytes(w, p.ErrorMarkerPrefix, id, data)
}

func (p Protocol) WriteControl(w io.Writer, id string, data []byte) {
	WriteProtocolBytes(w, p.ControlMarkerPrefix, id, data)
}

func (p Protocol) WriteUsage(w io.Writer, id string, encodedUsage string) {
	if p.UsageMarkerPrefix == "" || id == "" || encodedUsage == "" {
		return
	}
	WriteProtocolLine(w, p.UsageMarkerPrefix+id+":"+encodedUsage)
}

func EncodeJSONBase64(value any) string {
	buf, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func (p Protocol) WriteExit(w io.Writer, id string, code int) {
	if p.ExitMarkerPrefix == "" || id == "" {
		return
	}
	WriteProtocolLine(w, p.ExitMarkerPrefix+id+":"+itoa(code))
}

func (p Protocol) WriteTiming(w io.Writer, id, phase string, start time.Time) {
	if p.TimingMarkerPrefix == "" || id == "" || phase == "" {
		return
	}
	WriteProtocolLine(w, p.TimingMarkerPrefix+id+":"+phase+":"+itoa(int(time.Since(start).Milliseconds())))
}

type request = protocol.ManagedExecRequest

type managedExec struct {
	stdinMu      sync.Mutex
	stdinWriteMu sync.Mutex
	stdin        io.WriteCloser

	processMu sync.Mutex
	process   *os.Process
	family    *ProcessFamily
	pending   syscall.Signal
	pty       *os.File
	ptyImpl   PTY
}

// reconnectingGuestControl is the logical guest-to-host protocol writer. A
// BSD control TCP connection can be replaced while commands are still alive;
// their output must follow the replacement connection rather than remain tied
// to the socket on which the command started.
type reconnectingGuestControl struct {
	ctx context.Context

	mu      sync.Mutex
	conn    net.Conn
	changed chan struct{}
}

func newReconnectingGuestControl(ctx context.Context) *reconnectingGuestControl {
	return &reconnectingGuestControl{ctx: ctx, changed: make(chan struct{})}
}

func (c *reconnectingGuestControl) signalChangedLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *reconnectingGuestControl) setConnection(conn net.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.signalChangedLocked()
	c.mu.Unlock()
}

func (c *reconnectingGuestControl) clearConnection(conn net.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
		c.signalChangedLocked()
	}
	c.mu.Unlock()
}

func (c *reconnectingGuestControl) connection() (net.Conn, error) {
	for {
		c.mu.Lock()
		conn := c.conn
		changed := c.changed
		c.mu.Unlock()
		if conn != nil {
			return conn, nil
		}
		select {
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		case <-changed:
		}
	}
}

func (c *reconnectingGuestControl) Write(data []byte) (int, error) {
	for {
		conn, err := c.connection()
		if err != nil {
			return 0, err
		}
		n, _ := conn.Write(data)
		if n == len(data) {
			return n, nil
		}
		c.clearConnection(conn)
		_ = conn.Close()
		// A partial protocol line on the failed physical connection is
		// discarded with that connection. Resend the complete line after
		// reconnect so the logical transcript always receives a valid frame.
	}
}

func Run(opts Options) error {
	opts = normalizeOptions(opts)
	_ = os.Chdir("/")
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	control := newReconnectingGuestControl(ctx)
	active := NewActiveExecSet()
	pending := NewPendingRequests[request]()
	startOrphanReaper(ctx)
	connected := false
	for {
		conn, err := connectControl(ctx, opts)
		if err != nil {
			if ctx.Err() != nil || !connected {
				return err
			}
			writeConsole("ccx3-" + opts.Name + "-init: control reconnect failed; retrying: " + err.Error() + "\n")
			continue
		}
		connected = true
		// Write readiness before publishing the replacement connection so
		// resumed command output cannot overtake the connection marker.
		if _, err := io.WriteString(conn, ReadyMarker+"\n"); err != nil {
			_ = conn.Close()
			continue
		}
		control.setConnection(conn)
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-done:
			}
		}()
		err = commandLoop(opts, conn, control, active, pending)
		close(done)
		control.clearConnection(conn)
		_ = conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		writeConsole("ccx3-" + opts.Name + "-init: control connection lost; reconnecting: " + err.Error() + "\n")
	}
}

func WriteConsole(s string) {
	writeConsole(s)
}

func normalizeOptions(opts Options) Options {
	if strings.TrimSpace(opts.Name) == "" {
		opts.Name = "guest"
	}
	if strings.TrimSpace(opts.DialAddr) == "" {
		opts.DialAddr = "10.42.0.1:10777"
	}
	if opts.ConnectTries <= 0 {
		opts.ConnectTries = 80
	}
	return opts
}

func connectControl(ctx context.Context, opts Options) (net.Conn, error) {
	var last error
	for i := 0; i < opts.ConnectTries; i++ {
		dialCtx, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp4", opts.DialAddr)
		cancel()
		if err == nil {
			return conn, nil
		}
		last = err
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("connect control: %w", last)
}

func commandLoop(opts Options, conn net.Conn, control io.Writer, active *ActiveExecSet, pending *PendingRequests[request]) error {
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("read request: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			writeConsole("ccx3-" + opts.Name + "-init: decode request: " + err.Error() + "\n")
			continue
		}
		if req.Kind == "" {
			req.Kind = "exec"
		}
		if requestStartsWork(req.Kind) {
			if err := validateGuestUser(req.User); err != nil {
				proto := DefaultProtocol()
				proto.WriteBegin(control, req.ID)
				writeErr(opts, control, req.ID, err)
				proto.WriteExit(control, req.ID, 126)
				continue
			}
		}
		switch req.Kind {
		case "exec":
			if req.ID == "" || len(req.Command) == 0 {
				continue
			}
			if len(req.Stdin) != 0 {
				startManagedExec(opts, control, active, req, io.NopCloser(bytes.NewReader(req.Stdin)), nil)
				continue
			}
			if req.TTY || req.ControlFD {
				stdinR, stdinW := io.Pipe()
				managed := &managedExec{stdin: stdinW, ptyImpl: opts.PTY}
				startManagedExec(opts, control, active, req, stdinR, managed)
				continue
			}
			// Start streamed commands before their first stdin message. Keeping
			// them pending made a signal race with stdin_close: the signal could
			// be discarded before the command became active, leaving it running
			// indefinitely. EOF remains explicit and is delivered by stdin_close.
			stdinR, stdinW := io.Pipe()
			managed := &managedExec{stdin: stdinW, ptyImpl: opts.PTY}
			startManagedExec(opts, control, active, req, stdinR, managed)
		case "sync":
			go runSync(control, req.ID)
		case "fs_mkdir":
			go runMkdir(opts, control, req)
		case "fs_write":
			go runWrite(opts, control, req)
		case "fs_extract":
			if len(req.Stdin) > 0 {
				go runExtract(opts, control, req, io.NopCloser(bytes.NewReader(req.Stdin)), func() {})
				continue
			}
			stdinR, stdinW := io.Pipe()
			managed := &managedExec{stdin: stdinW, ptyImpl: opts.PTY}
			active.Add(req.ID, managed)
			go runExtract(opts, control, req, stdinR, func() {
				_ = managed.close()
				active.Delete(req.ID)
			})
		case "fs_archive":
			go runArchive(opts, control, req)
		case "stdin":
			pendingReq, pendingOK := pending.Take(req.ID)
			if pendingOK {
				stdinR, stdinW := io.Pipe()
				managed := &managedExec{stdin: stdinW, ptyImpl: opts.PTY}
				startManagedExec(opts, control, active, pendingReq, stdinR, managed)
				_ = managed.write(req.Stdin)
				continue
			}
			_ = HandleActiveControl(active, ActiveControlRequest{
				Kind:  req.Kind,
				ID:    req.ID,
				Stdin: req.Stdin,
			})
		case "stdin_close":
			pendingReq, pendingOK := pending.Take(req.ID)
			if pendingOK {
				startManagedExec(opts, control, active, pendingReq, io.NopCloser(bytes.NewReader(nil)), nil)
				continue
			}
			_ = HandleActiveControl(active, ActiveControlRequest{
				Kind: req.Kind,
				ID:   req.ID,
			})
		case "signal":
			if req.ControlID != "" && active.ControlAcknowledged(req.ID, req.ControlID) {
				WriteProtocolLine(control, protocol.ControlAckPrefix+req.ControlID)
				continue
			}
			result := HandleActiveControl(active, ActiveControlRequest{
				Kind:   req.Kind,
				ID:     req.ID,
				Signal: req.Signal,
			})
			if req.ControlID != "" && result.Found && result.Err == nil {
				active.AcknowledgeControl(req.ID, req.ControlID)
				WriteProtocolLine(control, protocol.ControlAckPrefix+req.ControlID)
			}
		case "resize":
			_ = HandleActiveControl(active, ActiveControlRequest{
				Kind: req.Kind,
				ID:   req.ID,
				Cols: req.Cols,
				Rows: req.Rows,
			})
		}
	}
}

func requestStartsWork(kind string) bool {
	switch kind {
	case "exec", "sync", "fs_mkdir", "fs_write", "fs_extract", "fs_archive":
		return true
	default:
		return false
	}
}

func isRootUserRequest(user string) bool {
	switch strings.ToLower(strings.TrimSpace(user)) {
	case "", "root", "0", "0:0", "root:root", "root:wheel":
		return true
	default:
		return false
	}
}

func (m *managedExec) write(data []byte) error {
	m.stdinWriteMu.Lock()
	defer m.stdinWriteMu.Unlock()
	m.stdinMu.Lock()
	stdin := m.stdin
	m.stdinMu.Unlock()
	if stdin == nil {
		return fmt.Errorf("stdin is closed")
	}
	_, err := stdin.Write(data)
	return err
}

func (m *managedExec) close() error {
	m.stdinMu.Lock()
	stdin := m.stdin
	m.stdin = nil
	m.stdinMu.Unlock()
	if stdin == nil {
		return nil
	}
	return stdin.Close()
}

func (m *managedExec) setProcess(p *os.Process, family *ProcessFamily) {
	m.processMu.Lock()
	m.process = p
	m.family = family
	pending := m.pending
	m.pending = 0
	m.processMu.Unlock()
	if pending != 0 {
		_ = signalProcessGroup(p, pending)
		_ = family.Signal(pending)
	}
}

func (m *managedExec) setPTY(pty *os.File) {
	m.processMu.Lock()
	defer m.processMu.Unlock()
	m.pty = pty
}

func (m *managedExec) resize(cols, rows int) error {
	m.processMu.Lock()
	defer m.processMu.Unlock()
	if m.ptyImpl == nil {
		return nil
	}
	if m.pty == nil {
		return fmt.Errorf("exec has no tty")
	}
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid tty size %dx%d", cols, rows)
	}
	return m.ptyImpl.Resize(m.pty, cols, rows)
}

func (m *managedExec) WriteStdin(data []byte) error {
	return m.write(data)
}

func (m *managedExec) CloseStdin() error {
	return m.close()
}

func (m *managedExec) Signal(name string) error {
	return m.signal(name)
}

func (m *managedExec) Resize(cols, rows int) error {
	return m.resize(cols, rows)
}

func (m *managedExec) signal(name string) error {
	sig, err := parseSignal(name)
	if err != nil {
		return err
	}
	// A streamed stdin reader keeps os/exec's copy goroutine alive. SIGKILL
	// guarantees that the command cannot consume more input, so close it before
	// waiting for the process; otherwise Wait can retain a killed command until
	// the control connection happens to close stdin separately.
	if sig == syscall.SIGKILL {
		_ = m.close()
	}
	m.processMu.Lock()
	if m.process == nil {
		if m.pending == 0 || sig == syscall.SIGKILL {
			m.pending = sig
		}
		m.processMu.Unlock()
		return nil
	}
	process := m.process
	family := m.family
	m.processMu.Unlock()
	// Deliver the requested signal to the command's process group first. A
	// descendant tracker is best-effort bookkeeping for processes which escaped
	// that group; it must not delay or prevent the ordinary command lifecycle.
	groupErr := signalProcessGroup(process, sig)
	if family != nil {
		err = family.Signal(sig)
	}
	if err == nil {
		err = groupErr
	}
	if family != nil {
		// Refresh once more after the group signal to catch a child which raced
		// with the first process-table snapshot.
		_ = family.Signal(sig)
	}
	return err
}

func signalProcessGroup(process *os.Process, sig syscall.Signal) error {
	if process.Pid > 0 {
		if err := syscall.Kill(-process.Pid, sig); err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
	}
	return process.Signal(sig)
}

func startManagedExec(opts Options, control io.Writer, active *ActiveExecSet, req request, stdin io.ReadCloser, managed *managedExec) {
	if managed == nil {
		managed = &managedExec{ptyImpl: opts.PTY}
	}
	closePending := active.Add(req.ID, managed)
	// The request's input sink is usable once it is registered. Advertise that
	// protocol state before the worker runs so the host can safely send input or
	// EOF without relying on guest scheduler timing.
	DefaultProtocol().WriteTiming(control, req.ID, "input_ready", time.Now())
	if closePending {
		_ = managed.close()
	}
	go runExec(opts, control, req, stdin, managed, func() {
		_ = managed.close()
		active.Delete(req.ID)
	})
}

func runExec(opts Options, control io.Writer, req request, stdin io.ReadCloser, managed *managedExec, cleanup func()) {
	defer cleanup()
	proto := DefaultProtocol()
	proto.WriteBegin(control, req.ID)
	if err := validateExecWorkDir(req.RootDir, req.WorkDir); err != nil {
		if managed != nil {
			_ = managed.close()
		}
		if stdin != nil {
			_ = stdin.Close()
		}
		writeErr(opts, control, req.ID, fmt.Errorf("invalid workdir: %w", err))
		proto.WriteExit(control, req.ID, 126)
		return
	}
	cmd := execCommand(req)
	var controlR, controlW *os.File
	if req.ControlFD {
		var err error
		controlR, controlW, err = os.Pipe()
		if err != nil {
			if managed != nil {
				_ = managed.close()
			}
			writeErr(opts, control, req.ID, fmt.Errorf("open control fd: %w", err))
			proto.WriteExit(control, req.ID, 126)
			return
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, controlW)
	}
	var wg sync.WaitGroup
	var stdoutR, stdoutW, stderrR, stderrW *os.File
	var ptyMaster, ptySlave *os.File
	if req.TTY && opts.PTY != nil {
		var err error
		ptyMaster, ptySlave, err = opts.PTY.Open(req.Cols, req.Rows)
		if err != nil {
			if managed != nil {
				_ = managed.close()
			}
			if controlR != nil {
				_ = controlR.Close()
			}
			if controlW != nil {
				_ = controlW.Close()
			}
			writeErr(opts, control, req.ID, fmt.Errorf("open pty: %w", err))
			proto.WriteExit(control, req.ID, 126)
			return
		}
		if managed != nil {
			managed.setPTY(ptyMaster)
		}
		cmd.Stdin = ptySlave
		cmd.Stdout = ptySlave
		cmd.Stderr = ptySlave
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0,
		}
		wg.Add(1)
		go copyMarked(&wg, control, proto.OutputMarkerPrefix, req.ID, ptyMaster)
		if stdin != nil {
			wg.Add(1)
			go copyPTYStdin(&wg, stdin, ptyMaster)
		}
	} else {
		cmd.Stdin = stdin
		var err error
		stdoutR, stdoutW, err = os.Pipe()
		if err == nil {
			stderrR, stderrW, err = os.Pipe()
		}
		if err != nil {
			if managed != nil {
				_ = managed.close()
			}
			if stdin != nil {
				_ = stdin.Close()
			}
			if controlR != nil {
				_ = controlR.Close()
			}
			if controlW != nil {
				_ = controlW.Close()
			}
			if stdoutR != nil {
				_ = stdoutR.Close()
			}
			if stdoutW != nil {
				_ = stdoutW.Close()
			}
			writeErr(opts, control, req.ID, fmt.Errorf("open output pipes: %w", err))
			proto.WriteExit(control, req.ID, 126)
			return
		}
		cmd.Stdout = stdoutW
		cmd.Stderr = stderrW
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		wg.Add(2)
		go copyMarked(&wg, control, proto.OutputMarkerPrefix, req.ID, stdoutR)
		go copyMarked(&wg, control, proto.ErrorMarkerPrefix, req.ID, stderrR)
	}
	if controlR != nil {
		wg.Add(1)
		go copyMarked(&wg, control, proto.ControlMarkerPrefix, req.ID, controlR)
	}
	preparedFamily, prepareErr := PrepareProcessFamily(cmd, req.ID)
	if prepareErr != nil {
		writeErr(opts, control, req.ID, fmt.Errorf("prepare command containment: %w", prepareErr))
		proto.WriteExit(control, req.ID, 126)
		return
	}
	if err := startManagedCommand(cmd); err != nil {
		preparedFamily.Abort()
		if managed != nil {
			_ = managed.close()
		}
		if controlR != nil {
			_ = controlR.Close()
		}
		if controlW != nil {
			_ = controlW.Close()
		}
		if stdoutW != nil {
			_ = stdoutW.Close()
		}
		if stderrW != nil {
			_ = stderrW.Close()
		}
		if ptyMaster != nil {
			_ = ptyMaster.Close()
		}
		if ptySlave != nil {
			_ = ptySlave.Close()
		}
		wg.Wait()
		if stdoutR != nil {
			_ = stdoutR.Close()
		}
		if stderrR != nil {
			_ = stderrR.Close()
		}
		writeErr(opts, control, req.ID, err)
		proto.WriteExit(control, req.ID, 126)
		return
	}
	if controlW != nil {
		_ = controlW.Close()
	}
	family := preparedFamily.Start(cmd.Process.Pid)
	if managed != nil {
		managed.setProcess(cmd.Process, family)
	}
	waitErr := waitManagedCommand(cmd)
	if managed != nil {
		managed.processMu.Lock()
		family = managed.family
		managed.processMu.Unlock()
	}
	if family != nil {
		family.Terminate()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if stdoutW != nil {
		_ = stdoutW.Close()
	}
	if stderrW != nil {
		_ = stderrW.Close()
	}
	if ptySlave != nil {
		_ = ptySlave.Close()
	}
	if ptyMaster != nil {
		_ = ptyMaster.Close()
	}
	wg.Wait()
	if stdoutR != nil {
		_ = stdoutR.Close()
	}
	if stderrR != nil {
		_ = stderrR.Close()
	}
	if controlR != nil {
		_ = controlR.Close()
	}
	code := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			code = ProcessExitCode(cmd.ProcessState, exitErr.ExitCode())
		} else {
			writeErr(opts, control, req.ID, waitErr)
			code = 126
		}
	}
	proto.WriteExit(control, req.ID, code)
}

func ProcessExitCode(state *os.ProcessState, fallback int) int {
	if state == nil {
		return fallback
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return fallback
	}
	return 128 + int(status.Signal())
}

func validateExecWorkDir(rootDir, workDir string) error {
	if strings.TrimSpace(workDir) == "" {
		return nil
	}
	path := rootPath(rootDir, workDir)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

func execCommand(req request) *exec.Cmd {
	name := req.Command[0]
	cmd := exec.Command(name, req.Command[1:]...)
	if !strings.ContainsRune(name, filepath.Separator) {
		if pathValue, ok := environmentValue(req.Env, "PATH"); ok {
			path, err := lookPath(name, pathValue)
			cmd.Path = path
			cmd.Err = err
		}
	}
	if req.WorkDir != "" {
		cmd.Dir = rootPath(req.RootDir, req.WorkDir)
	}
	if len(req.Env) != 0 {
		cmd.Env = req.Env
	}
	return cmd
}

func environmentValue(env []string, name string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(env[i], "=")
		if ok && key == name {
			return value, true
		}
	}
	return "", false
}

func lookPath(name, pathValue string) (string, error) {
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: %s", exec.ErrNotFound, name)
}

func runSync(control io.Writer, id string) {
	proto := DefaultProtocol()
	proto.WriteBegin(control, id)
	syscall.Sync()
	proto.WriteExit(control, id, 0)
}

func runMkdir(opts Options, control io.Writer, req request) {
	proto := DefaultProtocol()
	proto.WriteBegin(control, req.ID)
	code := 0
	target := rootPath(req.RootDir, firstNonEmpty(req.Path, "."))
	if err := os.MkdirAll(target, 0o755); err != nil {
		writeErr(opts, control, req.ID, err)
		code = 1
	}
	proto.WriteExit(control, req.ID, code)
}

func runWrite(opts Options, control io.Writer, req request) {
	proto := DefaultProtocol()
	proto.WriteBegin(control, req.ID)
	code := 0
	if strings.TrimSpace(req.Path) == "" {
		writeErr(opts, control, req.ID, fmt.Errorf("destination path is required"))
		code = 1
	} else {
		target := rootPath(req.RootDir, req.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			writeErr(opts, control, req.ID, err)
			code = 1
		} else if err := os.WriteFile(target, req.Stdin, 0o644); err != nil {
			writeErr(opts, control, req.ID, err)
			code = 1
		}
	}
	proto.WriteExit(control, req.ID, code)
}

func runExtract(opts Options, control io.Writer, req request, r io.ReadCloser, cleanup func()) {
	defer cleanup()
	defer r.Close()
	proto := DefaultProtocol()
	proto.WriteBegin(control, req.ID)
	code := 0
	ctx, cancel, err := ArchiveContext(context.Background(), req.ArchiveLimits)
	if err != nil {
		writeErr(opts, control, req.ID, err)
		proto.WriteExit(control, req.ID, 1)
		return
	}
	defer cancel()
	if err := extractTarToPath(ctx, r, req.RootDir, req.Path, req.Directory, req.ArchiveLimits); err != nil {
		writeErr(opts, control, req.ID, err)
		code = 1
	}
	proto.WriteExit(control, req.ID, code)
}

func runArchive(opts Options, control io.Writer, req request) {
	proto := DefaultProtocol()
	proto.WriteBegin(control, req.ID)
	code := 0
	if err := archivePath(control, req.ID, req.RootDir, req.Path); err != nil {
		writeErr(opts, control, req.ID, err)
		code = 1
	}
	proto.WriteExit(control, req.ID, code)
}

func archivePath(control io.Writer, id, rootDir, src string) error {
	if strings.TrimSpace(src) == "" {
		return fmt.Errorf("source path is required")
	}
	src = rootPath(rootDir, src)
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		err := WritePathTar(pw, src, filepath.Base(src), info)
		_ = pw.CloseWithError(err)
	}()
	var buf [32768]byte
	for {
		n, err := pr.Read(buf[:])
		if n > 0 {
			DefaultProtocol().WriteStdout(control, id, buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func WritePathTar(w io.Writer, src, rootName string, rootInfo os.FileInfo) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	hardlinks := make(map[string]string)
	return filepath.WalkDir(src, func(filePath string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Dir(src), filePath)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = rootName
			info = rootInfo
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filePath)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Format = tar.FormatPAX
		xattrs, err := archiveXattrs(filePath)
		if err != nil {
			return err
		}
		if len(xattrs) != 0 {
			header.PAXRecords = xattrs
		}
		if info.Mode().IsRegular() {
			if key, ok := archiveHardlinkKey(info); ok {
				if first, exists := hardlinks[key]; exists {
					header.Typeflag = tar.TypeLink
					header.Linkname = first
					header.Size = 0
				} else {
					hardlinks[key] = header.Name
				}
			}
		}
		if !info.Mode().IsRegular() || header.Typeflag == tar.TypeLink {
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		exts, sparse := archiveSparseExtents(file, info.Size())
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			_ = file.Close()
			return err
		}
		if sparse {
			physical := int64(0)
			mapValues := make([]string, 0, len(exts)*2)
			for _, extent := range exts {
				physical += extent.length
				mapValues = append(mapValues, strconv.FormatInt(extent.offset, 10), strconv.FormatInt(extent.length, 10))
			}
			if header.PAXRecords == nil {
				header.PAXRecords = make(map[string]string)
			}
			header.PAXRecords["VMSH.sparse.numblocks"] = strconv.Itoa(len(exts))
			header.PAXRecords["VMSH.sparse.map"] = strings.Join(mapValues, ",")
			header.PAXRecords["VMSH.sparse.size"] = strconv.FormatInt(info.Size(), 10)
			header.Size = physical
		}
		if err := tw.WriteHeader(header); err != nil {
			_ = file.Close()
			return err
		}
		var copyErr error
		if sparse {
			for _, extent := range exts {
				if _, err := file.Seek(extent.offset, io.SeekStart); err != nil {
					copyErr = err
					break
				}
				if _, err := io.CopyN(tw, file, extent.length); err != nil {
					copyErr = err
					break
				}
			}
		} else {
			_, copyErr = io.Copy(tw, file)
		}
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

type archiveSparseExtent struct {
	offset int64
	length int64
}

func archiveSparseExtents(file *os.File, size int64) ([]archiveSparseExtent, bool) {
	if size <= 0 {
		return nil, false
	}
	var extents []archiveSparseExtent
	for offset := int64(0); offset < size; {
		data, err := archiveSeekData(int(file.Fd()), offset)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if err != nil || data < offset || data >= size {
			return nil, false
		}
		hole, err := archiveSeekHole(int(file.Fd()), data)
		if err != nil || hole <= data {
			return nil, false
		}
		if hole > size {
			hole = size
		}
		extents = append(extents, archiveSparseExtent{offset: data, length: hole - data})
		offset = hole
	}
	physical := int64(0)
	for _, extent := range extents {
		physical += extent.length
	}
	return extents, physical < size
}

const archiveXattrPrefix = "VMSH.xattr."

func archiveXattrs(path string) (map[string]string, error) {
	size, err := archiveListXattrs(path, nil)
	if err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) {
			return nil, nil
		}
		return nil, fmt.Errorf("list extended attributes for %q: %w", path, err)
	}
	if size == 0 {
		return nil, nil
	}
	names := make([]byte, size)
	size, err = archiveListXattrs(path, names)
	if err != nil {
		return nil, fmt.Errorf("list extended attributes for %q: %w", path, err)
	}
	records := make(map[string]string)
	for _, name := range strings.Split(string(names[:size]), "\x00") {
		// Artifacts cross trust and VM boundaries. Preserve the portable user
		// namespace, but never replay security, trusted, or system metadata
		// supplied by another filesystem.
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		valueSize, err := archiveGetXattr(path, name, nil)
		if err != nil {
			return nil, fmt.Errorf("read extended attribute %q for %q: %w", name, path, err)
		}
		value := make([]byte, valueSize)
		valueSize, err = archiveGetXattr(path, name, value)
		if err != nil {
			return nil, fmt.Errorf("read extended attribute %q for %q: %w", name, path, err)
		}
		key := archiveXattrPrefix + base64.RawURLEncoding.EncodeToString([]byte(name))
		records[key] = base64.StdEncoding.EncodeToString(value[:valueSize])
	}
	return records, nil
}

func archiveHardlinkKey(info os.FileInfo) (string, bool) {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return "", false
	}
	dev, devOK := archiveStatField(value.FieldByName("Dev"))
	ino, inoOK := archiveStatField(value.FieldByName("Ino"))
	if !devOK || !inoOK || ino == 0 {
		return "", false
	}
	return fmt.Sprintf("%d:%d", dev, ino), true
}

func archiveStatField(value reflect.Value) (uint64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		integer := value.Int()
		return uint64(integer), integer >= 0
	default:
		return 0, false
	}
}

func extractTarToPath(ctx context.Context, r io.Reader, rootDir, dst string, dstDir bool, limits *client.ArchiveLimits) error {
	return ExtractTarToPathContext(ctx, r, rootDir, dst, dstDir, limits)
}

var ErrUnsafeTarExtractionPath = errors.New("unsafe tar extraction path")

func ExtractTarToPath(r io.Reader, rootDir, dst string, dstDir bool) error {
	return ExtractTarToPathContext(context.Background(), r, rootDir, dst, dstDir, nil)
}

// ArchiveOwnership applies one caller-selected guest identity to every entry
// published by an archive extraction. Tar header ownership is intentionally
// ignored because archives can cross trust and VM boundaries.
type ArchiveOwnership struct {
	UID int
	GID int
}

type ArchiveLimitError struct {
	Resource string
	Limit    uint64
	Actual   uint64
}

func (e *ArchiveLimitError) Error() string {
	return fmt.Sprintf("archive %s limit exceeded: limit=%d actual=%d", e.Resource, e.Limit, e.Actual)
}

func ArchiveContext(parent context.Context, limits *client.ArchiveLimits) (context.Context, context.CancelFunc, error) {
	if limits == nil || limits.TimeoutSeconds == 0 {
		return parent, func() {}, nil
	}
	if limits.TimeoutSeconds < 0 || math.IsNaN(limits.TimeoutSeconds) || math.IsInf(limits.TimeoutSeconds, 0) {
		return nil, nil, fmt.Errorf("archive timeout must be finite and positive")
	}
	d := time.Duration(limits.TimeoutSeconds * float64(time.Second))
	if d <= 0 {
		return nil, nil, fmt.Errorf("archive timeout is below timer resolution")
	}
	ctx, cancel := context.WithTimeout(parent, d)
	return ctx, cancel, nil
}

func ExtractTarToPathContext(ctx context.Context, r io.Reader, rootDir, dst string, dstDir bool, requested *client.ArchiveLimits) error {
	return ExtractTarToPathContextWithOwnership(ctx, r, rootDir, dst, dstDir, requested, nil)
}

func ExtractTarToPathContextWithOwnership(ctx context.Context, r io.Reader, rootDir, dst string, dstDir bool, requested *client.ArchiveLimits, owner *ArchiveOwnership) error {
	if strings.TrimSpace(dst) == "" {
		return fmt.Errorf("destination path is required")
	}
	dst = rootPath(rootDir, dst)
	limits, err := resolveArchiveLimits(dst, requested)
	if err != nil {
		return err
	}
	stopClose := make(chan struct{})
	if closer, ok := r.(io.Closer); ok && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-stopClose:
			}
		}()
		defer close(stopClose)
	}
	dstExisted := false
	if info, err := os.Lstat(dst); err == nil {
		dstExisted = true
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: destination is a symlink: %s", ErrUnsafeTarExtractionPath, dst)
		}
		if info.IsDir() {
			dstDir = true
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	extractionRoot := filepath.Dir(dst)
	if dstDir {
		extractionRoot = dst
	}
	extractionBoundary, err := nearestExistingTarParent(extractionRoot)
	if err != nil {
		return err
	}
	if err := ensureTarParents(extractionBoundary, extractionRoot, owner); err != nil {
		return err
	}
	if dstDir && !dstExisted {
		if err := applyArchiveOwnership(extractionRoot, false, owner); err != nil {
			return err
		}
	}
	tr := tar.NewReader(r)
	sawEntry := false
	var dirs []tarDirMtime
	var entries uint64
	var expanded uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if !sawEntry {
				return fmt.Errorf("archive is empty")
			}
			return restoreTarDirMtimes(dirs)
		}
		if err != nil {
			return err
		}
		sawEntry = true
		entries++
		if entries > limits.MaxEntries {
			return &ArchiveLimitError{Resource: "entry count", Limit: limits.MaxEntries, Actual: entries}
		}
		if header.Size < 0 {
			return fmt.Errorf("archive entry %q has negative size", header.Name)
		}
		logicalSize, sizeErr := archiveLogicalSize(header)
		if sizeErr != nil {
			return sizeErr
		}
		entryBytes := uint64(logicalSize)
		if entryBytes > limits.MaxFileBytes {
			return &ArchiveLimitError{Resource: "file bytes", Limit: limits.MaxFileBytes, Actual: entryBytes}
		}
		if entryBytes > limits.MaxExpandedBytes-expanded {
			return &ArchiveLimitError{Resource: "expanded bytes", Limit: limits.MaxExpandedBytes, Actual: expanded + entryBytes}
		}
		metadataBytes, metadataErr := archiveHeaderMetadataBytes(header)
		if metadataErr != nil {
			return metadataErr
		}
		if metadataBytes > limits.MaxExpandedBytes-expanded-entryBytes {
			return &ArchiveLimitError{Resource: "expanded bytes", Limit: limits.MaxExpandedBytes, Actual: expanded + entryBytes + metadataBytes}
		}
		expanded += entryBytes + metadataBytes
		target, err := tarTarget(dst, dstDir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := ensureTarParents(extractionRoot, filepath.Dir(target), owner); err != nil {
				return err
			}
			if err := ensureTarTargetCompatible(target, true); err != nil {
				return err
			}
			if err := os.Mkdir(target, os.FileMode(header.Mode).Perm()); err != nil && !os.IsExist(err) {
				return err
			}
			_ = os.Chmod(target, os.FileMode(header.Mode).Perm())
			if err := applyArchiveOwnership(target, false, owner); err != nil {
				return err
			}
			if err := applyArchiveXattrs(target, false, header); err != nil {
				return err
			}
			dirs = append(dirs, tarDirMtime{path: target, mtime: header.ModTime})
		case tar.TypeSymlink:
			if err := ensureTarParents(extractionRoot, filepath.Dir(target), owner); err != nil {
				return err
			}
			if err := validateTarSymlinkTarget(extractionRoot, target, header.Linkname); err != nil {
				return err
			}
			if err := ensureTarTargetCompatible(target, false); err != nil {
				return err
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
			if err := applyArchiveOwnership(target, true, owner); err != nil {
				return err
			}
			if err := applyArchiveXattrs(target, true, header); err != nil {
				return err
			}
			if err := setArchiveTimes(target, header.ModTime, true); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := ensureTarParents(extractionRoot, filepath.Dir(target), owner); err != nil {
				return err
			}
			if err := ensureTarTargetCompatible(target, false); err != nil {
				return err
			}
			file, err := os.CreateTemp(filepath.Dir(target), ".cc-extract-*")
			if err != nil {
				return err
			}
			tmpName := file.Name()
			copyErr := copyTarRegularFile(ctx, file, tr, header)
			closeErr := file.Close()
			if copyErr != nil {
				_ = os.Remove(tmpName)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return copyErr
			}
			if closeErr != nil {
				_ = os.Remove(tmpName)
				return closeErr
			}
			if err := applyArchiveOwnership(tmpName, false, owner); err != nil {
				_ = os.Remove(tmpName)
				return err
			}
			perm := os.FileMode(header.Mode).Perm()
			if err := os.Chmod(tmpName, perm); err != nil {
				_ = os.Remove(tmpName)
				return err
			}
			if err := os.Chtimes(tmpName, header.ModTime, header.ModTime); err != nil {
				_ = os.Remove(tmpName)
				return err
			}
			if err := applyArchiveXattrs(tmpName, false, header); err != nil {
				_ = os.Remove(tmpName)
				return err
			}
			if err := os.Rename(tmpName, target); err != nil {
				_ = os.Remove(tmpName)
				return err
			}
		case tar.TypeLink:
			if err := ensureTarParents(extractionRoot, filepath.Dir(target), owner); err != nil {
				return err
			}
			source, err := tarTarget(dst, dstDir, header.Linkname)
			if err != nil {
				return fmt.Errorf("%w: hard link %q: %v", ErrUnsafeTarExtractionPath, header.Name, err)
			}
			sourceInfo, err := os.Lstat(source)
			if err != nil {
				return fmt.Errorf("hard link source %q: %w", header.Linkname, err)
			}
			if !sourceInfo.Mode().IsRegular() {
				return fmt.Errorf("%w: hard link source is not a regular file: %s", ErrUnsafeTarExtractionPath, source)
			}
			if err := ensureTarTargetCompatible(target, false); err != nil {
				return err
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Link(source, target); err != nil {
				return err
			}
			// Some FUSE clients retain the pre-link attributes for an inode even
			// when LINK returns the updated entry. A no-op setattr makes the new
			// link count visible without changing archive metadata.
			if err := os.Chmod(source, sourceInfo.Mode().Perm()); err != nil {
				return err
			}
		}
	}
}

func archiveHeaderMetadataBytes(header *tar.Header) (uint64, error) {
	var total uint64
	for key, encoded := range header.PAXRecords {
		if !strings.HasPrefix(key, archiveXattrPrefix) {
			continue
		}
		name, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, archiveXattrPrefix))
		if err != nil {
			return 0, fmt.Errorf("archive entry %q has an invalid extended attribute name", header.Name)
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return 0, fmt.Errorf("archive entry %q has an invalid extended attribute value", header.Name)
		}
		addition := uint64(len(name) + len(value) + 64)
		if addition > math.MaxUint64-total {
			return 0, fmt.Errorf("archive entry %q metadata is too large", header.Name)
		}
		total += addition
	}
	return total, nil
}

func copyTarRegularFile(ctx context.Context, file *os.File, tr *tar.Reader, header *tar.Header) error {
	exts, logicalSize, sparse, err := archiveSparseMap(header)
	if err != nil {
		return err
	}
	reader := contextReader{ctx: ctx, r: tr}
	if !sparse {
		_, err := io.Copy(file, reader)
		return err
	}
	for _, extent := range exts {
		if extent.offset < 0 || extent.length < 0 || extent.offset > logicalSize-extent.length {
			return fmt.Errorf("archive entry %q has an invalid sparse map", header.Name)
		}
		if _, err := file.Seek(extent.offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(file, reader, extent.length); err != nil {
			return err
		}
	}
	return file.Truncate(logicalSize)
}

func archiveSparseMap(header *tar.Header) ([]archiveSparseExtent, int64, bool, error) {
	if header == nil || header.PAXRecords == nil {
		return nil, 0, false, nil
	}
	encodedSize, present := header.PAXRecords["VMSH.sparse.size"]
	if !present {
		return nil, 0, false, nil
	}
	logicalSize, sizeErr := strconv.ParseInt(encodedSize, 10, 64)
	if sizeErr != nil || logicalSize < 0 {
		return nil, 0, false, fmt.Errorf("archive entry %q has an invalid sparse size", header.Name)
	}
	count, err := strconv.Atoi(header.PAXRecords["VMSH.sparse.numblocks"])
	if err != nil || count < 0 {
		return nil, 0, false, fmt.Errorf("archive entry %q has an invalid sparse extent count", header.Name)
	}
	values := []string{}
	if encoded := header.PAXRecords["VMSH.sparse.map"]; encoded != "" {
		values = strings.Split(encoded, ",")
	}
	if len(values) != count*2 {
		return nil, 0, false, fmt.Errorf("archive entry %q has an invalid sparse map", header.Name)
	}
	exts := make([]archiveSparseExtent, 0, count)
	var physicalSize int64
	var previousEnd int64
	for i := 0; i < len(values); i += 2 {
		offset, offsetErr := strconv.ParseInt(values[i], 10, 64)
		length, lengthErr := strconv.ParseInt(values[i+1], 10, 64)
		if offsetErr != nil || lengthErr != nil || offset < 0 || length < 0 {
			return nil, 0, false, fmt.Errorf("archive entry %q has an invalid sparse map", header.Name)
		}
		if offset < previousEnd || offset > logicalSize-length || physicalSize > header.Size-length {
			return nil, 0, false, fmt.Errorf("archive entry %q has an invalid sparse map", header.Name)
		}
		physicalSize += length
		previousEnd = offset + length
		exts = append(exts, archiveSparseExtent{offset: offset, length: length})
	}
	if physicalSize != header.Size {
		return nil, 0, false, fmt.Errorf("archive entry %q has an invalid sparse payload size", header.Name)
	}
	return exts, logicalSize, true, nil
}

func archiveLogicalSize(header *tar.Header) (int64, error) {
	_, logicalSize, sparse, err := archiveSparseMap(header)
	if err != nil {
		return 0, err
	}
	if sparse {
		return logicalSize, nil
	}
	return header.Size, nil
}

func applyArchiveXattrs(path string, symlink bool, header *tar.Header) error {
	if header == nil {
		return nil
	}
	for key, encoded := range header.PAXRecords {
		if !strings.HasPrefix(key, archiveXattrPrefix) {
			continue
		}
		name, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, archiveXattrPrefix))
		if err != nil || !bytes.HasPrefix(name, []byte("user.")) || bytes.IndexByte(name, 0) >= 0 {
			return fmt.Errorf("archive entry %q has an invalid extended attribute name", header.Name)
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("archive entry %q has an invalid extended attribute value", header.Name)
		}
		err = archiveSetXattr(path, string(name), value, symlink)
		if err != nil {
			return fmt.Errorf("restore extended attribute %q for %q: %w", string(name), path, err)
		}
	}
	return nil
}

func setArchiveTimes(path string, timestamp time.Time, symlink bool) error {
	if !symlink {
		return os.Chtimes(path, timestamp, timestamp)
	}
	times := []unix.Timespec{
		{Sec: timestamp.Unix(), Nsec: int64(timestamp.Nanosecond())},
		{Sec: timestamp.Unix(), Nsec: int64(timestamp.Nanosecond())},
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, times, unix.AT_SYMLINK_NOFOLLOW)
}

func applyArchiveOwnership(target string, symlink bool, owner *ArchiveOwnership) error {
	if owner == nil {
		return nil
	}
	if symlink {
		return os.Lchown(target, owner.UID, owner.GID)
	}
	return os.Chown(target, owner.UID, owner.GID)
}

type resolvedArchiveLimits struct {
	MaxEntries       uint64
	MaxFileBytes     uint64
	MaxExpandedBytes uint64
}

func resolveArchiveLimits(dst string, requested *client.ArchiveLimits) (resolvedArchiveLimits, error) {
	probe := dst
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	available, entries, err := archiveFilesystemCapacity(probe)
	if err != nil {
		return resolvedArchiveLimits{}, fmt.Errorf("inspect archive destination capacity: %w", err)
	}
	if entries == 0 {
		entries = ^uint64(0)
	}
	limits := resolvedArchiveLimits{MaxEntries: entries, MaxFileBytes: available, MaxExpandedBytes: available}
	if requested != nil {
		if requested.MaxEntries > 0 {
			limits.MaxEntries = requested.MaxEntries
		}
		if requested.MaxFileBytes < 0 || requested.MaxExpandedBytes < 0 {
			return resolvedArchiveLimits{}, fmt.Errorf("archive byte limits cannot be negative")
		}
		if requested.MaxFileBytes > 0 {
			limits.MaxFileBytes = uint64(requested.MaxFileBytes)
		}
		if requested.MaxExpandedBytes > 0 {
			limits.MaxExpandedBytes = uint64(requested.MaxExpandedBytes)
		}
	}
	return limits, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func ensureTarTargetCompatible(target string, incomingDir bool) error {
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: target is a symlink: %s", ErrUnsafeTarExtractionPath, target)
	}
	existingDir := info.IsDir()
	switch {
	case incomingDir && !existingDir:
		return fmt.Errorf("copy conflict at %s: cannot overwrite non-directory with directory", target)
	case !incomingDir && existingDir:
		return fmt.Errorf("copy conflict at %s: cannot overwrite directory with non-directory", target)
	default:
		return nil
	}
}

func ensureTarParents(root, parent string, owner *ArchiveOwnership) error {
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: target parent escapes destination: %s", ErrUnsafeTarExtractionPath, parent)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: target parent is a symlink: %s", ErrUnsafeTarExtractionPath, root)
	}
	if !info.IsDir() {
		return fmt.Errorf("tar target parent is not a directory: %s", root)
	}
	if rel == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return err
			}
			if err := applyArchiveOwnership(current, false, owner); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: target parent is a symlink: %s", ErrUnsafeTarExtractionPath, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("tar target parent is not a directory: %s", current)
		}
	}
	return nil
}

func nearestExistingTarParent(parent string) (string, error) {
	current := filepath.Clean(parent)
	for {
		if _, err := os.Lstat(current); err == nil {
			return current, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(current)
		if next == current {
			return "", fmt.Errorf("no existing parent for tar target %s", parent)
		}
		current = next
	}
}

func validateTarSymlinkTarget(root, target, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("%w: absolute symlink target %q", ErrUnsafeTarExtractionPath, linkname)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(linkname)))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: symlink target escapes destination: %q", ErrUnsafeTarExtractionPath, linkname)
	}
	return nil
}

func tarTarget(dst string, dstDir bool, name string) (string, error) {
	cleanName := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(name, "/")))
	if cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	if dstDir {
		return filepath.Join(dst, cleanName), nil
	}
	parts := strings.SplitN(cleanName, string(filepath.Separator), 2)
	if len(parts) == 1 {
		return dst, nil
	}
	return filepath.Join(dst, parts[1]), nil
}

type tarDirMtime struct {
	path  string
	mtime time.Time
}

func restoreTarDirMtimes(dirs []tarDirMtime) error {
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		if dir.mtime.IsZero() {
			continue
		}
		if err := os.Chtimes(dir.path, dir.mtime, dir.mtime); err != nil {
			return err
		}
	}
	return nil
}

func copyMarked(wg *sync.WaitGroup, control io.Writer, marker, id string, r io.Reader) {
	defer wg.Done()
	var buf [4096]byte
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			WriteProtocolBytes(control, marker, id, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func copyPTYStdin(wg *sync.WaitGroup, stdin io.ReadCloser, ptyMaster *os.File) {
	defer wg.Done()
	defer stdin.Close()
	var buf [4096]byte
	for {
		n, err := stdin.Read(buf[:])
		if n > 0 {
			if _, writeErr := ptyMaster.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				_, _ = ptyMaster.Write([]byte{4})
			}
			return
		}
	}
}

func writeErr(opts Options, control io.Writer, id string, err error) {
	DefaultProtocol().WriteStderr(control, id, []byte("ccx3-"+opts.Name+"-init: "+err.Error()+"\n"))
}

func WriteProtocolBytes(control io.Writer, marker, id string, data []byte) {
	if marker == "" || id == "" || len(data) == 0 {
		return
	}
	WriteProtocolLine(control, marker+id+":"+base64.StdEncoding.EncodeToString(data))
}

var protocolMu sync.Mutex

func WriteProtocolLine(w io.Writer, line string) {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	_, _ = io.WriteString(w, strings.TrimRight(line, "\n")+"\n")
}

func writeConsole(s string) {
	if console, err := os.OpenFile("/dev/console", os.O_RDWR, 0); err == nil {
		defer console.Close()
		_, _ = console.WriteString(s)
		return
	}
	_, _ = os.Stderr.WriteString(s)
}

func rootPath(rootDir, name string) string {
	cleanRoot := filepath.Clean("/" + strings.TrimPrefix(rootDir, "/"))
	if cleanRoot == "/" {
		cleanRoot = ""
	}
	cleanName := filepath.Clean("/" + strings.TrimPrefix(name, "/"))
	return filepath.Join(cleanRoot, cleanName)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parseSignal(name string) (syscall.Signal, error) {
	return ParseSignal(name)
}

func ParseSignal(name string) (syscall.Signal, error) {
	switch strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(name), "SIG")) {
	case "HUP":
		return syscall.SIGHUP, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "", "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "WINCH":
		return syscall.SIGWINCH, nil
	default:
		return 0, fmt.Errorf("unsupported signal %q", name)
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
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
