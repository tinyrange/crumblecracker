package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestExecRequestCopiesFields(t *testing.T) {
	req := ExecRequest("7", client.ExecRequest{
		Command:     []string{"/bin/sh", "-c", "echo hi"},
		Env:         []string{"A=B"},
		RootDir:     "/mnt",
		Path:        "/tmp/file",
		Directory:   true,
		ReplaceEnv:  true,
		SkipResolve: true,
		WorkDir:     "/work",
		User:        "1000:1000",
		Stdin:       []byte("input"),
		TTY:         true,
		ControlFD:   true,
		Cols:        80,
		Rows:        24,
	})
	if req.Kind != "exec" || req.ID != "7" || req.Command[0] != "/bin/sh" || req.Env[0] != "A=B" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if !req.Directory || !req.TTY || !req.ControlFD || req.Cols != 80 || req.Rows != 24 {
		t.Fatalf("missing flags: %+v", req)
	}
	if !req.ReplaceEnv || !req.SkipResolve {
		t.Fatalf("missing resolver flags: %+v", req)
	}
}

func TestInputRequestConvertsLegacyInputString(t *testing.T) {
	req, ok := InputRequest("9", client.ExecInput{Kind: "stdin", Input: "hello"})
	if !ok {
		t.Fatalf("InputRequest returned !ok")
	}
	if string(req.Stdin) != "hello" {
		t.Fatalf("stdin = %q", req.Stdin)
	}
}

func TestSendWritesCompleteJSONLine(t *testing.T) {
	var buf bytes.Buffer
	if err := Send(&buf, protocol.ManagedExecRequest{Kind: "sync", ID: "1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte{'\n'}) {
		t.Fatalf("send did not append newline: %q", buf.String())
	}
	var got protocol.ManagedExecRequest
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != "sync" || got.ID != "1" {
		t.Fatalf("got %+v", got)
	}
}

func TestSendExecClosesEmptyStdin(t *testing.T) {
	var buf bytes.Buffer
	if err := SendExec(&buf, "42", client.ExecRequest{Command: []string{"true"}}); err != nil {
		t.Fatalf("SendExec: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %q", len(lines), buf.String())
	}
	var got protocol.ManagedExecRequest
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("unmarshal close request: %v", err)
	}
	if got.Kind != "stdin_close" || got.ID != "42" {
		t.Fatalf("close request = %+v", got)
	}
}

func TestSendExecKeepsInlineStdinOpen(t *testing.T) {
	var buf bytes.Buffer
	if err := SendExec(&buf, "42", client.ExecRequest{Command: []string{"cat"}, Stdin: []byte("input")}); err != nil {
		t.Fatalf("SendExec: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1: %q", len(lines), buf.String())
	}
}

func TestWriteFullRejectsZeroWrite(t *testing.T) {
	if err := WriteFull(zeroWriter{}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteFull error = %v, want ErrShortWrite", err)
	}
}

func TestForwardInputsClosesStdinOnce(t *testing.T) {
	inputs := make(chan client.ExecInput, 3)
	inputs <- client.ExecInput{Kind: "stdin", Data: []byte("a")}
	inputs <- client.ExecInput{Kind: "stdin_close"}
	inputs <- client.ExecInput{Kind: "stdin", Data: []byte("ignored")}
	close(inputs)

	var got []protocol.ManagedExecRequest
	if err := ForwardInputs(context.Background(), "2", inputs, func(req protocol.ManagedExecRequest) error {
		got = append(got, req)
		return nil
	}); err != nil {
		t.Fatalf("ForwardInputs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(got), got)
	}
	if got[0].Kind != "stdin" || string(got[0].Stdin) != "a" || got[1].Kind != "stdin_close" {
		t.Fatalf("messages = %+v", got)
	}
}

func TestForwardInputsReportsSendFailure(t *testing.T) {
	inputs := make(chan client.ExecInput, 1)
	inputs <- client.ExecInput{Kind: "signal", Signal: "TERM"}
	want := errors.New("control delivery failed")
	err := ForwardInputs(context.Background(), "2", inputs, func(protocol.ManagedExecRequest) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("ForwardInputs error = %v, want %v", err, want)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
