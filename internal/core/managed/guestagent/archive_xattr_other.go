//go:build !linux && !freebsd && !netbsd && !darwin && !windows

package guestagent

import "syscall"

func archiveListXattrs(string, []byte) (int, error)       { return 0, syscall.ENOTSUP }
func archiveGetXattr(string, string, []byte) (int, error) { return 0, syscall.ENOTSUP }
func archiveSetXattr(string, string, []byte, bool) error  { return syscall.ENOTSUP }
