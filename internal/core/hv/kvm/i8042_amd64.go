//go:build linux && amd64

package kvm

import "sync"

const (
	i8042DataPort   = 0x60
	i8042StatusPort = 0x64

	i8042StatusOutputFull = 0x01

	i8042CmdSelfTest     = 0xaa
	i8042CmdTestKeyboard = 0xab
)

type I8042 struct {
	mu    sync.Mutex
	queue []byte
}

func NewI8042() *I8042 {
	return &I8042{}
}

func (c *I8042) HandleIO(ioExit IOExit) (bool, error) {
	if ioExit.Port != i8042DataPort && ioExit.Port != i8042StatusPort {
		return false, nil
	}
	if ioExit.Size == 0 || ioExit.Count == 0 {
		return true, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := uint32(0); i < ioExit.Count; i++ {
		off := uint64(i) * uint64(ioExit.Size)
		if ioExit.Write {
			c.write(ioExit.Port, ioExit.Data[off:off+uint64(ioExit.Size)])
			continue
		}
		c.read(ioExit.Data[off:off+uint64(ioExit.Size)], ioExit.Port)
	}
	return true, nil
}

func (c *I8042) write(port uint16, data []byte) {
	if port != i8042StatusPort || len(data) == 0 {
		return
	}
	switch data[0] {
	case i8042CmdSelfTest:
		c.queue = append(c.queue, 0xfc)
	case i8042CmdTestKeyboard:
		c.queue = append(c.queue, 0x00)
	}
}

func (c *I8042) read(dst []byte, port uint16) {
	var value byte
	switch port {
	case i8042DataPort:
		if len(c.queue) != 0 {
			value = c.queue[0]
			copy(c.queue, c.queue[1:])
			c.queue = c.queue[:len(c.queue)-1]
		}
	case i8042StatusPort:
		if len(c.queue) != 0 {
			value = i8042StatusOutputFull
		}
	}
	for i := range dst {
		dst[i] = value
	}
}
