package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tinyrange/crumblecracker/internal/core/managed/protocol"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type SendFunc func(protocol.ManagedExecRequest) error

func ExecRequest(id string, req client.ExecRequest) protocol.ManagedExecRequest {
	return protocol.ManagedExecRequest{
		Kind:          RequestKind(req.Kind),
		ID:            id,
		Command:       append([]string(nil), req.Command...),
		Env:           append([]string(nil), req.Env...),
		RootDir:       req.RootDir,
		Path:          req.Path,
		Directory:     req.Directory,
		ReplaceEnv:    req.ReplaceEnv,
		SkipResolve:   req.SkipResolve,
		WorkDir:       req.WorkDir,
		User:          req.User,
		Stdin:         append([]byte(nil), req.Stdin...),
		TTY:           req.TTY,
		ControlFD:     req.ControlFD,
		Cols:          req.Cols,
		Rows:          req.Rows,
		ArchiveLimits: req.ArchiveLimits,
	}
}

func RequestKind(kind string) string {
	if kind == "" {
		return "exec"
	}
	return kind
}

func InputRequest(id string, input client.ExecInput) (protocol.ManagedExecRequest, bool) {
	msg := protocol.ManagedExecRequest{ID: id, Kind: input.Kind}
	switch input.Kind {
	case "stdin":
		if len(input.Data) > 0 {
			msg.Stdin = append([]byte(nil), input.Data...)
		} else if input.Input != "" {
			msg.Stdin = []byte(input.Input)
		}
	case "stdin_close":
	case "signal":
		msg.Signal = input.Signal
	case "resize":
		msg.Cols = input.Cols
		msg.Rows = input.Rows
	default:
		return protocol.ManagedExecRequest{}, false
	}
	return msg, true
}

func StdinCloseRequest(id string) protocol.ManagedExecRequest {
	return protocol.ManagedExecRequest{ID: id, Kind: "stdin_close"}
}

func SyncRequest(id string) protocol.ManagedExecRequest {
	return protocol.ManagedExecRequest{ID: id, Kind: "sync"}
}

func Send(w io.Writer, msg protocol.ManagedExecRequest) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal exec request: %w", err)
	}
	if err := WriteFull(w, append(payload, '\n')); err != nil {
		return fmt.Errorf("write exec request: %w", err)
	}
	return nil
}

func SendExec(w io.Writer, id string, req client.ExecRequest) error {
	if err := Send(w, ExecRequest(id, req)); err != nil {
		return err
	}
	if len(req.Stdin) != 0 {
		return nil
	}
	return Send(w, StdinCloseRequest(id))
}

func WriteFull(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n > 0 {
			payload = payload[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func ForwardInputs(ctx context.Context, id string, inputs <-chan client.ExecInput, send SendFunc) error {
	stdinClosed := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case input, ok := <-inputs:
			if !ok {
				if !stdinClosed {
					if err := send(StdinCloseRequest(id)); err != nil {
						return err
					}
				}
				return nil
			}
			if input.Kind == "stdin_close" {
				if stdinClosed {
					continue
				}
				stdinClosed = true
			} else if input.Kind == "stdin" && stdinClosed {
				continue
			}
			if msg, ok := InputRequest(id, input); ok {
				if err := send(msg); err != nil {
					return err
				}
			}
		}
	}
}
