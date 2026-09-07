package managed

import (
	"context"
	"fmt"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/core/vm/execplan"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type Core struct {
	osName         string
	session        managedsession.Session
	root           imagefs.Directory
	baseEnv        []string
	defaultUser    string
	defaultRootDir string
	workDir        string
	caps           managedguest.Capabilities
	env            func(base, overrides []string, replace bool) []string
	user           func(string) (string, error)
	missingRootErr string
	markResolved   bool
}

type Config struct {
	OSName         string
	Session        managedsession.Session
	Root           imagefs.Directory
	BaseEnv        []string
	DefaultUser    string
	DefaultRootDir string
	WorkDir        string
	Capabilities   managedguest.Capabilities
	Env            func(base, overrides []string, replace bool) []string
	User           func(string) (string, error)
	MissingRootErr string
	MarkResolved   bool
}

func NewCore(cfg Config) *Core {
	return &Core{
		osName:         cfg.OSName,
		session:        cfg.Session,
		root:           cfg.Root,
		baseEnv:        append([]string(nil), cfg.BaseEnv...),
		defaultUser:    strings.TrimSpace(cfg.DefaultUser),
		defaultRootDir: cfg.DefaultRootDir,
		workDir:        cfg.WorkDir,
		caps:           cfg.Capabilities,
		env:            cfg.Env,
		user:           cfg.User,
		missingRootErr: cfg.MissingRootErr,
		markResolved:   cfg.MarkResolved,
	}
}

func (i *Core) ManagedCapabilities() managedguest.Capabilities {
	if i == nil {
		return managedguest.Capabilities{}
	}
	return i.caps
}

func (i *Core) Session() managedsession.Session {
	if i == nil {
		return nil
	}
	return i.session
}

func (i *Core) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	if i == nil || i.session == nil {
		return client.ExecResponse{}, fmt.Errorf("instance is not running")
	}
	if req.Kind != "" && req.Kind != "exec" {
		if err := i.CheckControlRequest(req.Kind); err != nil {
			return client.ExecResponse{}, err
		}
		return i.session.Exec(ctx, req)
	}
	execReq, err := i.ExecRequest(req)
	if err != nil {
		return client.ExecResponse{}, err
	}
	return i.session.Exec(ctx, execReq)
}

func (i *Core) ExecStream(ctx context.Context, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	if i == nil || i.session == nil {
		return fmt.Errorf("instance is not running")
	}
	if req.Kind != "" && req.Kind != "exec" {
		if err := i.CheckControlRequest(req.Kind); err != nil {
			return err
		}
		return i.session.ExecStream(ctx, execplan.ControlRequest(req, i.workDir), inputs, onEvent)
	}
	execReq, err := i.ExecRequest(req)
	if err != nil {
		return err
	}
	return i.session.ExecStream(ctx, execReq, inputs, onEvent)
}

func (i *Core) ExecRequest(req client.ExecRequest) (client.ExecRequest, error) {
	if i == nil {
		return client.ExecRequest{}, fmt.Errorf("instance is not running")
	}
	if strings.TrimSpace(req.User) == "" {
		req.User = i.defaultUser
	}
	return execplan.ResolveExecRequest(req, execplan.Resolver{
		Root:           i.root,
		BaseEnv:        i.baseEnv,
		DefaultWorkDir: i.workDir,
		DefaultRootDir: i.defaultRootDir,
		MissingRootErr: i.missingRootError(),
		Env:            i.env,
		User:           i.user,
		MarkResolved:   i.markResolved,
	})
}

func (i *Core) AddShare(context.Context, client.ShareMount) error {
	return i.Unsupported("filesystem shares")
}

func (i *Core) AddPortForward(context.Context, client.PortForward) error {
	return i.Unsupported("port forwards")
}

func (i *Core) Flush(ctx context.Context) error {
	if i == nil {
		return FlushSession(ctx, nil)
	}
	return FlushSession(ctx, i.session)
}

func (i *Core) ConsoleHistory(ctx context.Context) (string, error) {
	if i == nil {
		return SessionConsoleHistory(ctx, nil)
	}
	return SessionConsoleHistory(ctx, i.session)
}

func (i *Core) Wait() error {
	if i == nil {
		return nil
	}
	return WaitSession(i.session)
}

func (i *Core) Close() error {
	if i == nil {
		return nil
	}
	return CloseSession(i.session)
}

func (i *Core) missingRootError() string {
	if i == nil {
		return ""
	}
	if strings.TrimSpace(i.missingRootErr) != "" {
		return i.missingRootErr
	}
	return fmt.Sprintf("running %s instance does not have a root filesystem", i.displayName())
}

func (i *Core) Unsupported(feature string) error {
	if i == nil {
		return execplan.UnsupportedFeature("", managedguest.Capabilities{}, feature)
	}
	return execplan.UnsupportedFeature(i.displayName(), i.caps, feature)
}

func (i *Core) CheckControlRequest(kind string) error {
	if i == nil {
		return execplan.CheckControlRequest("", managedguest.Capabilities{}, kind)
	}
	return execplan.CheckControlRequest(i.displayName(), i.caps, kind)
}

func (i *Core) displayName() string {
	if i == nil || strings.TrimSpace(i.osName) == "" {
		return "managed guest"
	}
	return i.osName
}
