//go:build !windows

package capturerelay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const captureFinishPrefix = "\x1dvmsh-capture-finish:"
const captureFinishSuffix = "\x1f\n"

// Run serves a private command-capture control FIFO. One process owns every
// capture in a persistent shell context; retired streams keep draining guest
// writers without retaining their spool files or a process per stream.
func Run(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: capture-relay CONTROL_FIFO SHELL_CONTROL MAX_STORED")
	}
	maxStored, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || maxStored < 0 {
		return fmt.Errorf("invalid maximum stored size %q", args[2])
	}
	return newRelay(args[0], args[1], maxStored).run()
}

type relay struct {
	controlPath string
	shellPath   string
	maxStored   int64
	captures    map[string]*capture
	controlBuf  []byte
	pollFDs     []int
	pollReady   []bool
	pollCapture []*capture
	pollDirty   bool
	poller      relayPoller
}

type capture struct {
	owner      *relay
	account    string
	outputPath string
	fifoPath   string
	closedPath string
	overflow   string
	retired    string
	registered string
	finished   string

	spool         *os.File
	reader        *os.File
	total         int64
	stored        int64
	primaryDone   bool
	isRetired     bool
	overflowed    bool
	finishMarker  []byte
	finishPending []byte
}

func newRelay(controlPath, shellPath string, maxStored int64) *relay {
	return &relay{controlPath: controlPath, shellPath: shellPath, maxStored: maxStored, captures: make(map[string]*capture), pollDirty: true}
}

func (r *relay) run() error {
	_ = os.Remove(r.controlPath)
	if err := syscall.Mkfifo(r.controlPath, 0o600); err != nil {
		return fmt.Errorf("create capture control fifo: %w", err)
	}
	defer os.Remove(r.controlPath)
	if info, err := os.Stat(filepath.Dir(r.controlPath)); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid := int(stat.Uid), int(stat.Gid)
			if err := os.Chown(r.controlPath, uid, gid); err != nil {
				return fmt.Errorf("assign capture control fifo: %w", err)
			}
			if os.Geteuid() == 0 && (uid != 0 || gid != 0) {
				if err := syscall.Setgroups([]int{gid}); err != nil {
					return fmt.Errorf("set capture relay groups: %w", err)
				}
				if err := syscall.Setgid(gid); err != nil {
					return fmt.Errorf("set capture relay gid: %w", err)
				}
				if err := syscall.Setuid(uid); err != nil {
					return fmt.Errorf("set capture relay uid: %w", err)
				}
			}
		}
	}
	control, err := os.OpenFile(r.controlPath, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open capture control fifo: %w", err)
	}
	defer control.Close()
	readyPath := r.controlPath + ".ready"
	if err := os.WriteFile(readyPath, []byte("1\n"), 0o600); err != nil {
		return fmt.Errorf("publish capture relay readiness: %w", err)
	}
	defer os.Remove(readyPath)
	fmt.Println("vmsh-capture-relay-ready")
	defer r.closeCaptures()
	return r.serve(control)
}

func (r *relay) serve(control *os.File) error {
	defer r.poller.close()
	for {
		if r.pollDirty {
			if err := r.rebuildPollSet(control); err != nil {
				return err
			}
		}
		if err := r.poller.wait(r.pollReady); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("wait for capture relay input: %w", err)
		}
		if r.pollReady[0] {
			stop, err := r.readControl(control)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
		for i, capture := range r.pollCapture {
			if !r.pollReady[i+1] {
				continue
			}
			complete, err := capture.drainReady()
			if err != nil {
				return fmt.Errorf("drain capture %s: %w", capture.account, err)
			}
			if complete {
				capture.complete()
				r.remove(capture)
			}
		}
	}
}

func (r *relay) rebuildPollSet(control *os.File) error {
	r.pollFDs = append(r.pollFDs[:0], int(control.Fd()))
	r.pollCapture = r.pollCapture[:0]
	for _, capture := range r.captures {
		if capture.reader == nil {
			continue
		}
		r.pollCapture = append(r.pollCapture, capture)
		r.pollFDs = append(r.pollFDs, int(capture.reader.Fd()))
	}
	if cap(r.pollReady) < len(r.pollFDs) {
		r.pollReady = make([]bool, len(r.pollFDs))
	} else {
		r.pollReady = r.pollReady[:len(r.pollFDs)]
	}
	if err := r.poller.reset(r.pollFDs); err != nil {
		return fmt.Errorf("configure capture relay readiness: %w", err)
	}
	r.pollDirty = false
	return nil
}

func (r *relay) readControl(control *os.File) (bool, error) {
	var buf [32 << 10]byte
	for {
		n, err := unix.Read(int(control.Fd()), buf[:])
		if n > 0 {
			r.controlBuf = append(r.controlBuf, buf[:n]...)
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				break
			}
			return false, fmt.Errorf("read capture control: %w", err)
		}
		if n == 0 {
			break
		}
	}
	for {
		newline := bytes.IndexByte(r.controlBuf, '\n')
		if newline < 0 {
			break
		}
		line := strings.TrimSuffix(string(r.controlBuf[:newline]), "\r")
		r.controlBuf = append(r.controlBuf[:0], r.controlBuf[newline+1:]...)
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "register":
			if len(fields) != 9 {
				return false, fmt.Errorf("invalid capture registration")
			}
			if err := r.register(fields[1:]); err != nil {
				return false, err
			}
		case "finish":
			if len(fields) != 2 {
				return false, fmt.Errorf("invalid capture finish")
			}
			if capture := r.capture(fields[1]); capture != nil {
				if err := capture.finishPrimary(); err != nil {
					return false, err
				}
			}
		case "retire":
			if len(fields) != 2 {
				return false, fmt.Errorf("invalid capture retirement")
			}
			if capture := r.capture(fields[1]); capture != nil {
				if err := capture.retire(); err != nil {
					return false, err
				}
			}
		case "stop":
			return true, nil
		default:
			return false, fmt.Errorf("unknown capture relay operation %q", fields[0])
		}
	}
	return false, nil
}

func (r *relay) register(fields []string) error {
	c := &capture{
		owner: r, account: fields[0], outputPath: fields[1], fifoPath: fields[2],
		closedPath: fields[3], overflow: fields[4], retired: fields[5], registered: fields[6], finished: fields[7],
	}
	c.finishMarker = []byte(captureFinishPrefix + c.account + captureFinishSuffix)
	var err error
	c.spool, err = os.OpenFile(c.outputPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err == nil {
		// Start connected so poll/kqueue never latch an EOF before the command
		// opens its end. finishPrimary replaces this with a read-only descriptor,
		// preserving real EOF detection without retaining a second FIFO handle.
		c.reader, err = os.OpenFile(c.fifoPath, os.O_RDWR|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		if c.spool != nil {
			_ = c.spool.Close()
		}
		if c.reader != nil {
			_ = c.reader.Close()
		}
		if publishErr := writeExisting(c.registered, "error: "+err.Error()+"\n"); publishErr != nil {
			return fmt.Errorf("register capture %s: %w (publish failure: %v)", c.account, err, publishErr)
		}
		return nil
	}
	if _, exists := r.captures[c.account]; exists {
		_ = c.spool.Close()
		_ = c.reader.Close()
		if err := writeExisting(c.registered, "error: duplicate account\n"); err != nil {
			return fmt.Errorf("publish duplicate capture registration %s: %w", c.account, err)
		}
		return nil
	}
	r.captures[c.account] = c
	r.pollDirty = true
	if err := writeExisting(c.registered, "ok\n"); err != nil {
		c.close()
		delete(r.captures, c.account)
		r.pollDirty = true
		return fmt.Errorf("publish capture registration %s: %w", c.account, err)
	}
	return nil
}

func (r *relay) capture(account string) *capture {
	return r.captures[account]
}

func (r *relay) remove(c *capture) {
	if r.captures[c.account] == c {
		delete(r.captures, c.account)
		r.pollDirty = true
	}
}

func (r *relay) closeCaptures() {
	for _, capture := range r.captures {
		capture.close()
	}
	clear(r.captures)
}

func (c *capture) drainReady() (bool, error) {
	var buf [32 << 10]byte
	for {
		n, err := unix.Read(int(c.reader.Fd()), buf[:])
		if n > 0 {
			if err := c.consume(buf[:n]); err != nil {
				return false, err
			}
			continue
		}
		if n == 0 && err == nil {
			return c.primaryDone, nil
		}
		if err == nil || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		if errors.Is(err, io.EOF) {
			return c.primaryDone, nil
		}
		return false, err
	}
}

func (c *capture) consume(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	out := make([]byte, 0, len(data))
	for _, value := range data {
		if c.primaryDone {
			out = append(out, value)
			continue
		}
		c.finishPending = append(c.finishPending, value)
		for len(c.finishPending) > 0 && !bytes.HasPrefix(c.finishMarker, c.finishPending) {
			out = append(out, c.finishPending[0])
			c.finishPending = c.finishPending[1:]
		}
		if bytes.Equal(c.finishPending, c.finishMarker) {
			c.finishPending = nil
			// Publish completion only after every preceding byte from this FIFO
			// has reached its spool. The guest waits on this acknowledgement
			// before publishing the command status on the independent control
			// stream.
			c.consumeData(out)
			out = out[:0]
			if err := c.finishPrimary(); err != nil {
				return err
			}
		}
	}
	c.consumeData(out)
	return nil
}

func (c *capture) consumeData(data []byte) {
	if len(data) == 0 {
		return
	}
	c.total += int64(len(data))
	if c.isRetired || c.spool == nil || c.stored >= c.owner.maxStored {
		c.markOverflowLocked()
		return
	}
	writable := int64(len(data))
	if remaining := c.owner.maxStored - c.stored; writable > remaining {
		writable = remaining
	}
	if writable > 0 {
		n, err := c.spool.Write(data[:writable])
		c.stored += int64(n)
		if err != nil || int64(n) != writable {
			c.markOverflowLocked()
			_ = c.spool.Close()
			c.spool = nil
		}
	}
	if writable != int64(len(data)) {
		c.markOverflowLocked()
	}
}

func (c *capture) markOverflowLocked() {
	if c.overflowed {
		return
	}
	c.overflowed = true
	_ = writeExisting(c.overflow, "1\n")
}

func (c *capture) finishPrimary() error {
	if c.primaryDone {
		return nil
	}
	reader, err := os.OpenFile(c.fifoPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("release capture FIFO writer %s: %w", c.account, err)
	}
	previous := c.reader
	c.reader = reader
	c.owner.pollDirty = true
	if previous != nil {
		_ = previous.Close()
	}
	if err := writeExisting(c.finished, "1\n"); err != nil {
		return fmt.Errorf("publish capture finish %s: %w", c.account, err)
	}
	c.primaryDone = true
	return nil
}

func (c *capture) retire() error {
	if !c.isRetired {
		c.isRetired = true
		if c.spool != nil {
			_ = c.spool.Close()
			c.spool = nil
		}
	}
	if err := writeExisting(c.retired, "1\n"); err != nil {
		return fmt.Errorf("publish capture retirement %s: %w", c.account, err)
	}
	return nil
}

func (c *capture) complete() {
	if len(c.finishPending) != 0 {
		pending := append([]byte(nil), c.finishPending...)
		c.finishPending = nil
		c.consumeData(pending)
	}
	if c.spool != nil {
		_ = c.spool.Close()
		c.spool = nil
	}
	total := c.total
	retired := c.isRetired
	c.close()
	if !retired {
		_ = writeExisting(c.closedPath, "1\n")
	}
	control, err := os.OpenFile(c.owner.shellPath, os.O_WRONLY, 0)
	if err == nil {
		_, _ = fmt.Fprintf(control, "\x1dvmsh-capture:%s:%d\x1f\n", c.account, total)
		_ = control.Close()
	}
}

func (c *capture) close() {
	if c.spool != nil {
		_ = c.spool.Close()
		c.spool = nil
	}
	if c.reader != nil {
		_ = c.reader.Close()
		c.reader = nil
	}
}

func writeExisting(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, value)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}
