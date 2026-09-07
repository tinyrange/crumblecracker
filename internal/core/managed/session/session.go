package session

import (
	"context"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type Session interface {
	Exec(context.Context, client.ExecRequest) (client.ExecResponse, error)
	ExecStream(context.Context, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
	Flush(context.Context) error
	ConsoleHistory(context.Context) (string, error)
	Wait() error
	Close() error
}
