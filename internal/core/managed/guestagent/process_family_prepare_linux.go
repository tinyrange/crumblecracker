//go:build linux

package guestagent

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type linuxProcessFamilyPreparation struct {
	path string
	dir  *os.File
}

func prepareProcessFamily(cmd *exec.Cmd, id string) (processFamilyPreparation, error) {
	name := fmt.Sprintf("cc-exec-%x", sha256.Sum256([]byte(id)))[:32]
	path := filepath.Join("/sys/fs/cgroup", name)
	if err := os.Mkdir(path, 0o755); err != nil {
		if os.Getpid() == 1 {
			return nil, fmt.Errorf("create command cgroup: %w", err)
		}
		return &linuxProcessFamilyPreparation{}, nil
	}
	dir, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		if os.Getpid() == 1 {
			return nil, fmt.Errorf("open command cgroup: %w", err)
		}
		return &linuxProcessFamilyPreparation{}, nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(dir.Fd())
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWCGROUP
	return &linuxProcessFamilyPreparation{path: path, dir: dir}, nil
}

func (p *linuxProcessFamilyPreparation) Start(int) (processFamilyTracker, error) {
	return &linuxCgroupTracker{path: p.path, dir: p.dir}, nil
}

func (p *linuxProcessFamilyPreparation) Abort() {
	if p.dir != nil {
		_ = p.dir.Close()
	}
	if p.path != "" {
		_ = os.Remove(p.path)
	}
}

type linuxCgroupTracker struct {
	path string
	dir  *os.File
	once sync.Once
}

func (*linuxCgroupTracker) kernelTracksDescendants() {}

func (t *linuxCgroupTracker) Snapshot() map[int]struct{} {
	pids := make(map[int]struct{})
	if t.path == "" {
		return pids
	}
	_ = filepath.WalkDir(t.path, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "cgroup.procs" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err == nil {
				pids[pid] = struct{}{}
			}
		}
		return nil
	})
	return pids
}

func (t *linuxCgroupTracker) Kill() error {
	if t.path == "" {
		return syscall.ENOTSUP
	}
	return os.WriteFile(filepath.Join(t.path, "cgroup.kill"), []byte("1"), 0o200)
}

func (t *linuxCgroupTracker) Close() {
	t.once.Do(func() {
		if t.dir != nil {
			_ = t.dir.Close()
		}
		if t.path != "" {
			var dirs []string
			_ = filepath.WalkDir(t.path, func(path string, entry os.DirEntry, err error) error {
				if err == nil && entry.IsDir() {
					dirs = append(dirs, path)
				}
				return nil
			})
			for i := len(dirs) - 1; i >= 0; i-- {
				_ = os.Remove(dirs[i])
			}
		}
	})
}
