//go:build darwin

package capturerelay

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin does not reliably wake poll or kqueue for FIFO data after writer
// transitions. select observes the connected FIFO's current readable state and
// lets the relay retain one readiness loop instead of one goroutine per stream.
type relayPoller struct {
	fds   []int
	maxFD int
}

func (p *relayPoller) reset(fds []int) error {
	var set unix.FdSet
	capacity := len(set.Bits) * int(unsafe.Sizeof(set.Bits[0])) * 8
	p.fds = append(p.fds[:0], fds...)
	p.maxFD = -1
	for _, fd := range p.fds {
		if fd < 0 || fd >= capacity {
			return fmt.Errorf("descriptor %d exceeds select capacity %d", fd, capacity)
		}
		if fd > p.maxFD {
			p.maxFD = fd
		}
	}
	return nil
}

func (p *relayPoller) wait(ready []bool) error {
	var set unix.FdSet
	for _, fd := range p.fds {
		set.Set(fd)
	}
	if _, err := unix.Select(p.maxFD+1, &set, nil, nil, nil); err != nil {
		return err
	}
	for i, fd := range p.fds {
		ready[i] = set.IsSet(fd)
	}
	return nil
}

func (*relayPoller) close() {}
