//go:build !windows && !darwin && !freebsd && !netbsd && !openbsd

package capturerelay

import "golang.org/x/sys/unix"

type relayPoller struct {
	fds []unix.PollFd
}

func (p *relayPoller) reset(fds []int) error {
	p.fds = p.fds[:0]
	for _, fd := range fds {
		p.fds = append(p.fds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR})
	}
	return nil
}

func (p *relayPoller) wait(ready []bool) error {
	for i := range ready {
		ready[i] = false
	}
	if _, err := unix.Poll(p.fds, -1); err != nil {
		return err
	}
	for i := range ready {
		ready[i] = p.fds[i].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0
	}
	return nil
}

func (*relayPoller) close() {}
