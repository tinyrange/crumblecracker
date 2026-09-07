package host

import (
	"context"

	"github.com/tinyrange/crumblecracker/internal/core/managed/machine"
	"github.com/tinyrange/crumblecracker/internal/core/managed/rootartifact"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type StartRequest struct {
	Spec        machine.Spec
	Artifact    rootartifact.Artifact
	Attachments any
}

type VMM interface {
	Start(context.Context, StartRequest, func(client.BootEvent) error) (managedsession.Session, error)
}
