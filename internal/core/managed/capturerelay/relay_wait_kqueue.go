//go:build freebsd || netbsd || openbsd

package capturerelay

import (
	"fmt"

	"golang.org/x/sys/unix"
)

type relayPoller struct {
	fd          int
	initialized bool
	index       map[int]int
	events      []unix.Kevent_t
}

func (p *relayPoller) reset(fds []int) error {
	p.close()
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	p.fd = fd
	p.initialized = true
	p.index = make(map[int]int, len(fds))
	changes := make([]unix.Kevent_t, len(fds))
	for i, watched := range fds {
		p.index[watched] = i
		// FIFOs remember EV_EOF when their last writer disconnects. EV_CLEAR
		// resets that state after delivery so a later command writer can make
		// the same persistent relay descriptor readable again.
		unix.SetKevent(&changes[i], watched, unix.EVFILT_READ, unix.EV_ADD|unix.EV_ENABLE|unix.EV_CLEAR)
	}
	if _, err := unix.Kevent(p.fd, changes, nil, nil); err != nil {
		p.close()
		return err
	}
	if cap(p.events) < len(fds) {
		p.events = make([]unix.Kevent_t, len(fds))
	} else {
		p.events = p.events[:len(fds)]
	}
	return nil
}

func (p *relayPoller) wait(ready []bool) error {
	for i := range ready {
		ready[i] = false
	}
	n, err := unix.Kevent(p.fd, nil, p.events, nil)
	if err != nil {
		return err
	}
	for _, event := range p.events[:n] {
		if event.Flags&unix.EV_ERROR != 0 && event.Data != 0 {
			return fmt.Errorf("kevent descriptor %d: %w", event.Ident, unix.Errno(event.Data))
		}
		if index, ok := p.index[int(event.Ident)]; ok {
			ready[index] = true
		}
	}
	return nil
}

func (p *relayPoller) close() {
	if !p.initialized {
		return
	}
	_ = unix.Close(p.fd)
	p.fd = -1
	p.initialized = false
}
