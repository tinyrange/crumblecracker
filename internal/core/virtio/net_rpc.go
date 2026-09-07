package virtio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sync"
)

const (
	NetPacketTX = "tx"
	NetPacketRX = "rx"
)

type NetPacket struct {
	Kind     string `json:"kind"`
	VMID     string `json:"vm_id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	Frame    []byte `json:"frame"`
}

type NetPacketCodec struct {
	conn io.ReadWriteCloser
	mu   sync.Mutex
}

func NewNetPacketCodec(conn io.ReadWriteCloser) *NetPacketCodec {
	return &NetPacketCodec{conn: conn}
}

func (c *NetPacketCodec) Send(packet NetPacket) error {
	payload, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	if len(payload) > math.MaxUint32 {
		return fmt.Errorf("net packet payload too large: %d", len(payload))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.conn.Write(header[:]); err != nil {
		return err
	}
	_, err = c.conn.Write(payload)
	return err
}

func (c *NetPacketCodec) Receive() (NetPacket, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return NetPacket{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return NetPacket{}, fmt.Errorf("empty net packet payload")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return NetPacket{}, err
	}
	var packet NetPacket
	if err := json.Unmarshal(payload, &packet); err != nil {
		return NetPacket{}, err
	}
	return packet, nil
}

func (c *NetPacketCodec) Close() error {
	return c.conn.Close()
}

type NetRemoteBackend struct {
	codec    *NetPacketCodec
	vmID     string
	deviceID string
}

func NewNetRemoteBackend(codec *NetPacketCodec, vmID, deviceID string) *NetRemoteBackend {
	return &NetRemoteBackend{codec: codec, vmID: vmID, deviceID: deviceID}
}

func (b *NetRemoteBackend) HandleTxPacket(packet []byte) error {
	if b == nil || b.codec == nil {
		return fmt.Errorf("net remote backend is not connected")
	}
	return b.codec.Send(NetPacket{
		Kind:     NetPacketTX,
		VMID:     b.vmID,
		DeviceID: b.deviceID,
		Frame:    append([]byte(nil), packet...),
	})
}

func ReceiveNetPackets(ctx context.Context, codec *NetPacketCodec, onPacket func(NetPacket) error) error {
	if codec == nil {
		return fmt.Errorf("net packet codec is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		packet, err := codec.Receive()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if onPacket != nil {
			if err := onPacket(packet); err != nil {
				return err
			}
		}
	}
}

func ReceiveNetRXPackets(ctx context.Context, codec *NetPacketCodec, dev *Net) error {
	return ReceiveNetPackets(ctx, codec, func(packet NetPacket) error {
		if packet.Kind != NetPacketRX {
			return nil
		}
		if dev == nil {
			return fmt.Errorf("virtio-net device is nil")
		}
		return dev.EnqueueRxPacketOwned(append([]byte(nil), packet.Frame...))
	})
}
