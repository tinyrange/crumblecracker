//go:build !linux && !freebsd && !darwin && !windows

package guestagent

import "syscall"

func archiveSeekData(int, int64) (int64, error) { return 0, syscall.ENOTSUP }
func archiveSeekHole(int, int64) (int64, error) { return 0, syscall.ENOTSUP }
