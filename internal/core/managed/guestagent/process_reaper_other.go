//go:build !freebsd && !netbsd && !openbsd && !windows

package guestagent

func reapPlatformOrphans(map[int]struct{}) {}
