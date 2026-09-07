package protocol

import "github.com/tinyrange/crumblecracker/internal/protocol"

const (
	ReadyMarker         = "__CCX3_READY__"
	BeginMarkerPrefix   = "__CCX3_BEGIN__:"
	OutputMarkerPrefix  = "__CCX3_OUT__:"
	ErrorMarkerPrefix   = "__CCX3_ERR__:"
	ControlMarkerPrefix = "__CCX3_CTL__:"
	UsageMarkerPrefix   = "__CCX3_USAGE__:"
	ExitMarkerPrefix    = "__CCX3_EXIT__:"
	TimingMarkerPrefix  = "__CCX3_TIMING__:"
	ControlAckPrefix    = "__CCX3_ACK__:"
)

type ManagedExecRequest struct {
	ID            string                `json:"id"`
	Command       []string              `json:"command"`
	Env           []string              `json:"env,omitempty"`
	RootDir       string                `json:"root_dir,omitempty"`
	Path          string                `json:"path,omitempty"`
	Directory     bool                  `json:"directory,omitempty"`
	ReplaceEnv    bool                  `json:"replace_env,omitempty"`
	SkipResolve   bool                  `json:"skip_resolve,omitempty"`
	WorkDir       string                `json:"workdir,omitempty"`
	User          string                `json:"user,omitempty"`
	Stdin         []byte                `json:"stdin,omitempty"`
	TTY           bool                  `json:"tty,omitempty"`
	ControlFD     bool                  `json:"control_fd,omitempty"`
	Kind          string                `json:"kind,omitempty"`
	Signal        string                `json:"signal,omitempty"`
	ControlID     string                `json:"control_id,omitempty"`
	Cols          int                   `json:"cols,omitempty"`
	Rows          int                   `json:"rows,omitempty"`
	ArchiveLimits *client.ArchiveLimits `json:"archive_limits,omitempty"`
}
