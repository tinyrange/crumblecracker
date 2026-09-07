package vmruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestExtractManagedExecResultCollectsOutputStderrAndUsage(t *testing.T) {
	usageJSON, err := json.Marshal(client.ResourceUsage{WallSeconds: 1.5, MaxRSSBytes: 4096})
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	serial := strings.Join([]string{
		"noise before",
		CommandBeginMarker + "42",
		CommandOutputMarker + "42:" + b64("hello "),
		CommandErrorMarker + "42:" + b64("there\n"),
		CommandUsageMarker + "42:" + base64.StdEncoding.EncodeToString(usageJSON),
		CommandExitMarkerPref + "42:7",
		"noise after",
	}, "\n")

	code, output, usage, ok := ExtractManagedExecResult(serial, "42", false)
	if !ok {
		t.Fatalf("result was not found")
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if output != "hello there" {
		t.Fatalf("output = %q, want trimmed combined stdout/stderr", output)
	}
	if usage == nil || usage.WallSeconds != 1.5 || usage.MaxRSSBytes != 4096 {
		t.Fatalf("usage = %+v, want parsed usage", usage)
	}
}

func TestExtractManagedExecResultDmesgReturnsTranscriptSegment(t *testing.T) {
	serial := strings.Join([]string{
		"kernel line",
		CommandBeginMarker + "abc",
		CommandOutputMarker + "abc:" + b64("guest output\n"),
		CommandExitMarkerPref + "abc:0",
	}, "\n")

	code, output, _, ok := ExtractManagedExecResult(serial, "abc", true)
	if !ok || code != 0 {
		t.Fatalf("result = (%d, %v), want successful result", code, ok)
	}
	wantOutput := strings.Join([]string{
		CommandBeginMarker + "abc",
		CommandOutputMarker + "abc:" + b64("guest output\n"),
		CommandExitMarkerPref + "abc:0",
		"guest output",
	}, "\n")
	if output != wantOutput {
		t.Fatalf("dmesg output = %q, want %q", output, wantOutput)
	}
}

func TestExtractManagedExecResultIgnoresMalformedMarkers(t *testing.T) {
	serial := strings.Join([]string{
		CommandBeginMarker + "1",
		CommandOutputMarker + "1:not-base64",
		CommandUsageMarker + "1:" + b64("{not-json"),
		CommandExitMarkerPref + "1:not-an-int",
	}, "\n")
	if _, _, _, ok := ExtractManagedExecResult(serial, "1", false); ok {
		t.Fatalf("malformed exit marker unexpectedly produced a result")
	}
}

func TestParseManagedExecEventLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     string
		want     client.ExecEvent
		wantDone bool
		wantOK   bool
		wantErr  bool
	}{
		{
			name:   "stdout",
			line:   CommandOutputMarker + "9:" + b64("out"),
			want:   client.ExecEvent{Kind: "stdout", Stream: "stdout", Output: "out", Data: []byte("out")},
			wantOK: true,
		},
		{
			name:   "stderr",
			line:   CommandErrorMarker + "9:" + b64("err"),
			want:   client.ExecEvent{Kind: "stderr", Stream: "stderr", Output: "err", Data: []byte("err")},
			wantOK: true,
		},
		{
			name:   "control",
			line:   CommandControlMarker + "9:" + b64("done\t0\t/work\n"),
			want:   client.ExecEvent{Kind: "control", Output: "done\t0\t/work\n", Data: []byte("done\t0\t/work\n")},
			wantOK: true,
		},
		{
			name:     "exit",
			line:     CommandExitMarkerPref + "9:13",
			want:     client.ExecEvent{Kind: "exit", ExitCode: 13},
			wantDone: true,
			wantOK:   true,
		},
		{
			name: "begin",
			line: CommandBeginMarker + "9",
		},
		{
			name:    "bad exit",
			line:    CommandExitMarkerPref + "9:nope",
			wantErr: true,
		},
		{
			name: "other id",
			line: CommandOutputMarker + "8:" + b64("ignored"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, done, ok, err := ParseManagedExecEventLine(tc.line, "9")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse event: %v", err)
			}
			if done != tc.wantDone || ok != tc.wantOK {
				t.Fatalf("done/ok = %v/%v, want %v/%v", done, ok, tc.wantDone, tc.wantOK)
			}
			if got.Kind != tc.want.Kind || got.Stream != tc.want.Stream || got.Output != tc.want.Output || got.ExitCode != tc.want.ExitCode || string(got.Data) != string(tc.want.Data) {
				t.Fatalf("event = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSerialTranscriptWaitFor(t *testing.T) {
	transcript := NewSerialTranscript()
	start := transcript.Len()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		text, err := transcript.WaitFor(ctx, start, func(text string) bool {
			return text == "boot ready\n"
		})
		if err == nil && text != "boot ready\n" {
			err = errors.New("unexpected transcript segment: " + text)
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := transcript.Write([]byte("boot ready\n")); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("wait for transcript: %v", err)
	}
}

func TestSerialTranscriptWaitForCommandIgnoresUnrelatedExitForMaterialization(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	start := transcript.Len()
	var maxPredicateBytes atomic.Int64
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		text, err := transcript.WaitForCommand(ctx, start, "wanted", func(text string) bool {
			for size := int64(len(text)); ; {
				previous := maxPredicateBytes.Load()
				if size <= previous || maxPredicateBytes.CompareAndSwap(previous, size) {
					break
				}
			}
			return strings.Contains(text, CommandBeginMarker+"wanted") && strings.Contains(text, CommandExitMarkerPref+"wanted:")
		})
		if err == nil && (!strings.Contains(text, CommandBeginMarker+"wanted") || !strings.Contains(text, CommandExitMarkerPref+"wanted:")) {
			err = fmt.Errorf("completed command records = %q", text)
		}
		done <- err
	}()
	if _, err := transcript.Write([]byte(CommandBeginMarker + "wanted\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Write(bytes.Repeat([]byte("unrelated output\n"), (2<<20)/17)); err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Write([]byte(CommandExitMarkerPref + "other:0\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := maxPredicateBytes.Load(); got > 256 {
		t.Fatalf("unrelated exit materialized %d bytes", got)
	}
	if _, err := transcript.Write([]byte(CommandExitMarkerPref + "wanted:0\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSerialTranscriptWaitForCommandEventSurvivesUnrelatedOutput(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		text, err := transcript.WaitForCommandEvent(ctx, 0, "wanted", func(text string) bool {
			return strings.Contains(text, ExecTimingMarker+"wanted:input_ready:")
		})
		if err == nil && !strings.Contains(text, CommandBeginMarker+"wanted") {
			err = fmt.Errorf("filtered command event omitted begin record: %q", text)
		}
		done <- err
	}()
	data := []byte(CommandBeginMarker + "wanted\n" + ExecTimingMarker + "wanted:input_ready:1\n")
	data = append(data, bytes.Repeat([]byte("unrelated output\n"), (2<<20)/17)...)
	if _, err := transcript.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSerialTranscriptReclaimsConsumedSpillPrefix(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	payload := bytes.Repeat([]byte("0123456789abcdef"), (20<<20)/16)
	if _, err := transcript.Write(payload); err != nil {
		t.Fatal(err)
	}
	reader := transcript.RetainReader(0)
	defer reader.Close()
	reader.Advance(12 << 20)
	transcript.mu.Lock()
	file := transcript.file
	fileBase := transcript.fileBase
	segments := append([]serialTranscriptSegment(nil), transcript.segments...)
	transcript.mu.Unlock()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fileBase != 16<<20 || info.Size() != 4<<20 || len(segments) != 1 || segments[0].base != 8<<20 || segments[0].end != 16<<20 {
		t.Fatalf("segmented transcript file base=%d size=%d segments=%+v", fileBase, info.Size(), segments)
	}
	text, next := transcript.ReadFrom(12 << 20)
	if next <= 12<<20 || text != string(payload[12<<20:12<<20+len(text)]) {
		t.Fatal("absolute cursor did not survive transcript compaction")
	}
}

func TestSerialTranscriptSegmentsReclaimContinuousConsumedOutputWithoutCopy(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	payload := bytes.Repeat([]byte{'x'}, 40<<20)
	if _, err := transcript.Write(payload); err != nil {
		t.Fatal(err)
	}
	reader := transcript.RetainReader(0)
	defer reader.Close()
	reader.Advance(31 << 20)
	transcript.mu.Lock()
	fileBase := transcript.fileBase
	segments := append([]serialTranscriptSegment(nil), transcript.segments...)
	transcript.mu.Unlock()
	if fileBase != 32<<20 || len(segments) != 1 || segments[0].base != 24<<20 {
		t.Fatalf("continuous segmented reclaim file base=%d segments=%+v", fileBase, segments)
	}
	text, next := transcript.ReadFrom(31 << 20)
	if len(text) == 0 || next <= 31<<20 {
		t.Fatal("segmented transcript lost the live suffix")
	}
}

func TestSerialTranscriptGenericWaitSeesMarkerBeforeContinuousTailEvictsIt(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	done := make(chan error, 1)
	go func() {
		_, err := transcript.WaitFor(t.Context(), 0, func(text string) bool {
			return strings.Contains(text, "guest-ready-marker")
		})
		done <- err
	}()
	if _, err := transcript.Write([]byte("guest-ready-marker\n")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := transcript.Write(bytes.Repeat([]byte{'n'}, serialTranscriptReadBytes)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("continuous boot output evicted the ready marker before evaluation")
	}
}

func TestSerialTranscriptBatchesEndOfFileReclamation(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	reader := transcript.RetainReader(0)
	defer reader.Close()
	if _, err := transcript.Write(bytes.Repeat([]byte{'a'}, 2<<20)); err != nil {
		t.Fatal(err)
	}
	reader.Advance(transcript.Len())
	info, err := transcript.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2<<20 {
		t.Fatalf("small consumed batch was reclaimed immediately: %d", info.Size())
	}
	if _, err := transcript.Write(bytes.Repeat([]byte{'b'}, 7<<20)); err != nil {
		t.Fatal(err)
	}
	reader.Advance(transcript.Len())
	info, err = transcript.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	transcript.mu.Lock()
	segments := len(transcript.segments)
	transcript.mu.Unlock()
	if info.Size() != 1<<20 || segments != 0 {
		t.Fatalf("batched segmented transcript size=%d segments=%d, want one bounded active segment", info.Size(), segments)
	}
}

func TestEnvHelpers(t *testing.T) {
	merged := MergeEnv([]string{"A=1", "BAD", "B=2"}, []string{"B=override", "C=3", "=ignored"})
	want := []string{"A=1", "B=override", "C=3"}
	if strings.Join(merged, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("MergeEnv = %#v, want %#v", merged, want)
	}

	defaulted := WithDefaultEnv([]string{"PATH=/custom", "X=1"})
	if !HasEnvKey(defaulted, "PATH") || !HasEnvKey(defaulted, "HOME") || HasEnvKey(defaulted, "MISSING") {
		t.Fatalf("default env keys not applied correctly: %#v", defaulted)
	}
	if got := DefaultHostname(" (none) "); got != "ccx3" {
		t.Fatalf("DefaultHostname = %q, want ccx3", got)
	}
}

func TestHasManagedControlAck(t *testing.T) {
	text := protocol.ControlAckPrefix + "signal-7\n"
	if !HasManagedControlAck(text, "signal-7") || HasManagedControlAck(text, "signal-8") {
		t.Fatalf("control acknowledgement matching failed for %q", text)
	}
}

func b64(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}
