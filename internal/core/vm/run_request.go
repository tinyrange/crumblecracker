package vm

import (
	"os"
	"strconv"
	"sync/atomic"

	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

var guestExecCounter atomic.Uint64

func guestExecID() string {
	return "exec-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(guestExecCounter.Add(1), 36)
}

func guestExecIDFor(id string) string {
	if id != "" {
		return id
	}
	return guestExecID()
}

func runExecRequest(req client.RunRequest) client.ExecRequest {
	return client.ExecRequest{
		ID:         guestExecIDFor(req.ID),
		Command:    append([]string(nil), req.Command...),
		Env:        append([]string(nil), req.Env...),
		RootDir:    req.RootDir,
		ReplaceEnv: req.ReplaceEnv,
		WorkDir:    req.WorkDir,
		User:       req.User,
		Stdin:      append([]byte(nil), req.Stdin...),
		TTY:        req.TTY,
		ControlFD:  req.ControlFD,
		Cols:       req.Cols,
		Rows:       req.Rows,
	}
}

func runningVMExecRequest(req client.RunRequest) client.ExecRequest {
	execReq := runExecRequest(req)
	execReq.SkipResolve = true
	return execReq
}

func mergeImageRunEnv(base, overrides []string, _ bool) []string {
	return vmruntime.WithDefaultEnv(vmruntime.MergeEnv(base, overrides))
}
