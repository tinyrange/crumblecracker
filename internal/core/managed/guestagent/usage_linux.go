//go:build linux

package guestagent

import (
	"os"
	"syscall"
	"time"
)

type ExecUsage struct {
	WallSeconds   float64 `json:"wall_seconds,omitempty"`
	UserSeconds   float64 `json:"user_seconds,omitempty"`
	SystemSeconds float64 `json:"system_seconds,omitempty"`
	CPUSeconds    float64 `json:"cpu_seconds,omitempty"`
	MaxRSSBytes   uint64  `json:"max_rss_bytes,omitempty"`
}

func UsageFromProcessState(state *os.ProcessState, wall time.Duration) *ExecUsage {
	usage := &ExecUsage{WallSeconds: wall.Seconds()}
	if state == nil {
		return usage
	}
	if state.UserTime() > 0 {
		usage.UserSeconds = state.UserTime().Seconds()
	}
	if state.SystemTime() > 0 {
		usage.SystemSeconds = state.SystemTime().Seconds()
	}
	usage.CPUSeconds = usage.UserSeconds + usage.SystemSeconds
	if raw, ok := state.SysUsage().(*syscall.Rusage); ok && raw != nil && raw.Maxrss > 0 {
		usage.MaxRSSBytes = uint64(raw.Maxrss) * 1024
	}
	return usage
}

func EncodeExecUsage(usage *ExecUsage) string {
	return EncodeJSONBase64(usage)
}
