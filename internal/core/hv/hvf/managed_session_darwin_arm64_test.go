//go:build darwin && arm64

package hvf

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestManagedSessionExecClosesEmptyStdin(t *testing.T) {
	transcript := newSerialTranscript()
	control := &recordingManagedControl{transcript: transcript}
	session := &ManagedSession{
		control:    control,
		transcript: transcript,
		serialOut:  newSerialTranscript(),
		done:       newManagedSessionDone(),
	}

	resp, err := session.Exec(t.Context(), client.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("Exec exit code = %d", resp.ExitCode)
	}
	if got := control.kinds(); len(got) != 2 || got[0] != "exec" || got[1] != "stdin_close" {
		t.Fatalf("control request kinds = %v", got)
	}
}

type recordingManagedControl struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	requests   []protocol.ManagedExecRequest
	transcript io.Writer
}

func (c *recordingManagedControl) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *recordingManagedControl) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, _ := c.buf.Write(data)
	for {
		line, err := c.buf.ReadBytes('\n')
		if err != nil {
			c.buf.Write(line)
			return n, nil
		}
		var req protocol.ManagedExecRequest
		if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
			return n, err
		}
		if req.Kind == "" {
			req.Kind = "exec"
		}
		c.requests = append(c.requests, req)
		if req.Kind == "stdin_close" {
			_, _ = c.transcript.Write([]byte(protocol.BeginMarkerPrefix + req.ID + "\n"))
			_, _ = c.transcript.Write([]byte(protocol.ExitMarkerPrefix + req.ID + ":0\n"))
		}
	}
}

func (c *recordingManagedControl) Close() error {
	return nil
}

func (c *recordingManagedControl) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.requests))
	for i, req := range c.requests {
		out[i] = req.Kind
	}
	return out
}
