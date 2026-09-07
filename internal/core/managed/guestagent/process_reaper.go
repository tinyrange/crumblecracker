//go:build !windows

package guestagent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var agentChildren = struct {
	sync.Mutex
	pids   map[int]struct{}
	groups map[int]struct{}
}{
	pids:   make(map[int]struct{}),
	groups: make(map[int]struct{}),
}

func startManagedCommand(cmd *exec.Cmd) error {
	agentChildren.Lock()
	err := cmd.Start()
	if err == nil {
		agentChildren.pids[cmd.Process.Pid] = struct{}{}
	}
	agentChildren.Unlock()
	return err
}

func waitManagedCommand(cmd *exec.Cmd) error {
	err := cmd.Wait()
	agentChildren.Lock()
	delete(agentChildren.pids, cmd.Process.Pid)
	// Every guest command starts its own process group. Descendants orphaned
	// by the command retain that group even after its leader has been waited.
	agentChildren.groups[cmd.Process.Pid] = struct{}{}
	agentChildren.Unlock()
	return err
}

// startOrphanReaper gives the BSD guest agent the other essential job of PID
// 1: collecting descendants left behind by commands. Direct command children
// remain reserved for os/exec so their structured exit status cannot be stolen.
func startOrphanReaper(ctx context.Context) {
	if os.Getpid() != 1 {
		return
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reapAgentOrphans()
			}
		}
	}()
}

func reapAgentOrphans() {
	agentChildren.Lock()
	defer agentChildren.Unlock()

	for pgid := range agentChildren.groups {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-pgid, &status, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			delete(agentChildren.groups, pgid)
		}
	}

	// With no command directly owned by os/exec, any remaining children are
	// necessarily orphans, including daemonized descendants that changed their
	// process group. Reap those without an external process-table dependency.
	if len(agentChildren.pids) == 0 {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
		return
	}

	reapPlatformOrphans(agentChildren.pids)
}
