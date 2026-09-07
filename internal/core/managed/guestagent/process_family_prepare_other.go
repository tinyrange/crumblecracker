//go:build !linux && !freebsd && !netbsd && !openbsd && !windows

package guestagent

import "os/exec"

type noopProcessFamilyPreparation struct{}

func prepareProcessFamily(*exec.Cmd, string) (processFamilyPreparation, error) {
	return noopProcessFamilyPreparation{}, nil
}

func (noopProcessFamilyPreparation) Start(int) (processFamilyTracker, error) { return nil, nil }
func (noopProcessFamilyPreparation) Abort()                                  {}
