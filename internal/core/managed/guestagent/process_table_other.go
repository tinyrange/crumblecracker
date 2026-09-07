//go:build !linux && !freebsd && !netbsd && !openbsd && !windows

package guestagent

import "time"

const processFamilyPollInterval = 20 * time.Millisecond

func processSnapshot(string) (map[int]int, map[int]struct{}) { return nil, nil }
