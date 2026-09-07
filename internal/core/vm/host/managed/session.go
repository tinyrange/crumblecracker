package managed

import (
	"context"
	"fmt"

	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
)

func FlushSession(ctx context.Context, session managedsession.Session) error {
	if session == nil {
		return fmt.Errorf("instance is not running")
	}
	return session.Flush(ctx)
}

func SessionConsoleHistory(ctx context.Context, session managedsession.Session) (string, error) {
	if session == nil {
		return "", nil
	}
	return session.ConsoleHistory(ctx)
}

func WaitSession(session managedsession.Session) error {
	if session == nil {
		return nil
	}
	return session.Wait()
}

func CloseSession(session managedsession.Session, cleanups ...func() error) error {
	if session == nil {
		return nil
	}
	err := session.Close()
	for _, cleanup := range cleanups {
		if cleanup == nil {
			continue
		}
		if cleanupErr := cleanup(); err == nil {
			err = cleanupErr
		}
	}
	return err
}

type NetworkCloser interface {
	Close() error
}

func CloseSessionWithNetwork(session managedsession.Session, network NetworkCloser) error {
	if session == nil {
		if network == nil {
			return nil
		}
		return network.Close()
	}
	return CloseSession(session, func() error {
		if network == nil {
			return nil
		}
		return network.Close()
	})
}
