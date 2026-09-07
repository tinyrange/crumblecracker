package vmruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSerialTranscriptSpillsAndStreamsIncrementally(t *testing.T) {
	transcript := NewSerialTranscript()
	data := bytes.Repeat([]byte("serial-output\n"), (2<<20)/len("serial-output\n"))
	if n, err := transcript.Write(data); err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
	transcript.mu.Lock()
	path := transcript.path
	spilled := transcript.file != nil
	inMemory := len(transcript.buf)
	transcript.mu.Unlock()
	if !spilled || inMemory != 0 {
		t.Fatalf("spilled = %t, retained memory = %d", spilled, inMemory)
	}
	var got []byte
	for offset := 0; offset < transcript.Len(); {
		chunk, next := transcript.ReadFrom(offset)
		if next <= offset || len(chunk) > serialTranscriptReadBytes {
			t.Fatalf("read offset %d returned %d bytes and next %d", offset, len(chunk), next)
		}
		got = append(got, chunk...)
		offset = next
	}
	if !bytes.Equal(got, data) {
		t.Fatal("incremental transcript differed from written data")
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	if path != "" {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("spill remained after close: %v", err)
		}
	}
}

func TestSerialTranscriptCloseWakesPollingReaderWithoutPanicking(t *testing.T) {
	transcript := NewSerialTranscript()
	if _, err := transcript.Write([]byte("prefix")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := transcript.WaitFor(context.Background(), transcript.Len(), func(string) bool { return false })
		done <- err
	}()
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, os.ErrClosed) {
		t.Fatalf("wait after close = %v", err)
	}
}

func TestSerialTranscriptReadErrorNeverAdvancesPastDeliveredBytes(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	data := bytes.Repeat([]byte("x"), (2<<20)+17)
	if _, err := transcript.Write(data); err != nil {
		t.Fatal(err)
	}
	transcript.mu.Lock()
	if transcript.file == nil {
		transcript.mu.Unlock()
		t.Fatal("transcript did not spill")
	}
	if err := transcript.file.Truncate(1); err != nil {
		transcript.mu.Unlock()
		t.Fatal(err)
	}
	transcript.mu.Unlock()
	text, next, err := transcript.readFrom(0)
	if err == nil {
		t.Fatal("short backing read was not reported")
	}
	if next != len(text) || next > 1 {
		t.Fatalf("short read delivered %d bytes but advanced to %d", len(text), next)
	}
}

func TestSerialTranscriptCompatibilityMaterializationFailsExplicitly(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	if _, err := transcript.Write(bytes.Repeat([]byte("x"), 4097)); err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.materializeStringLimit(4096); !errors.Is(err, ErrCommandOutputRequiresStreaming) {
		t.Fatalf("materialization error = %v", err)
	}
	if got, err := transcript.materializeStringLimit(4097); err != nil || len(got) != 4097 {
		t.Fatalf("boundary materialization bytes=%d err=%v", len(got), err)
	}
}

func TestSerialTranscriptReclaimsReleasedReaderStorage(t *testing.T) {
	transcript := NewSerialTranscript()
	start := transcript.Len()
	release := transcript.RetainFrom(start)
	data := bytes.Repeat([]byte("x"), 8<<20)
	if _, err := transcript.Write(data); err != nil {
		t.Fatal(err)
	}
	if transcript.file == nil {
		t.Fatal("large transcript did not spill")
	}
	before := transcript.Len()
	release()
	info, err := transcript.file.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("released transcript backing = %#v, %v", info, err)
	}
	if transcript.Len() != before {
		t.Fatalf("logical offset changed from %d to %d", before, transcript.Len())
	}
	if got := transcript.String(); len(got) > serialTranscriptTailBytes+128 {
		t.Fatalf("diagnostic transcript retained %d bytes", len(got))
	}

	start = transcript.Len()
	release = transcript.RetainFrom(start)
	if _, err := transcript.Write([]byte("next-command\n")); err != nil {
		t.Fatal(err)
	}
	got, next := transcript.ReadFrom(start)
	if got != "next-command\n" || next != transcript.Len() {
		t.Fatalf("read after reclaim = %q, %d", got, next)
	}
	release()
}

func TestSerialTranscriptWaitForReadsGrowingStreamIncrementally(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 16; i++ {
			if _, err := transcript.Write(bytes.Repeat([]byte{'a'}, serialTranscriptReadBytes)); err != nil {
				done <- err
				return
			}
		}
		_, err := transcript.Write([]byte("marker\n"))
		done <- err
	}()
	text, err := transcript.WaitFor(t.Context(), 0, func(text string) bool {
		return strings.HasSuffix(text, "marker\n")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > serialTranscriptWaitBytes || !strings.HasSuffix(text, "marker\n") {
		t.Fatalf("bounded wait returned %d bytes without its marker", len(text))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSerialTranscriptMovableReaderDoesNotPinLaterOutput(t *testing.T) {
	transcript := NewSerialTranscript()
	defer transcript.Close()
	reader := transcript.RetainReader(0)
	defer reader.Close()
	data := bytes.Repeat([]byte("x"), 8<<20)
	if _, err := transcript.Write(data); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < transcript.Len(); {
		_, next := transcript.ReadFrom(offset)
		if next == offset {
			t.Fatal("reader stopped advancing")
		}
		offset = next
		reader.Advance(next)
	}
	info, err := transcript.file.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("advanced reader retained transcript backing = %#v, %v", info, err)
	}
}
