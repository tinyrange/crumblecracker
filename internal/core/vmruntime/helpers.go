package vmruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const (
	InstanceReadyMarker   = protocol.ReadyMarker
	InitDurationMarker    = "__CCX3_INIT_MS__:"
	ExecTimingMarker      = protocol.TimingMarkerPrefix
	CommandBeginMarker    = protocol.BeginMarkerPrefix
	CommandOutputMarker   = protocol.OutputMarkerPrefix
	CommandErrorMarker    = protocol.ErrorMarkerPrefix
	CommandControlMarker  = protocol.ControlMarkerPrefix
	CommandUsageMarker    = protocol.UsageMarkerPrefix
	CommandExitMarkerPref = protocol.ExitMarkerPrefix
)

type SerialTranscript struct {
	mu         sync.Mutex
	buf        []byte
	file       *os.File
	path       string
	base       int
	fileBase   int
	size       int
	tail       []byte
	readers    map[uint64]int
	nextReader uint64
	closed     bool
	spillErr   error
	reclaimErr error
	staleFiles []*os.File
	stalePaths []string
	segments   []serialTranscriptSegment
}

type serialTranscriptSegment struct {
	file *os.File
	path string
	base int
	end  int
}

const (
	serialTranscriptMemoryBytes  = 1 << 20
	serialTranscriptReadBytes    = 256 << 10
	serialTranscriptTailBytes    = 64 << 10
	serialTranscriptWaitBytes    = 1 << 20
	serialTranscriptReclaimBytes = 8 << 20
	serialTranscriptSegmentBytes = 8 << 20
	// The legacy one-shot APIs must return a contiguous Go string. Commands are
	// unrestricted and remain spill-backed while running, but materializing a
	// larger compatibility response risks killing the host before Go can return
	// an allocation error. Streaming exec has no such response boundary.
	serialTranscriptCompatibilityBytes = 64 << 20
)

var ErrCommandOutputRequiresStreaming = errors.New("command output requires the streaming exec API")

type TranscriptReader interface {
	Advance(int)
	Close()
}

type serialTranscriptReader struct {
	transcript *SerialTranscript
	id         uint64
	once       sync.Once
}

func NewSerialTranscript() *SerialTranscript {
	s := &SerialTranscript{readers: make(map[uint64]int)}
	runtime.SetFinalizer(s, (*SerialTranscript).finalize)
	return s
}

func (s *SerialTranscript) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	if s.file == nil && len(s.buf)+len(data) <= serialTranscriptMemoryBytes {
		s.buf = append(s.buf, data...)
		s.size += len(data)
		s.appendTailLocked(data)
		return len(data), nil
	}
	if s.file == nil {
		if err := s.openSpillLocked(); err != nil {
			s.spillErr = err
			return 0, err
		}
	}
	written := 0
	for len(data) != 0 {
		capacity := serialTranscriptSegmentBytes - (s.size - s.fileBase)
		if capacity == 0 {
			if err := s.rotateSpillLocked(); err != nil {
				s.spillErr = errors.Join(s.spillErr, err)
				return written, err
			}
			capacity = serialTranscriptSegmentBytes
		}
		count := min(len(data), capacity)
		n, err := s.file.WriteAt(data[:count], int64(s.size-s.fileBase))
		s.size += n
		written += n
		s.appendTailLocked(data[:n])
		data = data[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (s *SerialTranscript) openSpillLocked() error {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "cc", "transcripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "serial-*")
	if err != nil {
		return err
	}
	if len(s.buf) > 0 {
		if _, err := f.Write(s.buf); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return err
		}
	}
	s.file, s.path, s.buf, s.fileBase = f, f.Name(), nil, s.base
	if err := os.Remove(s.path); err == nil {
		s.path = ""
	}
	return nil
}

func (s *SerialTranscript) rotateSpillLocked() error {
	dir := filepath.Dir(s.file.Name())
	next, err := os.CreateTemp(dir, "serial-*")
	if err != nil {
		return err
	}
	path := next.Name()
	if err := os.Remove(path); err == nil {
		path = ""
	}
	old := serialTranscriptSegment{file: s.file, path: s.path, base: s.fileBase, end: s.size}
	if old.end <= s.base {
		s.cleanupTranscriptSegmentLocked(old)
	} else {
		s.segments = append(s.segments, old)
	}
	s.file, s.path, s.fileBase = next, path, s.size
	return nil
}

func (s *SerialTranscript) cleanupTranscriptSegmentLocked(segment serialTranscriptSegment) {
	if segment.file != nil {
		if err := segment.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			s.reclaimErr = errors.Join(s.reclaimErr, err)
			s.staleFiles = append(s.staleFiles, segment.file)
			if segment.path != "" {
				s.stalePaths = append(s.stalePaths, segment.path)
			}
			return
		}
	}
	if segment.path != "" {
		if err := os.Remove(segment.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.reclaimErr = errors.Join(s.reclaimErr, err)
			s.stalePaths = append(s.stalePaths, segment.path)
		}
	}
}

func (s *SerialTranscript) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

func (s *SerialTranscript) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tail) == s.size {
		return string(s.tail)
	}
	return fmt.Sprintf("[... %d earlier transcript bytes omitted ...]\n%s", s.size-len(s.tail), s.tail)
}

func (s *SerialTranscript) appendTailLocked(data []byte) {
	if len(data) >= serialTranscriptTailBytes {
		s.tail = append(s.tail[:0], data[len(data)-serialTranscriptTailBytes:]...)
		return
	}
	if drop := len(s.tail) + len(data) - serialTranscriptTailBytes; drop > 0 {
		copy(s.tail, s.tail[drop:])
		s.tail = s.tail[:len(s.tail)-drop]
	}
	s.tail = append(s.tail, data...)
}

// RetainFrom registers a live reader. Releasing it permits transcript storage
// older than every remaining reader to be reclaimed. Offsets remain absolute,
// so concurrent commands can finish in any order without invalidating cursors.
func (s *SerialTranscript) RetainFrom(offset int) func() {
	reader := s.RetainReader(offset)
	return reader.Close
}

// RetainReader registers a movable reader cursor. Streaming parsers advance
// it after copying bytes into their own small framing buffer, which prevents a
// quiet, long-lived command from pinning unrelated later output.
func (s *SerialTranscript) RetainReader(offset int) TranscriptReader {
	if s == nil {
		return &serialTranscriptReader{}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &serialTranscriptReader{}
	}
	if offset < s.base {
		offset = s.base
	}
	s.nextReader++
	id := s.nextReader
	s.readers[id] = offset
	s.mu.Unlock()
	return &serialTranscriptReader{transcript: s, id: id}
}

func (r *serialTranscriptReader) Advance(offset int) {
	if r == nil || r.transcript == nil {
		return
	}
	s := r.transcript
	s.mu.Lock()
	if current, ok := s.readers[r.id]; ok && offset > current {
		if offset > s.size {
			offset = s.size
		}
		s.readers[r.id] = offset
		s.discardToReadersLocked()
	}
	s.mu.Unlock()
}

func (r *serialTranscriptReader) Close() {
	if r == nil || r.transcript == nil {
		return
	}
	r.once.Do(func() {
		s := r.transcript
		s.mu.Lock()
		delete(s.readers, r.id)
		s.discardToReadersLocked()
		s.mu.Unlock()
	})
}

func (s *SerialTranscript) discardToReadersLocked() {
	target := s.size
	for _, readerOffset := range s.readers {
		if readerOffset < target {
			target = readerOffset
		}
	}
	s.discardBeforeLocked(target)
}

func (s *SerialTranscript) discardBeforeLocked(offset int) {
	if offset <= s.base {
		return
	}
	if offset > s.size {
		offset = s.size
	}
	if s.file == nil {
		drop := offset - s.base
		if drop >= len(s.buf) {
			s.buf = nil
		} else {
			s.buf = s.buf[drop:]
		}
		s.base = offset
		return
	}
	kept := s.segments[:0]
	for _, segment := range s.segments {
		if segment.end <= offset {
			s.cleanupTranscriptSegmentLocked(segment)
		} else {
			kept = append(kept, segment)
		}
	}
	s.segments = kept
	if offset == s.size {
		s.base = offset
		if offset-s.fileBase < serialTranscriptReclaimBytes {
			return
		}
		if err := s.file.Truncate(0); err != nil {
			s.spillErr = errors.Join(s.spillErr, err)
			return
		}
		s.fileBase = offset
		return
	}
	s.base = offset
}

// ReadFrom returns only transcript bytes at and after offset. Callers which
// follow a live serial stream must not repeatedly materialize the entire boot
// transcript: guest output can be arbitrarily large, and doing so lets bulk
// output delay unrelated command lifecycle and cancellation events.
func (s *SerialTranscript) ReadFrom(offset int) (string, int) {
	text, next, _ := s.readFrom(offset)
	return text, next
}

func (s *SerialTranscript) readFrom(offset int) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset < 0 {
		offset = 0
	}
	if offset < s.base {
		offset = s.base
	}
	if offset > s.size {
		offset = s.size
	}
	end := min(s.size, offset+serialTranscriptReadBytes)
	data, err := s.readLocked(offset, end)
	return string(data), offset + len(data), err
}

func (s *SerialTranscript) readLocked(start, end int) ([]byte, error) {
	if s.closed {
		return nil, os.ErrClosed
	}
	if start < 0 {
		start = 0
	}
	if start < s.base {
		start = s.base
	}
	if end > s.size {
		end = s.size
	}
	if start >= end {
		return nil, nil
	}
	if s.file == nil {
		return append([]byte(nil), s.buf[start-s.base:end-s.base]...), s.spillErr
	}
	data := make([]byte, 0, end-start)
	position := start
	segments := append([]serialTranscriptSegment(nil), s.segments...)
	segments = append(segments, serialTranscriptSegment{file: s.file, base: s.fileBase, end: s.size})
	var readErr error
	for _, segment := range segments {
		if position >= end {
			break
		}
		if position >= segment.end || end <= segment.base {
			continue
		}
		segmentStart := max(position, segment.base)
		segmentEnd := min(end, segment.end)
		chunk := make([]byte, segmentEnd-segmentStart)
		n, err := segment.file.ReadAt(chunk, int64(segmentStart-segment.base))
		if errors.Is(err, io.EOF) && n == len(chunk) {
			err = nil
		}
		data = append(data, chunk[:n]...)
		position = segmentStart + n
		if err != nil {
			readErr = errors.Join(readErr, err)
			break
		}
	}
	if position < end && readErr == nil {
		readErr = io.ErrUnexpectedEOF
	}
	return data, errors.Join(s.spillErr, readErr)
}

func (s *SerialTranscript) WaitFor(ctx context.Context, start int, predicate func(string) bool) (string, error) {
	return s.waitFor(ctx, start, "", false, predicate)
}

// WaitForCommand permits complete compatibility output to be materialized
// only after this command's terminal record is present. An unrelated command
// completing on the shared control transcript must not repeatedly read a
// large active command back from disk.
func (s *SerialTranscript) WaitForCommand(ctx context.Context, start int, id string, predicate func(string) bool) (string, error) {
	return s.waitFor(ctx, start, id, false, predicate)
}

// WaitForCommandEvent waits on the requested command's filtered protocol
// records before command exit. It is intended for bounded lifecycle events
// such as input readiness; complete output waits should use WaitForCommand.
func (s *SerialTranscript) WaitForCommandEvent(ctx context.Context, start int, id string, predicate func(string) bool) (string, error) {
	return s.waitFor(ctx, start, id, true, predicate)
}

func (s *SerialTranscript) waitFor(ctx context.Context, start int, commandID string, beforeExit bool, predicate func(string) bool) (string, error) {
	reader := s.RetainReader(start)
	defer reader.Close()
	var window []byte
	var partialLine []byte
	var commandRecords *SerialTranscript
	if commandID != "" {
		commandRecords = NewSerialTranscript()
		defer commandRecords.Close()
	}
	terminalDirty := false
	offset := start
	dirty := false
	for {
		text, next, readErr := s.readFrom(offset)
		if readErr != nil {
			return "", readErr
		}
		if next > offset {
			if commandID != "" {
				var terminal bool
				var err error
				partialLine, terminal, err = appendCommandRecords(commandRecords, partialLine, []byte(text), commandID)
				if err != nil {
					return "", err
				}
				terminalDirty = terminalDirty || terminal
			}
			window = append(window, text...)
			if len(window) > serialTranscriptWaitBytes {
				copy(window, window[len(window)-serialTranscriptWaitBytes:])
				window = window[:serialTranscriptWaitBytes]
			}
			offset = next
			reader.Advance(offset)
			dirty = true
			// Generic waits are used for boot ready/fatal markers. Evaluate the
			// bounded rolling window as it advances so continuous later output
			// cannot evict a marker before the producer becomes quiescent.
			if commandID == "" && predicate(string(window)) {
				return string(window), nil
			}
			continue
		} else {
			s.mu.Lock()
			err := s.spillErr
			closed := s.closed
			s.mu.Unlock()
			if err != nil {
				return "", err
			}
			if closed {
				return "", os.ErrClosed
			}
		}

		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Debounce the predicate until the producer is momentarily quiescent.
		// Managed stdout arrives as many small protocol lines; reparsing the
		// complete command after every line is otherwise quadratic even though
		// the backing transcript itself is streamed.
		time.Sleep(5 * time.Millisecond)
		if s.Len() > offset {
			continue
		}
		if dirty {
			candidate := string(window)
			if commandID != "" {
				if record, terminal := commandRecord(partialLine, commandID); len(record) != 0 && terminal {
					terminalDirty = true
				}
				if !terminalDirty && !beforeExit {
					dirty = false
					continue
				}
				var err error
				candidate, err = commandRecords.materializeString()
				if err != nil {
					return "", err
				}
				if record, _ := commandRecord(partialLine, commandID); len(record) != 0 {
					candidate += string(record)
				}
				terminalDirty = false
			}
			if predicate(candidate) {
				return candidate, nil
			}
		}
		dirty = false
	}
}

func appendCommandRecords(records io.Writer, partial, data []byte, id string) ([]byte, bool, error) {
	partial = append(partial, data...)
	terminalSeen := false
	for {
		newline := bytes.IndexByte(partial, '\n')
		if newline < 0 {
			return partial, terminalSeen, nil
		}
		line := partial[:newline]
		if record, terminal := commandRecord(line, id); len(record) != 0 {
			if _, err := records.Write(record); err != nil {
				return partial, terminalSeen, err
			}
			if _, err := records.Write([]byte{'\n'}); err != nil {
				return partial, terminalSeen, err
			}
			terminalSeen = terminalSeen || terminal
		}
		partial = partial[newline+1:]
	}
}

func commandRecord(line []byte, id string) ([]byte, bool) {
	prefixes := []string{
		CommandBeginMarker + id,
		CommandOutputMarker + id + ":",
		CommandErrorMarker + id + ":",
		CommandControlMarker + id + ":",
		CommandUsageMarker + id + ":",
		ExecTimingMarker + id + ":",
		CommandExitMarkerPref + id + ":",
	}
	for _, prefix := range prefixes {
		if index := bytes.Index(line, []byte(prefix)); index >= 0 {
			return line[index:], prefix == CommandExitMarkerPref+id+":"
		}
	}
	return nil, false
}

func (s *SerialTranscript) materializeString() (string, error) {
	return s.materializeStringLimit(serialTranscriptCompatibilityBytes)
}

func (s *SerialTranscript) materializeStringLimit(limit int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size := s.size - s.base
	if limit >= 0 && size > limit {
		return "", fmt.Errorf("%w: spill-backed compatibility transcript is %d bytes (one-shot limit %d bytes)", ErrCommandOutputRequiresStreaming, size, limit)
	}
	var out strings.Builder
	out.Grow(size)
	for offset := s.base; offset < s.size; {
		next := min(s.size, offset+serialTranscriptReadBytes)
		data, err := s.readLocked(offset, next)
		if err != nil {
			return "", err
		}
		if len(data) == 0 {
			break
		}
		_, _ = out.Write(data)
		offset += len(data)
	}
	return out.String(), nil
}

func (s *SerialTranscript) Close() error {
	if s == nil {
		return nil
	}
	runtime.SetFinalizer(s, nil)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	f, path, segments, staleFiles, stalePaths := s.file, s.path, s.segments, s.staleFiles, s.stalePaths
	s.file, s.path, s.buf, s.tail, s.readers, s.size = nil, "", nil, nil, nil, 0
	s.segments, s.staleFiles, s.stalePaths = nil, nil, nil
	s.mu.Unlock()
	var errs []error
	if f != nil {
		errs = append(errs, f.Close())
	}
	for _, segment := range segments {
		if segment.file != nil {
			errs = append(errs, segment.file.Close())
		}
		if segment.path != "" {
			if err := os.Remove(segment.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	for _, stale := range staleFiles {
		if stale != nil {
			errs = append(errs, stale.Close())
		}
	}
	if path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, stalePath := range stalePaths {
		if err := os.Remove(stalePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *SerialTranscript) finalize() {
	_ = s.Close()
}

type BootEventWriter struct {
	ch       chan string
	done     chan struct{}
	callback func(client.BootEvent) error
	closeMu  sync.Mutex
	closed   bool
	errMu    sync.Mutex
	err      error
}

func NewBootEventWriter(callback func(client.BootEvent) error) *BootEventWriter {
	w := &BootEventWriter{
		ch:       make(chan string, 128),
		done:     make(chan struct{}),
		callback: callback,
	}
	go func() {
		defer close(w.done)
		for chunk := range w.ch {
			if w.callback == nil || chunk == "" {
				continue
			}
			if err := w.callback(client.BootEvent{Kind: "serial", Data: chunk}); err != nil {
				w.errMu.Lock()
				if w.err == nil {
					w.err = err
				}
				w.errMu.Unlock()
				return
			}
		}
	}()
	return w
}

func (w *BootEventWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	w.closeMu.Lock()
	if w.closed {
		w.closeMu.Unlock()
		return len(data), nil
	}
	select {
	case w.ch <- string(append([]byte(nil), data...)):
	default:
	}
	w.closeMu.Unlock()
	return len(data), nil
}

func (w *BootEventWriter) Close() error {
	w.closeMu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.closeMu.Unlock()
	<-w.done
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}

func HasFatalBootText(text string) bool {
	fatalMarkers := []string{
		"ccx3-init-fatal:",
		"Kernel panic",
		"kernel panic",
		"panic: ",
		"not syncing",
		"reboot: System halted",
	}
	for _, marker := range fatalMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func ParseInitDurationMarker(text string) (int, bool) {
	idx := strings.LastIndex(text, InitDurationMarker)
	if idx < 0 {
		return 0, false
	}
	rest := text[idx+len(InitDurationMarker):]
	end := strings.IndexByte(rest, '\n')
	if end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return 0, false
	}
	ms, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return ms, true
}
