package virgl

import (
	"encoding/binary"
	"fmt"
)

const maxCommandDwords = 1 << 20

type command struct {
	Opcode  uint8
	Object  uint8
	Payload []uint32
}

func decodeCommands(stream []byte) ([]command, error) {
	if len(stream)&3 != 0 {
		return nil, fmt.Errorf("VirGL command stream length %d is not dword aligned", len(stream))
	}
	if len(stream)/4 > maxCommandDwords {
		return nil, fmt.Errorf("VirGL command stream has %d dwords, limit is %d", len(stream)/4, maxCommandDwords)
	}
	dwords := make([]uint32, len(stream)/4)
	for index := range dwords {
		dwords[index] = binary.LittleEndian.Uint32(stream[index*4:])
	}
	var commands []command
	for cursor := 0; cursor < len(dwords); {
		header := dwords[cursor]
		length := int(header >> 16)
		if length > len(dwords)-cursor-1 {
			return nil, fmt.Errorf("VirGL command %d payload has %d dwords, only %d remain",
				len(commands), length, len(dwords)-cursor-1)
		}
		payload := append([]uint32(nil), dwords[cursor+1:cursor+1+length]...)
		commands = append(commands, command{
			Opcode:  uint8(header),
			Object:  uint8(header >> 8),
			Payload: payload,
		})
		cursor += 1 + length
	}
	return commands, nil
}
