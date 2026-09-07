package session

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestStreamExecEventsEmitsUntilExit(t *testing.T) {
	transcript := staticTranscript(protocol.BeginMarkerPrefix + "7\n" +
		protocol.OutputMarkerPrefix + "7:" + base64.StdEncoding.EncodeToString([]byte("out")) + "\n" +
		protocol.ExitMarkerPrefix + "7:3\n")
	var events []client.ExecEvent
	err := StreamExecEvents(context.Background(), StreamExecOptions{
		Transcript: transcript,
		ID:         "7",
		OnEvent: func(event client.ExecEvent) error {
			events = append(events, event)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("StreamExecEvents: %v", err)
	}
	if len(events) != 2 || events[0].Output != "out" || events[1].ExitCode != 3 {
		t.Fatalf("events = %+v", events)
	}
}

func TestStreamExecEventsCallbackFailureHook(t *testing.T) {
	transcript := staticTranscript(protocol.OutputMarkerPrefix + "7:" + base64.StdEncoding.EncodeToString([]byte("out")) + "\n")
	wantErr := errors.New("stop")
	called := false
	err := StreamExecEvents(context.Background(), StreamExecOptions{
		Transcript: transcript,
		ID:         "7",
		OnEvent: func(client.ExecEvent) error {
			return wantErr
		},
		OnCallbackFail: func() {
			called = true
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if !called {
		t.Fatalf("callback failure hook was not called")
	}
}

func TestStreamExecEventsObservesParsing(t *testing.T) {
	transcript := staticTranscript("ignored\n" + protocol.ExitMarkerPrefix + "7:0\n")
	var kinds []string
	var finalStats StreamExecStats
	err := StreamExecEvents(context.Background(), StreamExecOptions{
		Transcript: transcript,
		ID:         "7",
		OnObserve: func(obs StreamExecObservation) {
			kinds = append(kinds, obs.Kind)
			if obs.Kind == "done" {
				finalStats = obs.Stats
			}
		},
	})
	if err != nil {
		t.Fatalf("StreamExecEvents: %v", err)
	}
	if finalStats.Lines != 2 || finalStats.Matched != 1 || finalStats.Ignored != 1 {
		t.Fatalf("final stats = %+v", finalStats)
	}
	if len(kinds) == 0 {
		t.Fatalf("no observations")
	}
}

type staticTranscript string

func (s staticTranscript) ReadFrom(offset int) (string, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(s) {
		offset = len(s)
	}
	return string(s[offset:]), len(s)
}
