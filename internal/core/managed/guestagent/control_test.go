package guestagent

import (
	"errors"
	"testing"
)

func TestActiveExecSetTracksPendingClose(t *testing.T) {
	set := NewActiveExecSet()
	found, err := set.CloseStdin("7", true)
	if err != nil || found {
		t.Fatalf("CloseStdin missing = %v, %v; want false, nil", found, err)
	}
	exec := &fakeActiveExec{}
	if closePending := set.Add("7", exec); !closePending {
		t.Fatalf("Add closePending = false, want true")
	}
	if closePending := set.Add("8", &fakeActiveExec{}); closePending {
		t.Fatalf("Add closePending for new id = true, want false")
	}
}

func TestActiveExecSetForwardsControls(t *testing.T) {
	set := NewActiveExecSet()
	exec := &fakeActiveExec{err: errors.New("boom")}
	set.Add("7", exec)
	if found, err := set.WriteStdin("7", []byte("input")); !found || !errors.Is(err, exec.err) {
		t.Fatalf("WriteStdin = %v, %v; want true, boom", found, err)
	}
	if exec.stdin != "input" {
		t.Fatalf("stdin = %q", exec.stdin)
	}
	exec.err = nil
	if found, err := set.Signal("7", "TERM"); !found || err != nil {
		t.Fatalf("Signal = %v, %v; want true, nil", found, err)
	}
	if found, err := set.Resize("7", 80, 24); !found || err != nil {
		t.Fatalf("Resize = %v, %v; want true, nil", found, err)
	}
	if found, err := set.CloseStdin("7", false); !found || err != nil {
		t.Fatalf("CloseStdin = %v, %v; want true, nil", found, err)
	}
	if exec.signal != "TERM" || exec.cols != 80 || exec.rows != 24 || !exec.closed {
		t.Fatalf("exec = %+v", exec)
	}
	set.Delete("7")
	if got := set.Get("7"); got != nil {
		t.Fatalf("Get after delete = %#v, want nil", got)
	}
}

func TestHandleActiveControlDispatchesActiveExecRequests(t *testing.T) {
	set := NewActiveExecSet()
	exec := &fakeActiveExec{}
	set.Add("7", exec)
	for _, req := range []ActiveControlRequest{
		{Kind: "stdin", ID: "7", Stdin: []byte("input")},
		{Kind: "signal", ID: "7", Signal: "TERM"},
		{Kind: "resize", ID: "7", Cols: 100, Rows: 40},
		{Kind: "stdin_close", ID: "7", RememberPendingStdinClose: true},
	} {
		result := HandleActiveControl(set, req)
		if !result.Handled || !result.Found || result.Err != nil {
			t.Fatalf("HandleActiveControl(%s) = %+v", req.Kind, result)
		}
		if req.Kind == "stdin_close" && result.Exec != exec {
			t.Fatalf("stdin_close Exec = %#v, want %#v", result.Exec, exec)
		}
	}
	if exec.stdin != "input" || exec.signal != "TERM" || exec.cols != 100 || exec.rows != 40 || !exec.closed {
		t.Fatalf("exec = %+v", exec)
	}
	if result := HandleActiveControl(set, ActiveControlRequest{Kind: "unknown"}); result.Handled {
		t.Fatalf("unknown control result = %+v", result)
	}
	if result := HandleActiveControl(NewActiveExecSet(), ActiveControlRequest{Kind: "stdin_close", ID: "missing", RememberPendingStdinClose: true}); !result.Handled || result.Found {
		t.Fatalf("missing stdin_close result = %+v", result)
	}
}

func TestPendingRequestsStoresAndConsumesByID(t *testing.T) {
	pending := NewPendingRequests[string]()
	pending.Put("", "ignored")
	pending.Put("7", "first")
	pending.Put("7", "second")

	if got, ok := pending.Take(""); ok || got != "" {
		t.Fatalf("Take empty = %q, %v; want zero, false", got, ok)
	}
	if got, ok := pending.Take("7"); !ok || got != "second" {
		t.Fatalf("Take first = %q, %v; want second, true", got, ok)
	}
	if got, ok := pending.Take("7"); ok || got != "" {
		t.Fatalf("Take consumed = %q, %v; want zero, false", got, ok)
	}
}

func TestActiveExecControlAcknowledgements(t *testing.T) {
	active := NewActiveExecSet()
	active.Add("7", &fakeActiveExec{})
	if active.ControlAcknowledged("7", "signal-1") {
		t.Fatal("new control was already acknowledged")
	}
	active.AcknowledgeControl("7", "signal-1")
	if !active.ControlAcknowledged("7", "signal-1") {
		t.Fatal("acknowledged control was forgotten")
	}
	active.Delete("7")
	if active.ControlAcknowledged("7", "signal-1") {
		t.Fatal("deleted exec retained control acknowledgements")
	}
}

type fakeActiveExec struct {
	stdin  string
	closed bool
	signal string
	cols   int
	rows   int
	err    error
}

func (f *fakeActiveExec) WriteStdin(data []byte) error {
	f.stdin += string(data)
	return f.err
}

func (f *fakeActiveExec) CloseStdin() error {
	f.closed = true
	return f.err
}

func (f *fakeActiveExec) Signal(name string) error {
	f.signal = name
	return f.err
}

func (f *fakeActiveExec) Resize(cols, rows int) error {
	f.cols = cols
	f.rows = rows
	return f.err
}
